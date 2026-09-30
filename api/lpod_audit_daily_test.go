// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
)

const dailyAuditTestAPIKey = "daily-audit-test-admin-key"

func TestLPoDAuditDailyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name              string
		certificateHeight int
		reorg             bool
		prune             bool
		markerHeight      uint64
		wantReason        string
	}{
		{name: "missing exact row", certificateHeight: 4, wantReason: "exact block audit record missing"},
		{name: "certificate before closing witness", certificateHeight: 2, wantReason: "predates the closing witness"},
		{name: "same-height certificate reorg", certificateHeight: 4, reorg: true, wantReason: "not canonical"},
		{name: "pruned body", certificateHeight: 4, prune: true, wantReason: "metadata is unavailable"},
		{name: "partial first day", certificateHeight: 4, markerHeight: 2, wantReason: "starts after the interval opening"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db, hashes, path := dailyAuditFixture(t, tc.markerHeight)
			if tc.prune {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				fixtureDB, err := leveldb.OpenFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixtureDB.Put(append([]byte("b/"), hashes[2][:]...), []byte(`{"pruned":true}`), nil); err != nil {
					t.Fatal(err)
				}
				if err := fixtureDB.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				srv.SetStore(db)
			}
			certHash := hashes[tc.certificateHeight]
			if tc.reorg {
				certHash = crypto.HashBytes([]byte("different block at certificate height"))
			}
			if err := db.SaveFinalityCertificate(store.FinalityCertificate{
				Version: 1, Height: uint64(tc.certificateHeight), BlockHash: certHash,
				Votes: []store.FinalityVote{{Signature: []byte{1}}},
			}); err != nil {
				t.Fatalf("save fixture certificate: %v", err)
			}
			srv.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return true })
			code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
			if code != http.StatusOK || body["status"] != "incomplete" {
				t.Fatalf("fail-closed response: status=%d body=%#v", code, body)
			}
			if got, _ := body["reason"].(string); !containsText(got, tc.wantReason) {
				t.Fatalf("reason %q does not contain %q", got, tc.wantReason)
			}
		})
	}
}

func TestLPoDAuditDailyRequiresExactDate(t *testing.T) {
	srv, _, _, _ := dailyAuditFixture(t, 0)
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-1-2")
	if code != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("invalid date response: status=%d body=%#v", code, body)
	}
}

func TestLPoDAuditDailyRouteIsLoopbackAndKeyProtected(t *testing.T) {
	srv, _, _, _ := dailyAuditFixture(t, 0)
	for _, tc := range []struct {
		name       string
		host       string
		remoteAddr string
		apiKey     string
		want       int
	}{
		{name: "missing admin key", host: "127.0.0.1", remoteAddr: "127.0.0.1:12345", want: http.StatusUnauthorized},
		{name: "remote client", host: "audit.example", remoteAddr: "192.0.2.10:12345", apiKey: dailyAuditTestAPIKey, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/lpod/audit/daily?date=2024-01-02", nil)
			request.Host, request.RemoteAddr = tc.host, tc.remoteAddr
			if tc.apiKey != "" {
				request.Header.Set("X-API-Key", tc.apiKey)
			}
			srv.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("guard status %d, want %d: %s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

func TestLPoDAuditDailyCompletesWithCertifiedDescendant(t *testing.T) {
	srv, db, hashes, _ := dailyAuditCompleteFixture(t)
	certHash := hashes[14]
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		return height == 14 && hash == certHash
	})
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	coverage, _ := body["coverage"].(map[string]interface{})
	if code != http.StatusOK || body["status"] != "complete" || coverage["blocks"] != float64(2) {
		t.Fatalf("complete daily audit response: status=%d body=%#v", code, body)
	}
	if body["verification_level"] != "operator_attested" || !containsText(body["trust_assumption"].(string), "not an independent on-chain proof") {
		t.Fatalf("complete audit lacks explicit operator-attested trust disclosure: %#v", body)
	}
	if body["funding"] == nil || body["certificate"] == nil {
		t.Fatalf("funding and certificate identities are required: %#v", body)
	}
}

func TestLPoDAuditDailyFailsClosedOnRegistryEvidenceGaps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*store.LPoDBlockAudit)
		deleteNext bool
		wantReason string
	}{
		{
			name: "tampered post-operation snapshot",
			mutate: func(record *store.LPoDBlockAudit) {
				record.RegistryAfter.DynamicMinNAPR++
			},
			wantReason: "replayed post-operation registry differs",
		},
		{
			name: "missing opening baseline",
			mutate: func(record *store.LPoDBlockAudit) {
				record.RegistryBefore = nil
			},
			wantReason: "registry snapshot missing",
		},
		{name: "included block audit gap", deleteNext: true, wantReason: "exact block audit record missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db, hashes, path := dailyAuditCompleteFixture(t)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			fixtureDB, err := leveldb.OpenFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.deleteNext {
				if err := fixtureDB.Delete(append([]byte("lpod/block-audit/v3/"), hashes[12][:]...), nil); err != nil {
					t.Fatal(err)
				}
			} else {
				key := append([]byte("lpod/block-audit/v3/"), hashes[11][:]...)
				raw, err := fixtureDB.Get(key, nil)
				if err != nil {
					t.Fatal(err)
				}
				var record store.LPoDBlockAudit
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				tc.mutate(&record)
				updated, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixtureDB.Put(key, updated, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixtureDB.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			srv.SetStore(db)
			certHash := hashes[14]
			if err := db.SaveFinalityCertificate(store.FinalityCertificate{
				Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
			}); err != nil {
				t.Fatal(err)
			}
			srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
				return height == 14 && hash == certHash
			})
			code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
			if code != http.StatusOK || body["status"] != "incomplete" ||
				!containsText(body["reason"].(string), tc.wantReason) {
				t.Fatalf("registry evidence gap response: status=%d body=%#v", code, body)
			}
			if body["verification_level"] != "operator_attested" || body["trust_assumption"] == "" {
				t.Fatalf("incomplete response lacks trust disclosure: %#v", body)
			}
		})
	}
}

func TestLPoDAuditDailyAllowsCanonicalTipNewerThanCertificate(t *testing.T) {
	srv, db, hashes, _ := dailyAuditCompleteFixture(t)
	certHeight, certHash := uint64(13), hashes[13]
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: certHeight, BlockHash: certHash,
		Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		return height == certHeight && hash == certHash
	})
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	coverage, _ := body["coverage"].(map[string]interface{})
	if code != http.StatusOK || body["status"] != "complete" || coverage["blocks"] != float64(2) {
		t.Fatalf("complete audit with newer tip: status=%d body=%#v", code, body)
	}
}

func TestLPoDAuditDailyFailsClosedWhenTimestampProbeBodyIsPruned(t *testing.T) {
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	date, _ := time.Parse("2006-01-02", "2024-01-02")
	endDate := time.Date(date.Year(), date.Month(), date.Day(), 20, 0, 0, 0, location)
	startDate := date.AddDate(0, 0, -1)
	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 20, 0, 0, 0, location)
	end := endDate.UnixNano()
	timestamps := make([]int64, 15)
	for height := 0; height <= 10; height++ {
		timestamps[height] = start.UnixNano() - int64(11-height)*int64(time.Second)
	}
	timestamps[11] = start.UnixNano()
	timestamps[12] = end - 1
	timestamps[13] = end
	timestamps[14] = end + int64(time.Second)
	parent := crypto.Hash32{}
	hashes := make([]crypto.Hash32, len(timestamps))
	for height, timestamp := range timestamps {
		txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1)}
		block := &core.Block{Header: core.BlockHeader{
			Height: uint64(height), PrevHash: parent, Timestamp: timestamp, ValidatorPub: pub,
			MerkleRoot: core.MerkleRoot(txs),
		}, Txs: txs}
		if err := block.Header.Sign(priv); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(block.Hash(), uint64(height), raw, nil, crypto.Hash32{}); err != nil {
			t.Fatalf("commit fixture block %d: %v", height, err)
		}
		hashes[height] = block.Hash()
		parent = block.Hash()
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Put(append([]byte("b/"), hashes[7][:]...), []byte(`{"pruned":true}`), nil); err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, 8+32+32+8+32+32)
	binary.BigEndian.PutUint64(marker[:8], 11)
	copy(marker[8:40], hashes[11][:])
	copy(marker[40:72], hashes[10][:])
	if err := rawDB.Put([]byte("lpod/block-audit-start/v3"), marker, nil); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	certHash := hashes[14]
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	for height := 10; height < len(hashes); height++ {
		if raw, err := db.GetRawBlock(hashes[height]); err != nil || len(raw) == 0 {
			t.Fatalf("body at/after opening anchor height %d was not retained: %v", height, err)
		}
	}
	srv := api.NewServer(":0", nil, nil, nil, testLogger())
	srv.SetStore(db)
	srv.SetAPIKey(dailyAuditTestAPIKey)
	srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		return height == 14 && hash == certHash
	})
key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
if err != nil { t.Fatal(err) }
t.Cleanup(key.Destroy)
srv.SetValidatorKey(key)
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	if code != http.StatusOK || body["status"] != "incomplete" ||
		!containsText(body["reason"].(string), "signed-header metadata is unavailable") ||
		!containsText(body["reason"].(string), "height 7") {
		t.Fatalf("pruned timestamp probe must fail closed with limitation: status=%d body=%#v", code, body)
	}
}

func TestLPoDAuditDailyUsesStoredBlockHintForPrunedSearchProbe(t *testing.T) {
	srv, db, hashes, path := dailyAuditCompleteFixture(t)
	prunedRaw := make(map[int][]byte)
	for _, height := range []int{7, 9} {
		raw, err := db.GetRawBlock(hashes[height])
		if err != nil {
			t.Fatal(err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		prunedRaw[height], err = json.Marshal(store.StoredBlock{
			Height: uint64(height), PrevHash: block.Header.PrevHash, Hash: hashes[height],
			Timestamp: block.Header.Timestamp, Round: block.Header.Round, TxCount: len(block.Txs),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for height, raw := range prunedRaw {
		if err := rawDB.Put(append([]byte("b/"), hashes[height][:]...), raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetStore(db)
	var pruneCursor [8]byte
	binary.LittleEndian.PutUint64(pruneCursor[:], 7)
	if err := db.PutMeta("prune_cursor", pruneCursor[:]); err != nil {
		t.Fatal(err)
	}
	certHash := hashes[14]
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		return height == 14 && hash == certHash
	})
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	coverage, _ := body["coverage"].(map[string]interface{})
	if code != http.StatusOK || body["status"] != "complete" || coverage["blocks"] != float64(2) {
		t.Fatalf("unsigned StoredBlock probe hint should preserve full-body audit inside retention: status=%d body=%#v", code, body)
	}
}

func TestLPoDAuditDailyFailsClosedWhenPruneCursorPassedOpening(t *testing.T) {
	srv, db, hashes, _ := dailyAuditCompleteFixture(t)
	certHash := hashes[14]
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	var pruneCursor [8]byte
	binary.LittleEndian.PutUint64(pruneCursor[:], 11)
	if err := db.PutMeta("prune_cursor", pruneCursor[:]); err != nil {
		t.Fatal(err)
	}
	srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		return height == 14 && hash == certHash
	})
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	if code != http.StatusOK || body["status"] != "incomplete" ||
		!containsText(body["reason"].(string), "prune cursor has reached the opening interval") {
		t.Fatalf("prune cursor past anchor must fail closed: status=%d body=%#v", code, body)
	}
}

func TestLPoDAuditDailyRejectsMalformedOrOverflowingPruneCursor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cursor     []byte
		wantReason string
	}{
		{name: "malformed length", cursor: []byte{1, 2, 3}, wantReason: "prune cursor metadata is malformed"},
		{name: "overflow", cursor: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, wantReason: "prune cursor overflows searchable height"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db, hashes, _ := dailyAuditCompleteFixture(t)
			certHash := hashes[14]
			if err := db.SaveFinalityCertificate(store.FinalityCertificate{
				Version: 1, Height: 14, BlockHash: certHash, Votes: []store.FinalityVote{{Signature: []byte{1}}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.PutMeta("prune_cursor", tc.cursor); err != nil {
				t.Fatal(err)
			}
			srv.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
				return height == 14 && hash == certHash
			})
			code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
			if code != http.StatusOK || body["status"] != "incomplete" ||
				body["reason"] != tc.wantReason {
				t.Fatalf("invalid prune cursor must fail closed: status=%d body=%#v", code, body)
			}
		})
	}
}

func TestLPoDAuditDailyRequiresCoverageMarker(t *testing.T) {
	srv, db, hashes, path := dailyAuditFixture(t, 0)
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 4, BlockHash: hashes[4], Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixtureDB.Delete([]byte("lpod/block-audit-start/v3"), nil); err != nil {
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetStore(db)
	srv.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return true })
	code, body := dailyAuditGet(t, srv, "/api/v1/lpod/audit/daily?date=2024-01-02")
	if code != http.StatusOK || body["status"] != "incomplete" || body["reason"] != "LPoD audit coverage marker missing" {
		t.Fatalf("missing marker response: status=%d body=%#v", code, body)
	}
}

func dailyAuditFixture(t *testing.T, markerHeight uint64) (*api.Server, *store.DB, []crypto.Hash32, string) {
	t.Helper()
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	closeDate, _ := time.Parse("2006-01-02", "2024-01-02")
	end := time.Date(closeDate.Year(), closeDate.Month(), closeDate.Day(), 20, 0, 0, 0, location).UnixNano()
	startDate := closeDate.AddDate(0, 0, -1)
	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 20, 0, 0, 0, location).UnixNano()
	timestamps := []int64{start - 1, start, end - 1, end, end + 1}
	var parent crypto.Hash32
	hashes := make([]crypto.Hash32, len(timestamps))
	for i, timestamp := range timestamps {
		txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1)}
		block := &core.Block{Header: core.BlockHeader{
			Height: uint64(i), PrevHash: parent, Timestamp: timestamp, ValidatorPub: pub,
			MerkleRoot: core.MerkleRoot(txs),
		}, Txs: txs}
		if err := block.Header.Sign(priv); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(block.Hash(), uint64(i), raw, nil, crypto.Hash32{}); err != nil {
			t.Fatalf("commit fixture block %d: %v", i, err)
		}
		parent = block.Hash()
		hashes[i] = parent
	}
	if markerHeight == 0 {
		markerHeight = 1
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, 8+32+32+8+32+32)
	binary.BigEndian.PutUint64(marker[:8], markerHeight)
	copy(marker[8:40], hashes[markerHeight][:])
	if markerHeight > 0 {
		copy(marker[40:72], hashes[markerHeight-1][:])
	}
	if err := fixtureDB.Put([]byte("lpod/block-audit-start/v3"), marker, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := api.NewServer(":0", nil, nil, nil, testLogger())
	srv.SetStore(db)
	srv.SetAPIKey(dailyAuditTestAPIKey)
key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
if err != nil { t.Fatal(err) }
t.Cleanup(key.Destroy)
srv.SetValidatorKey(key)
	return srv, db, hashes, path
}

func dailyAuditCompleteFixture(t *testing.T) (*api.Server, *store.DB, []crypto.Hash32, string) {
	t.Helper()
	path := t.TempDir()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	walletKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	leader := crypto.AddressFromKeys(crypto.MainnetByte, walletKeys)
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	closeDate, _ := time.Parse("2006-01-02", "2024-01-02")
	end := time.Date(closeDate.Year(), closeDate.Month(), closeDate.Day(), 20, 0, 0, 0, location).UnixNano()
	startDate := closeDate.AddDate(0, 0, -1)
	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 20, 0, 0, 0, location).UnixNano()
	timestamps := make([]int64, 15)
	for height := 0; height <= 10; height++ {
		timestamps[height] = start - int64(11-height)*int64(time.Second)
	}
	timestamps[11], timestamps[12], timestamps[13], timestamps[14] =
		start, end-1, end, end+int64(time.Second)
	var parent crypto.Hash32
	blocks := make([]*core.Block, len(timestamps))
	checkpoints := make([]*store.LPoDCheckpoint, len(timestamps))
	hashes := make([]crypto.Hash32, len(timestamps))
	root := crypto.HashBytes([]byte("daily-audit-funding-root"))
	for i, timestamp := range timestamps {
		checkpoint := &store.LPoDCheckpoint{
			State:   lpod.State{FundingDebit: lpod.InitialNAPRO, Balance: lpod.InitialNAPRO, LastHeight: uint64(i)},
			Carries: map[string]lpod.Carry{}, Positions: map[string]store.LPoDPosition{},
		}
		if i > 0 {
			checkpoint.Allocation = &store.LPoDAllocation{
				Version: 1, PositionLifecycleVersion: 1, FundingHeight: 1,
				Genesis: hashes[0], ReconciliationRoot: root,
				InitialValidatorRemaining: 2_000_000_000 * lpod.Unit,
				ValidatorRemaining:        2_000_000_000 * lpod.Unit,
				Remaining:                 7_000_000_000*lpod.Unit - lpod.InitialNAPRO,
			}
		}
		txs := []core.Transaction{}
		if i > 0 {
			auth, _ := json.Marshal(core.LPoDPayoutAuthorization{Leader: leader, Digest: checkpoint.Digest()})
			txs = append(txs, core.Transaction{Version: core.TxVersionLPoDPayout, Extra: auth},
				core.LPoDCheckpointTx(checkpoint.Digest()))
		}
		block := &core.Block{Header: core.BlockHeader{
			Height: uint64(i), PrevHash: parent, Timestamp: timestamp, ValidatorPub: pub,
			MerkleRoot: core.MerkleRoot(txs),
		}, Txs: txs}
		if err := block.Header.Sign(priv); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(block.Hash(), uint64(i), raw, nil, crypto.Hash32{}); err != nil {
			t.Fatalf("commit complete fixture block %d: %v", i, err)
		}
		parent, hashes[i], blocks[i], checkpoints[i] = block.Hash(), block.Hash(), block, checkpoint
	}
	for i := range checkpoints {
		if checkpoints[i].Allocation == nil {
			continue
		}
		checkpoints[i].Allocation.FundingBlock = hashes[1]
		checkpoints[i].Allocation.Genesis = hashes[0]
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(key []byte, value interface{}) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixtureDB.Put(key, raw, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, checkpoint := range checkpoints {
		key := append([]byte("lpod/checkpoint/v1/"), hashes[i][:]...)
		put(key, checkpoint)
	}
	zero := uint64(0)
	for _, height := range []int{11, 12} {
		block, checkpoint := blocks[height], checkpoints[height]
		before := checkpoints[height-1]
		registry := &core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{}}
		record := store.LPoDBlockAudit{
			Version: 3, Available: true, Height: uint64(height), BlockHash: hashes[height],
			ParentHash: block.Header.PrevHash, HeaderTimestamp: block.Header.Timestamp,
			BeforeCheckpointDigest: before.Digest(), AfterCheckpointDigest: checkpoint.Digest(),
			FundingHeight: 1, FundingGenesis: hashes[0], FundingRoot: root,
			RegistryBefore: registry, RegistryAfter: registry,
			PreviousStake: map[string]store.LPoDValidatorStake{}, Stake: map[string]store.LPoDValidatorStake{},
			PayoutTransactionHash:    block.Txs[0].Hash(),
			ProtocolBaseFeeBurnNAPRO: &zero, SignedIntentionalBurnNAPRO: &zero,
			AVMGasBurnNAPRO: &zero, TotalBurnNAPRO: &zero,
		}
		key := append([]byte("lpod/block-audit/v3/"), hashes[height][:]...)
		put(key, record)
	}
	marker := make([]byte, 8+32+32+8+32+32)
	binary.BigEndian.PutUint64(marker[:8], 1)
	copy(marker[8:40], hashes[1][:])
	copy(marker[40:72], hashes[0][:])
	binary.BigEndian.PutUint64(marker[72:80], 1)
	copy(marker[80:112], hashes[0][:])
	copy(marker[112:144], root[:])
	if err := fixtureDB.Put([]byte("lpod/block-audit-start/v3"), marker, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixtureDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := api.NewServer(":0", nil, nil, nil, testLogger())
	srv.SetStore(db)
	srv.SetAPIKey(dailyAuditTestAPIKey)
key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
if err != nil { t.Fatal(err) }
t.Cleanup(key.Destroy)
srv.SetValidatorKey(key)
	return srv, db, hashes, path
}

func dailyAuditGet(t *testing.T, srv *api.Server, path string) (int, map[string]interface{}) {
	t.Helper()
srv.StartDailyAuditWorker()
t.Cleanup(srv.StopDailyAuditWorker)
deadline := time.Now().Add(30*time.Second)
for {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1"
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-API-Key", dailyAuditTestAPIKey)
	srv.ServeHTTP(recorder, request)
	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body.String())
	}
reason, _ := body["reason"].(string)
if reason == "queued for bounded background verification" || reason == "bounded background verification in progress" {
if time.Now().After(deadline) { t.Fatal("background audit did not finish") }
time.Sleep(20*time.Millisecond)
continue
}
	return recorder.Code, body
}
}

func containsText(value, fragment string) bool {
	return strings.Contains(value, fragment)
}
