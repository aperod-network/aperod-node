package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	b2TestAccessKeyEnv  = "BACKBLAZE_TEST_ACCESS_KEY_ID"
	b2TestSecretKeyEnv  = "BACKBLAZE_TEST_SECRET_ACCESS_KEY"
	b2TestBucketIDEnv   = "BACKBLAZE_TEST_BUCKET_ID"
	b2TestBucketNameEnv = "BACKBLAZE_TEST_BUCKET_NAME"
	// The operator explicitly authorized this configured, empty backup bucket
	// for the rehearsal while scheduled backups are disabled. Keep the exception
	// exact; do not allow arbitrary names.
	b2AuthorizedTestBucketName = "aperod-vault"
)

var b2UploadHostPattern = regexp.MustCompile(
	`^pod-[0-9]{3}-[0-9]{4}-[0-9]{2}\.backblaze\.com$`,
)

func TestB2TestBucketPinnedRestore(t *testing.T) {
	accessKey := os.Getenv(b2TestAccessKeyEnv)
	secretKey := os.Getenv(b2TestSecretKeyEnv)
	bucketID := os.Getenv(b2TestBucketIDEnv)
	bucketName := os.Getenv(b2TestBucketNameEnv)
	configured := []bool{
		accessKey != "", secretKey != "", bucketID != "", bucketName != "",
	}
	configuredCount := 0
	for _, present := range configured {
		if present {
			configuredCount++
		}
	}
	if configuredCount == 0 {
		t.Skip("dedicated B2 test-bucket configuration is not set")
	}
	if configuredCount != len(configured) {
		t.Fatal("incomplete dedicated B2 test-bucket configuration")
	}
	if err := validateB2TestBucketName(bucketName); err != nil {
		t.Fatal(err)
	}

	b2, err := authorizeB2TestBucket(accessKey, secretKey, bucketID, bucketName)
	if err != nil {
		t.Fatalf("authorize dedicated B2 test bucket: %v", err)
	}

	sandbox := t.TempDir()
	if sandbox == stagingRoot || strings.HasPrefix(sandbox, stagingRoot+string(os.PathSeparator)) {
		t.Fatalf("integration fixture unexpectedly uses production staging path: %s", sandbox)
	}
	const checkpointID = "checkpoint-1189583511"
	const fixtureIdentity = "Aperod isolated B2 restore rehearsal"
	height := uint64(42)
	hash := [32]byte{}
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	hashHex := fmt.Sprintf("%x", hash[:])

	sourceRoot := filepath.Join(sandbox, "source")
	sourceCheckpoint := filepath.Join(sourceRoot, checkpointID)
	sourceDB := filepath.Join(sourceCheckpoint, "chain.db")
	if err := os.MkdirAll(sourceCheckpoint, 0700); err != nil {
		t.Fatal(err)
	}
	createSyntheticCheckpointDB(t, sourceDB, height, hash)
	manifest, err := json.Marshal(map[string]any{
		"success": true, "tip_height": height, "tip_hash": hashHex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceCheckpoint, "manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	sourceFiles := syntheticFileInventory(t, sourceCheckpoint)

	archiveBytes := tarSyntheticCheckpoint(t, sourceCheckpoint)
	archiveKeyHome := filepath.Join(sandbox, "archive-key-home")
	if err := os.Mkdir(archiveKeyHome, 0700); err != nil {
		t.Fatal(err)
	}
	runFixtureGPG(t, archiveKeyHome, nil,
		"--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-generate-key", fixtureIdentity, "rsa2048", "encr", "0")
	fingerprint := fixtureGPGFingerprint(t, archiveKeyHome)
	ciphertextPath := filepath.Join(sandbox, "synthetic-checkpoint.tar.gpg")
	runFixtureGPG(t, archiveKeyHome, bytes.NewReader(archiveBytes),
		"--trust-model", "always", "--compress-algo", "none",
		"--output", ciphertextPath, "--recipient", fingerprint, "--encrypt")
	ciphertext, err := os.ReadFile(ciphertextPath)
	if err != nil {
		t.Fatal(err)
	}
	secretKeyCopy := runFixtureGPG(t, archiveKeyHome, nil, "--export-secret-keys", fingerprint)
	restoreKeyHome := filepath.Join(sandbox, "offhost-restore-key-home")
	if err := os.Mkdir(restoreKeyHome, 0700); err != nil {
		t.Fatal(err)
	}
	runFixtureGPG(t, restoreKeyHome, bytes.NewReader(secretKeyCopy), "--import")
	if err := os.RemoveAll(archiveKeyHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archiveKeyHome); !os.IsNotExist(err) {
		t.Fatalf("archive-side encryption key home remains available: %v", err)
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("create unique test object name: %v", err)
	}
	objectName := "aperod-test-rehearsal/" + hex.EncodeToString(nonce) + ".tar.gpg"
	if b2.namePrefix != "" && !strings.HasPrefix(objectName, b2.namePrefix) {
		t.Fatalf("test application key is restricted to a different object prefix")
	}
	if err := b2.requireNoLifecycleRule(objectName); err != nil {
		t.Fatalf("refuse test object covered by an automatic lifecycle rule: %v", err)
	}
	existingVersions, err := b2.listFileVersions(objectName)
	if err != nil {
		t.Fatalf("check unique B2 test object name: %v", err)
	}
	if len(existingVersions) != 0 {
		t.Fatal("unique B2 test object name unexpectedly already exists")
	}

	uploadAttempted := false
	uploadedID := ""
	t.Cleanup(func() {
		if !uploadAttempted {
			return
		}
		if err := b2.cleanupUniqueTestObject(objectName, uploadedID); err != nil {
			t.Errorf("delete disposable B2 test object %q: %v", objectName, err)
		}
	})
	uploadAttempted = true
	uploaded, err := b2.uploadFile(objectName, ciphertext)
	if err != nil {
		t.Fatalf("upload disposable synthetic checkpoint to test bucket: %v", err)
	}
	uploadedID = uploaded.FileID
	if uploaded.FileID == "" || uploaded.FileName != objectName {
		t.Fatalf("B2 returned an unexpected upload receipt")
	}

	info, err := b2.getFileInfo(uploaded.FileID)
	if err != nil {
		t.Fatalf("inspect uploaded B2 test version: %v", err)
	}
	if info.FileID != uploaded.FileID || info.FileName != objectName ||
		info.ContentLength != int64(len(ciphertext)) {
		t.Fatal("B2 file info does not match the exact uploaded test version")
	}
	versions, err := b2.listFileVersions(objectName)
	if err != nil {
		t.Fatalf("list exact B2 test object versions: %v", err)
	}
	if len(versions) != 1 || versions[0].FileID != uploaded.FileID ||
		versions[0].FileName != objectName || versions[0].Action != "upload" {
		t.Fatalf("B2 version list does not contain exactly the recorded upload ID")
	}

	readback, err := b2.downloadFileByID(uploaded.FileID)
	if err != nil {
		t.Fatalf("download exact B2 file ID %q: %v", uploaded.FileID, err)
	}
	expectedDigest := sha256.Sum256(ciphertext)
	actualDigest := sha256.Sum256(readback)
	if !bytes.Equal(actualDigest[:], expectedDigest[:]) || len(readback) != len(ciphertext) {
		t.Fatal("downloaded ciphertext digest or size differs from the uploaded test object")
	}

	restoredTar := runFixtureGPG(t, restoreKeyHome, bytes.NewReader(readback), "--decrypt")
	restoreParent := filepath.Join(sandbox, "fresh-restore")
	if err := os.Mkdir(restoreParent, 0700); err != nil {
		t.Fatal(err)
	}
	restoreRoot := filepath.Join(restoreParent, checkpointID)
	if err := extractSyntheticTar(restoredTar, restoreRoot); err != nil {
		t.Fatalf("extract downloaded B2 archive: %v", err)
	}

	restoredManifest, err := os.ReadFile(filepath.Join(restoreRoot, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restoredMetadata struct {
		Success   bool   `json:"success"`
		TipHeight uint64 `json:"tip_height"`
		TipHash   string `json:"tip_hash"`
	}
	if err := json.Unmarshal(restoredManifest, &restoredMetadata); err != nil {
		t.Fatal(err)
	}
	if !restoredMetadata.Success || restoredMetadata.TipHeight != height ||
		restoredMetadata.TipHash != hashHex {
		t.Fatalf("restored B2 manifest does not match the synthetic tip: %+v", restoredMetadata)
	}
	restoredFiles := syntheticFileInventory(t, restoreRoot)
	if !reflect.DeepEqual(restoredFiles, sourceFiles) {
		t.Fatalf("restored B2 member inventory differs from source:\nsource:   %v\nrestored: %v",
			sourceFiles, restoredFiles)
	}
	report, err := inspectCheckpoint(
		filepath.Join(restoreRoot, "chain.db"), restoreParent, uint32(os.Getuid()),
	)
	if err != nil {
		t.Fatalf("closed-staging tip verification after B2 restore: %v", err)
	}
	if report.Height != height || report.Hash != hashHex || !report.HeightIndexMatches {
		t.Fatalf("restored B2 database tip differs from source: %+v", report)
	}
}

type b2TestClient struct {
	httpClient    *http.Client
	accountID     string
	authorization string
	apiURL        string
	downloadURL   string
	bucketID      string
	bucketName    string
	namePrefix    string
}

type b2TestFileVersion struct {
	FileID   string `json:"fileId"`
	FileName string `json:"fileName"`
	Action   string `json:"action"`
}

type b2TestFileInfo struct {
	FileID        string `json:"fileId"`
	FileName      string `json:"fileName"`
	ContentLength int64  `json:"contentLength"`
}

func authorizeB2TestBucket(accessKey, secretKey, bucketID, bucketName string) (*b2TestClient, error) {
	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	credentials := base64.StdEncoding.EncodeToString([]byte(accessKey + ":" + secretKey))
	request, err := http.NewRequest(http.MethodGet,
		"https://api.backblazeb2.com/b2api/v3/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Basic "+credentials)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("B2 authorization request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("B2 authorization returned HTTP %d", response.StatusCode)
	}
	var auth struct {
		AccountID          string `json:"accountId"`
		AuthorizationToken string `json:"authorizationToken"`
		APIInfo            struct {
			StorageAPI struct {
				APIURL       string   `json:"apiUrl"`
				DownloadURL  string   `json:"downloadUrl"`
				BucketID     string   `json:"bucketId"`
				NamePrefix   string   `json:"namePrefix"`
				Capabilities []string `json:"capabilities"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&auth); err != nil {
		return nil, fmt.Errorf("decode B2 authorization response: %w", err)
	}
	storage := auth.APIInfo.StorageAPI
	if auth.AccountID == "" || auth.AuthorizationToken == "" ||
		storage.APIURL == "" || storage.DownloadURL == "" {
		return nil, fmt.Errorf("B2 authorization response is incomplete")
	}
	if storage.BucketID == "" {
		return nil, fmt.Errorf("B2 test key is not restricted to a single bucket")
	}
	for _, capability := range []string{"listBuckets", "listFiles", "readFiles", "writeFiles", "deleteFiles"} {
		if !containsB2Capability(storage.Capabilities, capability) {
			return nil, fmt.Errorf("B2 test key lacks required %s capability", capability)
		}
	}
	if err := validateB2TestURL(storage.APIURL, false); err != nil {
		return nil, fmt.Errorf("unsafe B2 API URL: %w", err)
	}
	if err := validateB2TestURL(storage.DownloadURL, false); err != nil {
		return nil, fmt.Errorf("unsafe B2 download URL: %w", err)
	}
	b2 := &b2TestClient{
		httpClient: client, accountID: auth.AccountID,
		authorization: auth.AuthorizationToken, apiURL: storage.APIURL,
		downloadURL: storage.DownloadURL, bucketID: storage.BucketID,
		bucketName: bucketName, namePrefix: storage.NamePrefix,
	}
	if err := b2.requirePrivateTestBucket(); err != nil {
		return nil, fmt.Errorf("B2 test key does not select the expected private bucket: %w", err)
	}
	if storage.BucketID != bucketID {
		return nil, fmt.Errorf("configured B2 test bucket ID does not match the restricted key's bucket; update the test bucket ID")
	}
	return b2, nil
}

func containsB2Capability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func validateB2TestBucketName(bucketName string) error {
	if strings.HasPrefix(bucketName, "aperod-test-") || bucketName == b2AuthorizedTestBucketName {
		return nil
	}
	return fmt.Errorf("refusing B2 integration test unless bucket name starts with aperod-test- or is the explicitly authorized bucket")
}

func validateB2TestURL(raw string, uploadURL bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || parsed.User != nil ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.Fragment != "" {
		return fmt.Errorf("URL must use HTTPS without credentials, a custom port, or a fragment")
	}
	if uploadURL {
		if !b2UploadHostPattern.MatchString(host) {
			return fmt.Errorf("unexpected B2 upload host")
		}
		return nil
	}
	if host != "api.backblazeb2.com" && !strings.HasSuffix(host, ".backblazeb2.com") {
		return fmt.Errorf("unexpected B2 API/download host")
	}
	return nil
}

func TestB2TestConfigurationRejectsProductionTargets(t *testing.T) {
	if err := validateB2TestBucketName("aperod-test-checkpoint-restore"); err != nil {
		t.Fatalf("dedicated test bucket name rejected: %v", err)
	}
	if err := validateB2TestBucketName(b2AuthorizedTestBucketName); err != nil {
		t.Fatalf("explicitly authorized test bucket name rejected: %v", err)
	}
	for _, name := range []string{"", "aperod-production", "aperod-backup-prod", "aperodapro", "aperod-vault-prod"} {
		if err := validateB2TestBucketName(name); err == nil {
			t.Errorf("non-test bucket name accepted: %q", name)
		}
	}

	validURLs := []struct {
		raw    string
		upload bool
	}{
		{"https://api.backblazeb2.com", false},
		{"https://f000.backblazeb2.com", false},
		{"https://pod-050-1050-19.backblaze.com/upload", true},
	}
	for _, test := range validURLs {
		if err := validateB2TestURL(test.raw, test.upload); err != nil {
			t.Errorf("valid provider URL rejected %q: %v", test.raw, err)
		}
	}
	invalidURLs := []struct {
		raw    string
		upload bool
	}{
		{"http://api.backblazeb2.com", false},
		{"https://api.backblazeb2.com.attacker.invalid", false},
		{"https://user@api.backblazeb2.com", false},
		{"https://api.backblazeb2.com:444", false},
		{"https://api.backblazeb2.com/#fragment", false},
		{"https://pod-050-1050-19.backblaze.com.attacker.invalid/upload", true},
	}
	for _, test := range invalidURLs {
		if err := validateB2TestURL(test.raw, test.upload); err == nil {
			t.Errorf("unsafe provider URL accepted: %q", test.raw)
		}
	}
}

func (b2 *b2TestClient) request(method, rawURL string, body io.Reader, headers http.Header,
	uploadURL bool) (*http.Response, error) {
	if err := validateB2TestURL(rawURL, uploadURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := b2.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("B2 request failed: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("B2 request returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (b2 *b2TestClient) call(method string, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	requestURL := strings.TrimRight(b2.apiURL, "/") + "/b2api/v3/" + method
	headers := make(http.Header)
	headers.Set("Authorization", b2.authorization)
	headers.Set("Content-Type", "application/json")
	response, err := b2.request(http.MethodPost, requestURL, bytes.NewReader(body), headers, false)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode B2 %s response: %w", method, err)
	}
	return nil
}

func (b2 *b2TestClient) requirePrivateTestBucket() error {
	var result struct {
		Buckets []struct {
			BucketID       string `json:"bucketId"`
			BucketName     string `json:"bucketName"`
			BucketType     string `json:"bucketType"`
			LifecycleRules []struct {
				FileNamePrefix string `json:"fileNamePrefix"`
			} `json:"lifecycleRules"`
		} `json:"buckets"`
	}
	if err := b2.call("b2_list_buckets", map[string]string{
		"accountId": b2.accountID, "bucketId": b2.bucketID,
	}, &result); err != nil {
		return fmt.Errorf("list restricted B2 test bucket: %w", err)
	}
	for _, bucket := range result.Buckets {
		if bucket.BucketID != b2.bucketID {
			continue
		}
		if bucket.BucketName != b2.bucketName || bucket.BucketType != "allPrivate" {
			return fmt.Errorf("configured B2 bucket is not the expected private test bucket")
		}
		return nil
	}
	return fmt.Errorf("configured B2 test bucket ID was not returned")
}

func (b2 *b2TestClient) requireNoLifecycleRule(objectName string) error {
	var result struct {
		Buckets []struct {
			BucketID       string `json:"bucketId"`
			LifecycleRules []struct {
				FileNamePrefix string `json:"fileNamePrefix"`
			} `json:"lifecycleRules"`
		} `json:"buckets"`
	}
	if err := b2.call("b2_list_buckets", map[string]string{
		"accountId": b2.accountID, "bucketId": b2.bucketID,
	}, &result); err != nil {
		return err
	}
	for _, bucket := range result.Buckets {
		if bucket.BucketID != b2.bucketID {
			continue
		}
		for _, rule := range bucket.LifecycleRules {
			if strings.HasPrefix(objectName, rule.FileNamePrefix) {
				return fmt.Errorf("lifecycle rule for prefix %q applies", rule.FileNamePrefix)
			}
		}
		return nil
	}
	return fmt.Errorf("configured B2 test bucket ID was not returned")
}

func (b2 *b2TestClient) listFileVersions(objectName string) ([]b2TestFileVersion, error) {
	var result struct {
		Files        []b2TestFileVersion `json:"files"`
		NextFileName string              `json:"nextFileName"`
		NextFileID   string              `json:"nextFileId"`
	}
	if err := b2.call("b2_list_file_versions", map[string]any{
		"bucketId": b2.bucketID, "prefix": objectName, "maxFileCount": 100,
	}, &result); err != nil {
		return nil, err
	}
	if result.NextFileName != "" || result.NextFileID != "" {
		return nil, fmt.Errorf("B2 file-version listing was incomplete")
	}
	exact := make([]b2TestFileVersion, 0, len(result.Files))
	for _, version := range result.Files {
		if version.FileName == objectName {
			exact = append(exact, version)
		}
	}
	return exact, nil
}

func (b2 *b2TestClient) getFileInfo(fileID string) (b2TestFileInfo, error) {
	var result b2TestFileInfo
	err := b2.call("b2_get_file_info", map[string]string{"fileId": fileID}, &result)
	return result, err
}

func (b2 *b2TestClient) uploadFile(objectName string, data []byte) (b2TestFileVersion, error) {
	var upload struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := b2.call("b2_get_upload_url", map[string]string{"bucketId": b2.bucketID}, &upload); err != nil {
		return b2TestFileVersion{}, fmt.Errorf("get B2 upload URL: %w", err)
	}
	if upload.UploadURL == "" || upload.AuthorizationToken == "" {
		return b2TestFileVersion{}, fmt.Errorf("B2 returned an incomplete upload URL")
	}
	sha := sha1.Sum(data)
	headers := make(http.Header)
	headers.Set("Authorization", upload.AuthorizationToken)
	headers.Set("X-Bz-File-Name", url.QueryEscape(objectName))
	headers.Set("X-Bz-Content-Sha1", hex.EncodeToString(sha[:]))
	headers.Set("Content-Type", "application/octet-stream")
	response, err := b2.request(
		http.MethodPost, upload.UploadURL, bytes.NewReader(data), headers, true,
	)
	if err != nil {
		return b2TestFileVersion{}, err
	}
	defer response.Body.Close()
	var uploaded b2TestFileVersion
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&uploaded); err != nil {
		return b2TestFileVersion{}, fmt.Errorf("decode B2 upload receipt: %w", err)
	}
	return uploaded, nil
}

func (b2 *b2TestClient) downloadFileByID(fileID string) ([]byte, error) {
	downloadURL := strings.TrimRight(b2.downloadURL, "/") +
		"/b2api/v3/b2_download_file_by_id?fileId=" + url.QueryEscape(fileID)
	headers := make(http.Header)
	headers.Set("Authorization", b2.authorization)
	response, err := b2.request(http.MethodGet, downloadURL, nil, headers, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read downloaded B2 ciphertext: %w", err)
	}
	if len(data) > 64<<20 {
		return nil, fmt.Errorf("synthetic B2 test archive exceeded the 64 MiB limit")
	}
	return data, nil
}

func (b2 *b2TestClient) deleteExactVersion(fileID, objectName string) error {
	var deleted b2TestFileVersion
	if err := b2.call("b2_delete_file_version", map[string]string{
		"fileId": fileID, "fileName": objectName,
	}, &deleted); err != nil {
		return err
	}
	if deleted.FileID != fileID || deleted.FileName != objectName {
		return fmt.Errorf("B2 delete response did not match the exact test version")
	}
	versions, err := b2.listFileVersions(objectName)
	if err != nil {
		return fmt.Errorf("verify exact test version cleanup: %w", err)
	}
	for _, version := range versions {
		if version.FileID == fileID {
			return fmt.Errorf("B2 test file version remains after deletion")
		}
	}
	return nil
}

func (b2 *b2TestClient) cleanupUniqueTestObject(objectName, expectedID string) error {
	versions, err := b2.listFileVersions(objectName)
	if err != nil {
		return fmt.Errorf("list disposable test object versions: %w", err)
	}
	if len(versions) == 0 {
		return nil
	}
	if expectedID != "" {
		if len(versions) != 1 || versions[0].FileID != expectedID {
			return fmt.Errorf("B2 test object versions no longer match the recorded ID")
		}
		return b2.deleteExactVersion(expectedID, objectName)
	}
	if len(versions) != 1 || versions[0].FileID == "" ||
		versions[0].FileName != objectName || versions[0].Action != "upload" {
		return fmt.Errorf("cannot safely identify the version created by this test")
	}
	return b2.deleteExactVersion(versions[0].FileID, objectName)
}
