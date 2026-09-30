package api

// Daily verification is auxiliary work: one durable slot, one worker, never
// invoked by consensus. Each invocation checkpoints only fully verified blocks.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const dailyJobKey = "lpod/daily-job/v1"
const dailyBatchBlocks = 32
const dailyBatchTime = 250 * time.Millisecond
const dailyJobMaxBytes = 16 << 20

type dailyPositionProgress struct {
	Vault, After             string
	Accrued, Paid, Principal uint64
}

type dailyProgress struct {
	Certificate                 *store.FinalityCertificate
	Anchor, Witness             crypto.Hash32
	AnchorHeight, WitnessHeight uint64
	Marker                      crypto.Hash32
	Prune                       []byte
	Next                        uint64
	Previous                    crypto.Hash32
	PriorTS                     int64
	Vaults                      map[string][4]uint64
	Positions                   map[string]dailyPositionProgress
	Burns                       [4]uint64
	Included, First, Last       uint64
	FirstTS, LastTS             int64
	Digest                      crypto.Hash32
	HaveDigest                  bool
	Registry                    *core.RegistrySnapshot
}
type dailyJob struct {
	ID       int64
	Updated  int64
	Version  int
	Date     string
	State    string
	Progress *dailyProgress
	Result   json.RawMessage `json:",omitempty"`
}
type dailyJobService struct {
	mu          sync.Mutex
	cancel      context.CancelFunc
	batchCancel context.CancelFunc
	done        chan struct{}
	stopped     bool
}
type dailyProgressKey struct{}

func (s *Server) loadDailyJob() (*dailyJob, error) {
	if s.blockStore == nil {
		return nil, fmt.Errorf("block store unavailable")
	}
	raw, err := s.blockStore.GetMeta(dailyJobKey)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	if len(raw) > dailyJobMaxBytes {
		return nil, fmt.Errorf("daily job exceeds storage bound")
	}
	var j dailyJob
if s.myKey == nil || len(s.myKey.Public()) != 32 {
return nil, fmt.Errorf("daily job authentication key unavailable")
}
var envelope dailyJobEnvelope
if err := decodeDailyStrict(raw, &envelope); err != nil {
		return nil, err
	}
if envelope.Version != 2 || !s.myKey.Public().Verify(dailyJobDigest(envelope.Payload), envelope.Signature) {
return nil, fmt.Errorf("daily job authentication failed; DELETE to reset and reverify")
	}
if err := decodeDailyStrict(envelope.Payload, &j); err != nil { return nil, err }
if err := validateDailyJob(&j); err != nil {
return nil, err
	}
	return &j, nil
}
func (s *Server) saveDailyJob(j *dailyJob) error {
	j.Updated = time.Now().Unix()
raw, err := s.encodeDailyJob(j)
	if err != nil {
		return err
	}
	if len(raw) > dailyJobMaxBytes {
		return fmt.Errorf("daily job exceeds 16 MiB storage bound")
	}
	return s.blockStore.PutMetaSync(dailyJobKey, raw)
}

// StartDailyAuditWorker is idempotent. Recovery is lazy so API startup may
// precede SetBlockStore. A persisted queued job resumes without an HTTP request.
func (s *Server) StartDailyAuditWorker() {
	d := &s.dailyJobs
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil || d.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel, d.done = cancel, make(chan struct{})
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runDailyJobBatch(ctx)
			}
		}
	}()
}
func (s *Server) StopDailyAuditWorker() {
	d := &s.dailyJobs
	d.mu.Lock()
	d.stopped = true
	cancel, done := d.cancel, d.done
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func dailyPending(date, reason string) map[string]interface{} {
	out := map[string]interface{}{
		"version": 1, "date": date, "status": "incomplete", "reason": reason,
		"verification_level": "operator_attested",
		"trust_assumption":   "Validator registry snapshots are operator-attested auxiliary evidence, not an independent on-chain proof.",
		"vaults":             []lpodDailyVault{}, "positions": []lpodDailyPosition{},
		"coverage":       map[string]int{"blocks": 0, "vaults": 0, "positions": 0},
		"opening_anchor": nil, "closing_witness": nil, "certificate": nil, "funding": nil,
		"burns_napro": map[string]string{"protocol_base_fee": "0", "signed_intentional": "0", "avm_gas": "0", "total": "0"},
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		out["reason"] = "Europe/Moscow timezone unavailable"
		return out
	}
	parsed, _ := time.Parse("2006-01-02", date)
	end := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 20, 0, 0, 0, loc)
	out["interval_start"], out["interval_end"] = end.AddDate(0, 0, -1).Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)
	return out
}

// GET only enqueues/reads. DELETE cancels and removes the durable slot, allowing
// an explicit retry of a failed date. Both use the existing privileged route.
func (s *Server) restLPoDDailyJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET or DELETE only")
		return
	}
	q := r.URL.Query()
	date := q.Get("date")
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil || parsed.Format("2006-01-02") != date || len(q) != 1 || len(q["date"]) != 1 {
		writeJSONError(w, http.StatusBadRequest, "exactly one date=YYYY-MM-DD query parameter is required")
		return
	}
	if r.Context().Err() != nil {
		writeJSON(w, http.StatusOK, dailyPending(date, "daily audit request was cancelled"))
		return
	}
	d := &s.dailyJobs
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		writeJSON(w, http.StatusOK, dailyPending(date, "daily worker is stopped"))
		return
	}
	j, err := s.loadDailyJob()
	if r.Method == http.MethodDelete {
// Authenticated reset must work even for oversized, malformed, unsigned or
// wrong-key state. Only enforce date ownership when the record is trusted.
if err == nil && j != nil && j.Date != date {
			writeJSONError(w, http.StatusConflict, "another date occupies the daily slot")
			return
		}
if s.blockStore == nil { writeJSONError(w, http.StatusServiceUnavailable, "block store unavailable"); return }
		if d.batchCancel != nil {
			d.batchCancel()
		}
		if err := s.blockStore.PutMetaSync(dailyJobKey, nil); err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, dailyPending(date, "daily audit cancelled"))
		return
	}
if err != nil {
writeJSON(w, http.StatusOK, dailyPending(date, err.Error()))
return
}
	if j != nil && j.Date == date {
		if (j.State == "failed" && time.Now().Unix()-j.Updated >= 30) ||
			(j.State == "complete" && (!s.dailyBindingValid(j.Progress) ||
				!s.lpodFinalized(j.Progress.Certificate.Height, j.Progress.Certificate.BlockHash))) {
			// Re-attest an older complete report by verifying only its unprocessed
			// descendant tail. Invalidated/failed progress is never reused.
			if j.State == "failed" || !s.dailyBindingValid(j.Progress) {
				j.Progress = &dailyProgress{}
			}
			j.ID, j.State, j.Result = time.Now().UnixNano(), "queued", nil
			if err := s.saveDailyJob(j); err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, dailyPending(date, "queued for bounded background verification"))
			return
		}
		if len(j.Result) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Write(j.Result)
			return
		}
		writeJSON(w, http.StatusOK, dailyPending(date, "queued for bounded background verification"))
		return
	}
	if j != nil && j.State == "queued" {
		writeJSONError(w, http.StatusTooManyRequests, "daily audit queue is full")
		return
	}
	j = &dailyJob{ID: time.Now().UnixNano(), Version: 1, Date: date, State: "queued", Progress: &dailyProgress{}}
	if err := s.saveDailyJob(j); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dailyPending(date, "queued for bounded background verification"))
}

func (s *Server) dailyBindingValid(p *dailyProgress) bool {
	if p == nil || p.Certificate == nil || s.lpodFinalized == nil {
		return false
	}
	c := p.Certificate
	h, found, err := s.blockStore.GetCanonicalHash(c.Height)
	if err != nil || !found || h != c.BlockHash {
		return false
	}
	latest, err := s.blockStore.LoadFinalityCertificate()
	if err != nil || latest == nil || latest.Height < c.Height {
		return false
	}
	latestHash, ok, err := s.blockStore.GetCanonicalHash(latest.Height)
	if err != nil || !ok || latestHash != latest.BlockHash || !s.lpodFinalized(latest.Height, latest.BlockHash) {
		return false
	}
	if p.Anchor != (crypto.Hash32{}) {
		anchor, ok, err := s.blockStore.GetCanonicalHash(p.AnchorHeight)
		if err != nil || !ok || anchor != p.Anchor {
			return false
		}
		witness, ok, err := s.blockStore.GetCanonicalHash(p.WitnessHeight)
		if err != nil || !ok || witness != p.Witness {
			return false
		}
		prune, err := s.blockStore.GetMeta("prune_cursor")
		if err != nil || !bytes.Equal(prune, p.Prune) {
			return false
		}
		marker, found, err := s.blockStore.LoadLPoDAuditStart()
		if err != nil || !found {
			return false
		}
		raw, err := json.Marshal(marker)
		if err != nil || crypto.HashBytes(raw) != p.Marker {
			return false
		}
	}
	return true
}

func (s *Server) runDailyJobBatch(ctx context.Context) {
	// Do not hold this mutex while verifying: HTTP must never wait for a scan.
	d := &s.dailyJobs
	d.mu.Lock()
	j, err := s.loadDailyJob()
	batchCtx, batchCancel := context.WithCancel(ctx)
	d.batchCancel = batchCancel
	d.mu.Unlock()
	defer func() {
		batchCancel()
		d.mu.Lock()
		d.batchCancel = nil
		d.mu.Unlock()
	}()
	ctx = batchCtx
	if err != nil || j == nil || j.State != "queued" || ctx.Err() != nil {
		return
	}
	if j.Progress == nil {
		j.Progress = &dailyProgress{}
	}
	before := j.Progress.Next
	req := httptest.NewRequest(http.MethodGet, "/?date="+j.Date, nil)
	req = req.WithContext(context.WithValue(ctx, dailyProgressKey{}, j.Progress))
	rec := httptest.NewRecorder()
	s.restLPoDAuditDaily(rec, req)
	if ctx.Err() != nil {
		return
	} // retain last fully committed batch
	j.Result = append(json.RawMessage(nil), rec.Body.Bytes()...)
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(j.Result, &result) != nil {
		return
	}
	if result.Status == "complete" {
		j.State = "complete"
	} else if j.Progress.Next == before {
		j.State = "failed"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, err := s.loadDailyJob()
	// Cancellation/replacement wins over an in-flight batch.
	if err != nil || current == nil || current.ID != j.ID || current.State != "queued" || current.Progress == nil || current.Progress.Next != before || d.stopped {
		return
	}
	if err := s.saveDailyJob(j); err != nil {
		j.State, j.Progress = "failed", nil
		j.Result, _ = json.Marshal(dailyPending(j.Date, err.Error()))
		if saveErr := s.saveDailyJob(j); saveErr != nil && s.log != nil {
			s.log.Error("daily audit progress persistence failed", "err", saveErr)
		}
	}
}
