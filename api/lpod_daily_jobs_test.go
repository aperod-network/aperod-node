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

	"github.com/syndtr/goleveldb/leveldb"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
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
	db, s, path, _ := makeDailyAuditBenchmarkFixture(t, dailyBatchBlocks+70, 1, 128, 4, 0, 8)
	enqueueDailyTest(t, s)
	other := authenticatedDailyAuditRequest()
	other.URL.RawQuery = "date=2024-01-03"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, other)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("queue must reject second date: %d", rec.Code)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	s.runDailyJobBatch(canceled)
	j, _ := s.loadDailyJob()
	if j.Progress.Next != 0 {
		t.Fatal("canceled batch changed progress")
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
	if err != nil {
		t.Fatal(err)
	}
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

func TestDailyJobPrecomputesBeforeClosingWitnessAndFinalizesAfterIt(t *testing.T) {
	db, s, path, _ := makeDailyAuditBenchmarkFixtureWithTail(t, 2, 0, 0, 1, 0, 0, 1)
	defer func() { _ = db.Close() }()

	earlyHash, found, err := db.GetCanonicalHash(2)
	if err != nil || !found {
		t.Fatalf("load pre-close certificate hash: found=%v err=%v", found, err)
	}
	closingHash, found, err := db.GetCanonicalHash(3)
	if err != nil || !found {
		t.Fatalf("load closing witness hash: found=%v err=%v", found, err)
	}
	earlyCertificate, err := json.Marshal(store.FinalityCertificate{
		Version: 1, Height: 2, BlockHash: earlyHash,
		Votes: []store.FinalityVote{{Signature: []byte{1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rawDB, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Put([]byte("consensus/finality_certificate/v1"), earlyCertificate, nil); err != nil {
		_ = rawDB.Close()
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetStore(db)
	persistedCertificate, err := db.LoadFinalityCertificate()
	if err != nil || persistedCertificate == nil || persistedCertificate.Height != 2 ||
		persistedCertificate.BlockHash != earlyHash {
		t.Fatalf("pre-close certificate fixture was not persisted: cert=%+v err=%v", persistedCertificate, err)
	}
	oracleHeight, oracleHash := uint64(2), earlyHash
	s.SetLPoDConfig(nil, func(height uint64, hash crypto.Hash32) bool {
		ok := height == oracleHeight && hash == oracleHash
		if !ok {
			t.Logf("finality oracle mismatch: got height=%d hash=%x want height=%d hash=%x", height, hash, oracleHeight, oracleHash)
		}
		return ok
	})
	enqueueDailyTest(t, s)

	s.runDailyJobBatch(context.Background())
	j, err := s.loadDailyJob()
	if err != nil || j == nil || j.State != "queued" || j.Progress == nil ||
		!j.Progress.Preclosing || j.Progress.Included != 1 ||
		!dailyJobResultIsWaiting(j.Result) {
		t.Fatalf("pre-close work did not remain safely queued: job=%+v err=%v", j, err)
	}

	if err := db.SaveFinalityCertificate(store.FinalityCertificate{
		Version: 1, Height: 3, BlockHash: closingHash,
		Votes: []store.FinalityVote{{Signature: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	oracleHeight, oracleHash = 3, closingHash
	for attempt := 0; attempt < 4; attempt++ {
		s.runDailyJobBatch(context.Background())
		j, err = s.loadDailyJob()
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "complete" {
			break
		}
	}
	var result struct {
		Status   string `json:"status"`
		Coverage struct {
			Blocks int `json:"blocks"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal(j.Result, &result); err != nil ||
		j.State != "complete" || j.Progress.Preclosing ||
		result.Status != "complete" || result.Coverage.Blocks != 2 {
		t.Fatalf("closing certificate did not finalize the complete daily audit: job=%+v result=%+v err=%v", j, result, err)
	}
}

func TestDailyJobResultWaitingVocabulary(t *testing.T) {
	for _, reason := range []string{
		"opening boundary not yet certified",
		"closing witness not yet present",
		"verified certificate predates the closing witness",
		"bounded background verification in progress",
	} {
		raw, _ := json.Marshal(map[string]string{"status": "incomplete", "reason": reason})
		if !dailyJobResultIsWaiting(raw) {
			t.Errorf("expected %q to remain queued", reason)
		}
	}
	if dailyJobResultIsWaiting([]byte(`{"status":"incomplete","reason":"block audit verification failed"}`)) ||
		dailyJobResultIsWaiting([]byte(`{"status":"complete","reason":""}`)) {
		t.Fatal("non-waiting result was treated as retryable")
	}
}

func TestDailyJobReorgAndFinalityFailClosed(t *testing.T) {
	for _, change := range []string{"predecessor", "certificate", "finality", "prune"} {
		t.Run(change, func(t *testing.T) {
			db, s, path, _ := makeDailyAuditBenchmarkFixture(t, dailyBatchBlocks+40, 0, 0, 1, 0, 1)
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
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
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
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, dailyBatchBlocks+40, 0, 0, 1, 0, 0)
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

func TestDailyJobStorageBudgetAndFailedResultRequiresExplicitRetry(t *testing.T) {
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, 2, 0, 0, 1, 0, 0)
	defer db.Close()
	j := enqueueDailyTest(t, s)
	oversize := *j
	oversize.Result = json.RawMessage(`"` + strings.Repeat("x", dailyJobMaxBytes) + `"`)
	if err := s.saveDailyJob(&oversize); err == nil {
		t.Fatal("accepted oversized job")
	}
	failedAnchor := crypto.HashBytes([]byte("failed daily audit anchor"))
	failedProgress := &dailyProgress{
		Certificate: &store.FinalityCertificate{
			Version: 1, Height: 2, BlockHash: crypto.HashBytes([]byte("failed daily certificate")),
		},
		Anchor: failedAnchor, Witness: crypto.HashBytes([]byte("failed daily witness")),
		AnchorHeight: 0, WitnessHeight: 1, Marker: crypto.HashBytes([]byte("failed daily marker")),
		Next: 1, Previous: crypto.HashBytes([]byte("failed daily previous")),
	}
	j.State, j.Progress = "failed", failedProgress
	j.Result, _ = json.Marshal(dailyPending(j.Date, "exact block audit record missing at height 20"))
	j.Updated = time.Now().Add(-time.Minute).Unix()
	raw, err := s.encodeDailyJob(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutMetaSync(dailyJobKey, raw); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, authenticatedDailyAuditRequest())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "exact block audit record missing at height 20") {
		t.Fatalf("failed result was hidden by automatic retry: %d %s", rec.Code, rec.Body.String())
	}
	current, err := s.loadDailyJob()
	if err != nil || current == nil || current.State != "failed" || current.ID != j.ID {
		t.Fatalf("GET mutated the failed job: %+v %v", current, err)
	}

	reset := authenticatedDailyAuditRequest()
	reset.Method = http.MethodDelete
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, reset)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit retry reset failed: %d %s", rec.Code, rec.Body.String())
	}
	if cleared, err := s.loadDailyJob(); err != nil || cleared != nil {
		t.Fatalf("explicit reset retained failed job: %+v %v", cleared, err)
	}
	retry := enqueueDailyTest(t, s)
	if retry.ID == j.ID {
		t.Fatal("explicit retry reused the failed job identity")
	}
}

// Production retains only 256 finalized heights in memory and restart may
// retain only the latest one. Require forward progress when EVERY previously
// saved certificate disappears from the oracle, not merely after 256 heights.
func TestDailyJobAdvancingCertificateOracleDoesNotResetCursor(t *testing.T) {
	const intervalBlocks = 4 * dailyBatchBlocks
	db, s, _, _ := makeDailyAuditBenchmarkFixtureWithTail(t, intervalBlocks, 1, 128, 4, 0, 4, intervalBlocks)
	defer db.Close()
	certificate, err := db.LoadFinalityCertificate()
	if err != nil {
		t.Fatal(err)
	}
	initialHeight := certificate.Height
	_, tipHeight, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	var oracleHeight uint64
	var oracleHash crypto.Hash32
	oracle := func(height uint64, hash crypto.Hash32) bool {
		return height == oracleHeight && hash == oracleHash
	}
	enqueueDailyTest(t, s)
	var priorNext uint64
	for batch := 0; batch < 100; batch++ {
		height := initialHeight + uint64(batch)*uint64(dailyBatchBlocks/2)
		if height > tipHeight {
			height = tipHeight
		}
		hash, found, err := db.GetCanonicalHash(height)
		if err != nil || !found {
			t.Fatal("missing tail certificate hash")
		}
		if err := db.SaveFinalityCertificate(store.FinalityCertificate{
			Version: 1, Height: height, BlockHash: hash,
			Votes: []store.FinalityVote{{Signature: []byte{1}}},
		}); err != nil {
			t.Fatal(err)
		}
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
		if err != nil || j == nil || j.State == "failed" {
			t.Fatalf("batch %d failed: %+v %v", batch, j, err)
		}
		if j.Progress.Next <= priorNext || j.Progress.Next-priorNext > dailyBatchBlocks {
			t.Fatalf("certificate advancement reset/stalled/unbounded cursor: batch=%d before=%d after=%d", batch, priorNext, j.Progress.Next)
		}
		priorNext = j.Progress.Next
		if j.State == "complete" {
			if oracleHeight-initialHeight <= 256 {
				t.Fatal("did not cross oracle retention window")
			}
			var result struct {
				Status   string `json:"status"`
				Coverage struct {
					Blocks    int `json:"blocks"`
					Positions int `json:"positions"`
				} `json:"coverage"`
			}
			if err := json.Unmarshal(j.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != "complete" || result.Coverage.Blocks != intervalBlocks || result.Coverage.Positions != 4 {
				t.Fatalf("unexpected report after advancing certificates: %s", j.Result)
			}
			t.Logf("completed after %d batches; certificate advanced %d heights; cursor=%d; only latest certificate accepted by oracle", batch+1, oracleHeight-initialHeight, priorNext)
			return
		}
	}
	t.Fatal("advancing oracle prevented completion")
}

func TestDailyOpeningBoundaryFailureDistinguishesBoundedWindow(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	before := start.Add(-time.Second).UnixNano()
	after := start.Add(time.Second).UnixNano()
	tests := []struct {
		name                   string
		anchorTimestamp        int64
		certifiedWindowLimited bool
		candidateAtSearchFloor bool
		parentHashMatches      bool
		want                   string
	}{
		{
			name:                   "bounded certificate window starts after opening",
			anchorTimestamp:        after,
			certifiedWindowLimited: true,
			candidateAtSearchFloor: true,
			parentHashMatches:      true,
			want:                   "certified search window starts after interval opening",
		},
		{
			name:              "actual parent mismatch",
			anchorTimestamp:   before,
			parentHashMatches: false,
			want:              "opening boundary is discontinuous",
		},
		{
			name:            "anchor timestamp is not before interval",
			anchorTimestamp: after,
			want:            "opening anchor timestamp is not before interval start",
		},
		{
			name:              "valid opening link",
			anchorTimestamp:   before,
			parentHashMatches: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dailyOpeningBoundaryFailure(start, tt.anchorTimestamp, tt.certifiedWindowLimited, tt.candidateAtSearchFloor, tt.parentHashMatches)
			if got != tt.want {
				t.Fatalf("dailyOpeningBoundaryFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDailyAuditIncompleteOmitsUnverifiedBurnTotals(t *testing.T) {
	s := &Server{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/lpod/audit/daily?date=2026-11-05", nil)
	s.restLPoDAuditDaily(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("incomplete audit status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode incomplete audit: %v", err)
	}
	if _, ok := body["burns_napro"]; ok {
		t.Fatal("incomplete audit exposed unverified burn totals")
	}
	if _, ok := dailyPending("2026-11-05", "queued")["burns_napro"]; ok {
		t.Fatal("pending response exposed unverified burn totals")
	}
}
