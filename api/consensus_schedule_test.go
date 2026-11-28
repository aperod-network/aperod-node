package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
)

const consensusSchedulePath = "/api/v1/admin/consensus-schedule"

func newConsensusScheduleServer(t *testing.T, apiKey string) *api.Server {
	t.Helper()
	srv := api.NewServer(
		":0",
		core.NewChain(),
		core.NewMempool(core.DefaultMempoolConfig()),
		core.NewUTXOSet(),
		testLogger(),
	)
	if apiKey != "" {
		srv.SetAPIKey(apiKey)
	}
	return srv
}

func requestConsensusSchedule(
	srv *api.Server,
	remoteAddr string,
	host string,
	apiKey string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, consensusSchedulePath, nil)
	req.RemoteAddr = remoteAddr
	req.Host = host
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, req)
	return recorder
}

func TestConsensusScheduleIsReadOnly(t *testing.T) {
	const apiKey = "schedule-test-key"
	srv := newConsensusScheduleServer(t, apiKey)

	req := httptest.NewRequest(http.MethodHead, consensusSchedulePath, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:8545"
	req.Header.Set("X-API-Key", apiKey)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestConsensusScheduleRequiresLoopbackAndAPIKey(t *testing.T) {
	const apiKey = "schedule-test-key"
	srv := newConsensusScheduleServer(t, apiKey)
	srv.SetConsensusActivationSchedule(10, 20, 30, "")

	tests := []struct {
		name       string
		remoteAddr string
		host       string
		apiKey     string
		wantStatus int
	}{
		{
			name:       "missing key",
			remoteAddr: "127.0.0.1:12345",
			host:       "127.0.0.1:8545",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong key",
			remoteAddr: "127.0.0.1:12345",
			host:       "127.0.0.1:8545",
			apiKey:     "wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "remote peer with valid key",
			remoteAddr: "192.0.2.10:12345",
			host:       "127.0.0.1:8545",
			apiKey:     apiKey,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "spoofed host with valid key",
			remoteAddr: "127.0.0.1:12345",
			host:       "operator.example",
			apiKey:     apiKey,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "authorized loopback",
			remoteAddr: "127.0.0.1:12345",
			host:       "127.0.0.1:8545",
			apiKey:     apiKey,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := requestConsensusSchedule(srv, tc.remoteAddr, tc.host, tc.apiKey)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
		})
	}

	unconfigured := newConsensusScheduleServer(t, "")
	recorder := requestConsensusSchedule(unconfigured, "127.0.0.1:12345", "127.0.0.1:8545", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("endpoint without configured API key returned %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestConsensusScheduleReportsZeroDefaults(t *testing.T) {
	srv := newConsensusScheduleServer(t, "schedule-test-key")
	recorder := requestConsensusSchedule(srv, "127.0.0.1:12345", "127.0.0.1:8545", "schedule-test-key")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response struct {
		GuardianFundActivationHeight    uint64  `json:"guardian_fund_activation_height"`
		LPoDDailyUpgradeHeight          uint64  `json:"lpod_daily_upgrade_height"`
		LPoDValidatorDailyUpgradeHeight uint64  `json:"lpod_validator_daily_upgrade_height"`
		MigrationWitnessStatus          string  `json:"migration_witness_status"`
		MigrationWitnessSHA256          *string `json:"migration_witness_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.GuardianFundActivationHeight != 0 ||
		response.LPoDDailyUpgradeHeight != 0 ||
		response.LPoDValidatorDailyUpgradeHeight != 0 {
		t.Fatalf("unexpected default heights: %+v", response)
	}
	if response.MigrationWitnessStatus != "not_configured" || response.MigrationWitnessSHA256 != nil {
		t.Fatalf("unexpected default witness status: %+v", response)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode response fields: %v", err)
	}
	allowedFields := map[string]struct{}{
		"guardian_fund_activation_height":     {},
		"lpod_daily_upgrade_height":           {},
		"lpod_validator_daily_upgrade_height": {},
		"migration_witness_status":            {},
		"migration_witness_sha256":            {},
	}
	if len(fields) != len(allowedFields) {
		t.Fatalf("response exposes unexpected fields: %v", fields)
	}
	for field := range fields {
		if _, allowed := allowedFields[field]; !allowed {
			t.Fatalf("response exposes unexpected field %q", field)
		}
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestConsensusScheduleReportsMigrationWitnessDigestWithoutPath(t *testing.T) {
	const apiKey = "schedule-test-key"
	witnessPath := filepath.Join(t.TempDir(), "migration-witness.json")
	witness := []byte(`{"version":1,"anchor_height":42}`)
	if err := os.WriteFile(witnessPath, witness, 0600); err != nil {
		t.Fatal(err)
	}

	srv := newConsensusScheduleServer(t, apiKey)
	srv.SetConsensusActivationSchedule(11, 22, 33, witnessPath)
	recorder := requestConsensusSchedule(srv, "127.0.0.1:12345", "127.0.0.1:8545", apiKey)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response struct {
		GuardianFundActivationHeight    uint64  `json:"guardian_fund_activation_height"`
		LPoDDailyUpgradeHeight          uint64  `json:"lpod_daily_upgrade_height"`
		LPoDValidatorDailyUpgradeHeight uint64  `json:"lpod_validator_daily_upgrade_height"`
		MigrationWitnessStatus          string  `json:"migration_witness_status"`
		MigrationWitnessSHA256          *string `json:"migration_witness_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantDigestBytes := sha256.Sum256(witness)
	wantDigest := hex.EncodeToString(wantDigestBytes[:])
	if response.GuardianFundActivationHeight != 11 ||
		response.LPoDDailyUpgradeHeight != 22 ||
		response.LPoDValidatorDailyUpgradeHeight != 33 {
		t.Fatalf("unexpected schedule heights: %+v", response)
	}
	if response.MigrationWitnessStatus != "available" ||
		response.MigrationWitnessSHA256 == nil ||
		*response.MigrationWitnessSHA256 != wantDigest {
		t.Fatalf("unexpected witness digest: %+v, want %s", response, wantDigest)
	}
	if strings.Contains(recorder.Body.String(), witnessPath) {
		t.Fatal("response must not expose the migration witness path")
	}
}

func TestConsensusScheduleMarksUnreadableWitnessUnavailable(t *testing.T) {
	const apiKey = "schedule-test-key"
	witnessPath := filepath.Join(t.TempDir(), "missing-witness.json")
	srv := newConsensusScheduleServer(t, apiKey)
	srv.SetConsensusActivationSchedule(0, 0, 0, witnessPath)

	recorder := requestConsensusSchedule(srv, "127.0.0.1:12345", "127.0.0.1:8545", apiKey)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		MigrationWitnessStatus string  `json:"migration_witness_status"`
		MigrationWitnessSHA256 *string `json:"migration_witness_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.MigrationWitnessStatus != "unavailable" || response.MigrationWitnessSHA256 != nil {
		t.Fatalf("unexpected unavailable witness response: %+v", response)
	}
	if strings.Contains(recorder.Body.String(), witnessPath) {
		t.Fatal("response must not expose the configured path")
	}
}
