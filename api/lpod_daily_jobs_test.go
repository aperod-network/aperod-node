package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
	"github.com/syndtr/goleveldb/leveldb"
)

func enqueueDailyTest(t *testing.T, s *Server) *dailyJob {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, authenticatedDailyAuditRequest())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "queued") {
		t.Fatalf("enqueue: %d %s", rec.Code, rec.Body.String())
	}
	j, err := s.loadDailyJob()
	if err != nil || j == nil || j.Progress.Next != 0 {
		t.Fatalf("enqueue did scan work: %+v %v", j, err)
	}
	return j
}

func TestDailyJobRestartBudgetAndQueue(t *testing.T) {
	db, s, path, _ := makeDailyAuditBenchmarkFixture(t, 70, 1, 128, 4, 0, 8)
	enqueueDailyTest(t, s)
	other := authenticatedDailyAuditRequest()
	other.URL.RawQuery = "date=2024-01-03"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, other)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("queue must reject second date: %d", rec.Code)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s.runDailyJobBatch(cancelled)
	j, _ := s.loadDailyJob()
	if j.Progress.Next != 0 {
		t.Fatal("cancelled batch changed progress")
	}
	s.runDailyJobBatch(context.Background())
	j, err := s.loadDailyJob()
	if err != nil || j.State != "queued" || j.Progress.Next == 0 || j.Progress.Next > dailyBatchBlocks {
		t.Fatalf("batch did not checkpoint bounded progress: %+v %v", j, err)
	}
	savedNext := j.Progress.Next
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := NewServer(":0", nil, nil, nil, slog.Default())
	restarted.SetStore(db)
reloadedKey, err := crypto.NewLockedValidatorKey(s.myKey.PrivKey().Bytes(), nil)
if err != nil { t.Fatal(err) }
defer reloadedKey.Destroy()
restarted.SetValidatorKey(reloadedKey)
	restarted.SetAPIKey(dailyAuditBenchmarkAPIKey)
	restarted.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return true })
	j, err = restarted.loadDailyJob()
	if err != nil || j.Progress.Next != savedNext {
		t.Fatal("durable progress lost on reopen")
	}
	for n := 0; n < 80 && j.State == "queued"; n++ {
		before := j.Progress.Next
		restarted.runDailyJobBatch(context.Background())
		j, err = restarted.loadDailyJob()
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "queued" && (j.Progress.Next <= before || j.Progress.Next-before > dailyBatchBlocks) {
			t.Fatal("batch failed bounded forward progress")
		}
	}
	if j.State != "complete" {
		t.Fatalf("resume failed: %s", j.Result)
	}
	// Compare against the unmodified full verifier, including positive APR,
	// payout reconstruction, burns, registry replay and decimal totals.
	baseline := httptest.NewRecorder()
	restarted.restLPoDAuditDaily(baseline, authenticatedDailyAuditRequest())
	var got, want map[string]interface{}
	json.Unmarshal(j.Result, &got)
	json.Unmarshal(baseline.Body.Bytes(), &want)
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("resumed report differs from full verification:\n%s\n%s", gotBytes, wantBytes)
	}
}

func TestDailyJobReorgAndFinalityFailClosed(t *testing.T) {
	for _, change := range []string{"predecessor", "certificate", "finality", "prune"} {
		t.Run(change, func(t *testing.T) {
			db, s, path, _ := makeDailyAuditBenchmarkFixture(t, 40, 0, 0, 1, 0, 1)
			enqueueDailyTest(t, s)
			s.runDailyJobBatch(context.Background())
			j, err := s.loadDailyJob()
			if err != nil || j.Progress.Next == 0 {
				t.Fatal("missing first batch")
			}
			switch change {
			case "predecessor", "certificate":
				height := j.Progress.Next - 1
				if change == "certificate" {
					height = j.Progress.Certificate.Height
				}
				db.Close()
				raw, err := leveldb.OpenFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				key := make([]byte, 10)
				copy(key, []byte("h/"))
				binary.BigEndian.PutUint64(key[2:], height)
				hash := crypto.HashBytes([]byte("reorg"))
				if err := raw.Put(key, hash[:], nil); err != nil {
					t.Fatal(err)
				}
				raw.Close()
				db, err = store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				s.SetStore(db)
			case "finality":
				s.SetLPoDConfig(nil, func(uint64, crypto.Hash32) bool { return false })
			case "prune":
				var cursor [8]byte
				binary.LittleEndian.PutUint64(cursor[:], 1)
				if err := db.PutMeta("prune_cursor", cursor[:]); err != nil {
					t.Fatal(err)
				}
			}
			defer db.Close()
			s.runDailyJobBatch(context.Background())
			j, err = s.loadDailyJob()
			if err != nil || j.State != "failed" || strings.Contains(string(j.Result), `"status":"complete"`) {
				t.Fatalf("%s accepted invalidated progress: %+v %v", change, j, err)
			}
		})
	}
}

func TestDailyJobCancellationAndShutdown(t *testing.T) {
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, 40, 0, 0, 1, 0, 0)
	defer db.Close()
	enqueueDailyTest(t, s)
	s.runDailyJobBatch(context.Background())
	req := authenticatedDailyAuditRequest()
	req.Method = http.MethodDelete
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	j, err := s.loadDailyJob()
	if rec.Code != http.StatusOK || err != nil || j != nil {
		t.Fatal("DELETE did not durably cancel")
	}
	enqueueDailyTest(t, s)
	s.StartDailyAuditWorker()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.dailyJobs.mu.Lock()
		j, err = s.loadDailyJob()
		s.dailyJobs.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if j.Progress.Next > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lifecycle worker did not recover persisted queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.StopDailyAuditWorker()
	s.StopDailyAuditWorker()
	j, err = s.loadDailyJob()
	if err != nil || j == nil || j.State != "queued" {
		t.Fatal("shutdown discarded resumable work")
	}
}

func TestDailyJobStorageBudgetAndRetry(t *testing.T) {
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, 2, 0, 0, 1, 0, 0)
	defer db.Close()
	j := enqueueDailyTest(t, s)
	oversize := *j
	oversize.Result = json.RawMessage(`"` + strings.Repeat("x", dailyJobMaxBytes) + `"`)
	if err := s.saveDailyJob(&oversize); err == nil {
		t.Fatal("accepted oversized job")
	}
	j.State, j.Progress = "failed", nil
	j.Result, _ = json.Marshal(dailyPending(j.Date, "closing witness not yet present"))
	j.Updated = time.Now().Add(-time.Minute).Unix()
raw, err := s.encodeDailyJob(j)
if err != nil { t.Fatal(err) }
	if err := db.PutMetaSync(dailyJobKey, raw); err != nil {
		t.Fatal(err)
	}
	enqueueDailyTest(t, s)
}

// Production retains only 256 finalized heights in memory and restart may
// retain only the latest one. Require forward progress when EVERY previously
// saved certificate disappears from the oracle, not merely after 256 heights.
func TestDailyJobAdvancingCertificateOracleDoesNotResetCursor(t *testing.T) {
db, s, _, _ := makeDailyAuditBenchmarkFixtureWithTail(t, 512, 1, 128, 4, 0, 4, 512)
defer db.Close()
certificate, err := db.LoadFinalityCertificate()
if err != nil { t.Fatal(err) }
initialHeight := certificate.Height
_, tipHeight, err := db.GetTip()
if err != nil { t.Fatal(err) }
var oracleHeight uint64
var oracleHash crypto.Hash32
oracle := func(height uint64, hash crypto.Hash32) bool {
return height == oracleHeight && hash == oracleHash
}
enqueueDailyTest(t, s)
var priorNext uint64
for batch := 0; batch < 100; batch++ {
height := initialHeight + uint64(batch)*32
if height > tipHeight { height = tipHeight }
hash, found, err := db.GetCanonicalHash(height)
if err != nil || !found { t.Fatal("missing tail certificate hash") }
if err := db.SaveFinalityCertificate(store.FinalityCertificate{
Version: 1, Height: height, BlockHash: hash,
Votes: []store.FinalityVote{{Signature: []byte{1}}},
}); err != nil { t.Fatal(err) }
oracleHeight, oracleHash = height, hash
if batch == 2 {
// Simulate loss of all server-local state while the durable job remains.
fresh := NewServer(":0", nil, nil, nil, slog.Default())
fresh.SetStore(db)
fresh.SetValidatorKey(s.myKey)
fresh.SetAPIKey(dailyAuditBenchmarkAPIKey)
s = fresh
}
s.SetLPoDConfig(nil, oracle)
s.runDailyJobBatch(context.Background())
j, err := s.loadDailyJob()
if err != nil || j == nil || j.State == "failed" { t.Fatalf("batch %d failed: %+v %v", batch, j, err) }
if j.Progress.Next <= priorNext || j.Progress.Next-priorNext > dailyBatchBlocks {
t.Fatalf("certificate advancement reset/stalled/unbounded cursor: batch=%d before=%d after=%d", batch, priorNext, j.Progress.Next)
}
priorNext = j.Progress.Next
if j.State == "complete" {
if oracleHeight-initialHeight <= 256 { t.Fatal("did not cross oracle retention window") }
var result struct {
Status string `json:"status"`
Coverage struct { Blocks int `json:"blocks"`; Positions int `json:"positions"` } `json:"coverage"`
}
if err := json.Unmarshal(j.Result, &result); err != nil { t.Fatal(err) }
if result.Status != "complete" || result.Coverage.Blocks != 512 || result.Coverage.Positions != 4 {
t.Fatalf("unexpected report after advancing certificates: %s", j.Result)
}
t.Logf("completed after %d batches; certificate advanced %d heights; cursor=%d; only latest certificate accepted by oracle", batch+1, oracleHeight-initialHeight, priorNext)
return
}
}
t.Fatal("advancing oracle prevented completion")
}
