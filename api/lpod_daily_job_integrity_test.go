package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDailyJobTamperingRejectedAcrossRestart(t *testing.T) {
	db, original, _, _ := makeDailyAuditBenchmarkFixture(t, 70, 1, 128, 4, 0, 8)
	defer db.Close()
	enqueueDailyTest(t, original)
	original.runDailyJobBatch(context.Background())
	trusted, err := db.GetMeta(dailyJobKey)
	if err != nil {
		t.Fatal(err)
	}
	var baseline dailyJobEnvelope
	if err := json.Unmarshal(trusted, &baseline); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"vault accrued", "position paid", "registry", "registry stake", "digest flag", "result", "date", "cursor", "signature", "unsigned legacy"} {
		t.Run(mutation, func(t *testing.T) {
			var envelope dailyJobEnvelope
			if err := json.Unmarshal(trusted, &envelope); err != nil {
				t.Fatal(err)
			}
			var j dailyJob
			if err := json.Unmarshal(envelope.Payload, &j); err != nil {
				t.Fatal(err)
			}
			if j.Progress.Included == 0 {
				t.Fatal("fixture has no authenticated prefix")
			}
			switch mutation {
			case "vault accrued":
				for id, row := range j.Progress.Vaults {
					row[0]++
					j.Progress.Vaults[id] = row
					break
				}
			case "position paid":
				for id, row := range j.Progress.Positions {
					row.Paid++
					j.Progress.Positions[id] = row
					break
				}
			case "registry":
				j.Progress.Registry = nil
			case "registry stake":
				for _, validator := range j.Progress.Registry.Validators {
					validator.StakeNAPR++
					break
				}
			case "digest flag":
				j.Progress.HaveDigest = false
			case "result":
				var result map[string]interface{}
				json.Unmarshal(j.Result, &result)
				result["status"] = "complete"
				j.Result, _ = json.Marshal(result)
			case "date":
				j.Date = "2024-01-03"
			case "cursor":
				j.Progress.Next++
			case "signature":
				envelope.Signature[0] ^= 1
			}
			envelope.Payload, _ = json.Marshal(j)
			tampered, _ := json.Marshal(envelope)
			if mutation == "unsigned legacy" {
				tampered = envelope.Payload
			}
			if bytes.Equal(tampered, trusted) {
				t.Fatal("test did not mutate record")
			}
			if err := db.PutMetaSync(dailyJobKey, tampered); err != nil {
				t.Fatal(err)
			}
			restarted := NewServer(":0", nil, nil, nil, slog.Default())
			restarted.SetStore(db)
			restarted.SetValidatorKey(original.myKey)
			restarted.SetAPIKey(dailyAuditBenchmarkAPIKey)
			restarted.SetLPoDConfig(nil, original.lpodFinalized)
			if _, err := restarted.loadDailyJob(); err == nil {
				t.Fatal("accepted tampered progress")
			}
			restarted.runDailyJobBatch(context.Background())
			after, _ := db.GetMeta(dailyJobKey)
			if !bytes.Equal(after, tampered) {
				t.Fatal("worker consumed/re-signed untrusted progress")
			}
			rec := httptest.NewRecorder()
			restarted.ServeHTTP(rec, authenticatedDailyAuditRequest())
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"incomplete"`) ||
				strings.Contains(rec.Body.String(), `"status":"complete"`) {
				t.Fatalf("unsafe result: %s", rec.Body.String())
			}
afterGET, err := db.GetMeta(dailyJobKey)
if err != nil || !bytes.Equal(afterGET, tampered) {
t.Fatal("GET overwrote/re-signed corrupt state instead of reporting the load error")
}
		})
	}
	if err := db.PutMetaSync(dailyJobKey, trusted); err != nil {
		t.Fatal(err)
	}
	if _, err := original.loadDailyJob(); err != nil {
		t.Fatalf("legitimate signed progress rejected: %v", err)
	}
}

// Even an authenticated payload must meet producer invariants. Sign directly
// in this test to simulate a buggy signer; production encodeDailyJob refuses
// such states before signing.
func TestDailyJobAuthenticatedStateConsistency(t *testing.T) {
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, 70, 0, 0, 1, 0, 2)
	defer db.Close()
	enqueueDailyTest(t, s)
	s.runDailyJobBatch(context.Background())
	partial, err := s.loadDailyJob()
	if err != nil {
		t.Fatal(err)
	}
	partialBytes, _ := json.Marshal(partial)
	for n := 0; n < 75; n++ {
		j, err := s.loadDailyJob()
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "complete" {
			break
		}
		s.runDailyJobBatch(context.Background())
	}
	complete, err := s.loadDailyJob()
	if err != nil || complete.State != "complete" {
		t.Fatalf("completion setup failed: %v", err)
	}
	completeBytes, _ := json.Marshal(complete)
	for _, mutation := range []string{"queued complete result", "failed complete result", "complete incomplete result", "missing registry", "missing digest flag", "missing certificate", "impossible cursor", "coverage", "paid result"} {
		t.Run(mutation, func(t *testing.T) {
			var j dailyJob
			source := completeBytes
			if mutation == "missing registry" || mutation == "missing digest flag" || mutation == "impossible cursor" {
				source = partialBytes
			}
			json.Unmarshal(source, &j)
			switch mutation {
			case "queued complete result":
				j.State = "queued"
			case "failed complete result":
				j.State = "failed"
			case "missing registry":
				j.Progress.Registry = nil
			case "missing digest flag":
				j.Progress.HaveDigest = false
			case "missing certificate":
				j.Progress.Certificate = nil
			case "impossible cursor":
				j.Progress.Next = j.Progress.AnchorHeight
			default:
				var result map[string]interface{}
				json.Unmarshal(j.Result, &result)
				switch mutation {
				case "complete incomplete result":
					result["status"] = "incomplete"
				case "coverage":
					result["coverage"].(map[string]interface{})["blocks"] = float64(1)
				case "paid result":
					result["positions"].([]interface{})[0].(map[string]interface{})["paid_napro"] = "123456"
				}
				j.Result, _ = json.Marshal(result)
			}
			if _, err := s.encodeDailyJob(&j); err == nil {
				t.Fatal("producer signed inconsistent state")
			}
			payload, _ := json.Marshal(j)
			signature, err := s.myKey.Sign(dailyJobDigest(payload))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(dailyJobEnvelope{2, payload, signature})
			if err := db.PutMetaSync(dailyJobKey, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := s.loadDailyJob(); err == nil {
				t.Fatal("consumer accepted inconsistent signed state")
			}
		})
	}
}

func TestDailyJobResetUnreadableAndWrongKeyState(t *testing.T) {
	db, s, _, _ := makeDailyAuditBenchmarkFixture(t, 2, 0, 0, 1, 0, 0)
	defer db.Close()
	enqueueDailyTest(t, s)
	trusted, _ := db.GetMeta(dailyJobKey)
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{"malformed", []byte("{")},
		{"oversized", bytes.Repeat([]byte("x"), dailyJobMaxBytes+1)},
		{"unknown version", []byte(`{"Version":900}`)},
		{"null", []byte("null")},
		{"wrong key", trusted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "wrong key" {
				benchmarkDailySigningKey(t, s)
			}
			if err := db.PutMetaSync(dailyJobKey, test.raw); err != nil {
				t.Fatal(err)
			}
			if _, err := s.loadDailyJob(); err == nil {
				t.Fatal("unreadable/wrong-key state accepted")
			}
getRecorder := httptest.NewRecorder()
s.ServeHTTP(getRecorder, authenticatedDailyAuditRequest())
afterGET, err := db.GetMeta(dailyJobKey)
if err != nil || !bytes.Equal(afterGET, test.raw) {
t.Fatal("GET changed unreadable state")
}
if getRecorder.Code != http.StatusOK || strings.Contains(getRecorder.Body.String(), "queued for bounded") {
t.Fatalf("GET did not report unreadable state: %d %s", getRecorder.Code, getRecorder.Body.String())
}
			req := authenticatedDailyAuditRequest()
			req.Method = http.MethodDelete
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reason":"daily audit cancelled"`) {
				t.Fatalf("reset failed: %d %s", rec.Code, rec.Body.String())
			}
			raw, err := db.GetMeta(dailyJobKey)
			if err != nil || len(raw) != 0 {
				t.Fatalf("reset not durable: %v", err)
			}
		})
	}
}
