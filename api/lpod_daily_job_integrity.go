package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"time"

	"github.com/aperod/aperod/crypto"
)

// The key is the existing protected node validator key, never a key or checksum
// stored beside the job in LevelDB. Domain separation prevents a job signature
// from being usable as a block/finality/transaction signature. Sign exact payload
// bytes and authenticate BEFORE decoding/reusing any saved state.
type dailyJobEnvelope struct {
	Version   int
	Payload   json.RawMessage
	Signature []byte
}

func dailyJobDigest(payload []byte) crypto.Hash32 {
	return crypto.HashBytes([]byte("aperod/lpod-daily-job/v2\x00"), payload)
}
func decodeDailyStrict(raw []byte, dst interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid daily job encoding: %w", err)
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("trailing daily job encoding")
	}
	return nil
}
func (s *Server) encodeDailyJob(j *dailyJob) ([]byte, error) {
	if err := validateDailyJob(j); err != nil {
		return nil, err
	}
	if s.myKey == nil {
		return nil, fmt.Errorf("daily job authentication key unavailable")
	}
	payload, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if len(payload) > dailyJobMaxBytes {
		return nil, fmt.Errorf("daily job exceeds storage bound")
	}
	signature, err := s.myKey.Sign(dailyJobDigest(payload))
	if err != nil {
		return nil, fmt.Errorf("daily job authentication: %w", err)
	}
	raw, err := json.Marshal(dailyJobEnvelope{Version: 2, Payload: payload, Signature: signature})
	if len(raw) > dailyJobMaxBytes {
		return nil, fmt.Errorf("daily job exceeds storage bound")
	}
	return raw, err
}

func validateDailyJob(j *dailyJob) error {
	fail := func() error {
		return fmt.Errorf("daily job state/progress/result inconsistent; DELETE to reset and reverify")
	}
	date, err := time.Parse("2006-01-02", j.Date)
	if err != nil || date.Format("2006-01-02") != j.Date || j.ID <= 0 || j.Updated <= 0 || j.Version != 1 {
		return fail()
	}
	if j.State != "queued" && j.State != "failed" && j.State != "complete" {
		return fail()
	}
	p := j.Progress
	if p == nil {
		if j.State != "failed" {
			return fail()
		}
	} else if p.Next == 0 {
		if !reflect.DeepEqual(*p, dailyProgress{}) || j.State == "complete" {
			return fail()
		}
	} else {
		if p.Certificate == nil || p.Certificate.Version != 1 ||
			p.Certificate.Height == ^uint64(0) || p.Certificate.BlockHash == (crypto.Hash32{}) ||
			p.Anchor == (crypto.Hash32{}) || p.Witness == (crypto.Hash32{}) ||
			p.Previous == (crypto.Hash32{}) || p.Marker == (crypto.Hash32{}) ||
			p.AnchorHeight >= p.WitnessHeight || p.WitnessHeight > p.Certificate.Height ||
			p.Next <= p.AnchorHeight || p.Next > p.Certificate.Height+1 ||
			p.Certificate.Height-p.AnchorHeight+1 > lpodDailyMaxBlocks ||
			(len(p.Prune) != 0 && len(p.Prune) != 8) ||
			len(p.Vaults)+len(p.Positions) > lpodDailyMaxEntries {
			return fail()
		}
		lastExclusive := p.Next
		if lastExclusive > p.WitnessHeight {
			lastExclusive = p.WitnessHeight
		}
		if p.Included != lastExclusive-(p.AnchorHeight+1) {
			return fail()
		}
		if p.Next == p.Certificate.Height+1 && p.Previous != p.Certificate.BlockHash {
			return fail()
		}
		if p.Included == 0 {
			if p.HaveDigest || p.Registry != nil || p.Digest != (crypto.Hash32{}) ||
				p.First != 0 || p.Last != 0 || p.FirstTS != 0 || p.LastTS != 0 ||
				len(p.Vaults)+len(p.Positions) != 0 || p.Burns != ([4]uint64{}) {
				return fail()
			}
		} else {
			if !p.HaveDigest || p.Digest == (crypto.Hash32{}) || p.Registry == nil ||
				p.Registry.Validators == nil || p.First != p.AnchorHeight+1 ||
				p.Last != p.First+p.Included-1 || p.LastTS < p.FirstTS || p.PriorTS < p.LastTS {
				return fail()
			}
			loc, err := time.LoadLocation("Europe/Moscow")
			if err != nil {
				return err
			}
			end := time.Date(date.Year(), date.Month(), date.Day(), 20, 0, 0, 0, loc)
			if p.FirstTS < end.AddDate(0, 0, -1).UnixNano() || p.LastTS >= end.UnixNano() {
				return fail()
			}
			for id, validator := range p.Registry.Validators {
				if validator == nil || len(validator.PubKey) != 32 || validator.PubKey.Hex() != id {
					return fail()
				}
			}
		}
		var burn uint64
		for _, amount := range p.Burns[:3] {
			if !sumDaily(&burn, amount) {
				return fail()
			}
		}
		if burn != p.Burns[3] {
			return fail()
		}
		for id := range p.Vaults {
			if id == "" {
				return fail()
			}
		}
		for id, position := range p.Positions {
			if id == "" || position.Vault == "" || position.After == "" {
				return fail()
			}
		}
		if j.State == "complete" && (p.Next != p.Certificate.Height+1 ||
			p.Included == 0 || p.Last+1 != p.WitnessHeight) {
			return fail()
		}
	}
	if len(j.Result) == 0 {
		if j.State != "queued" {
			return fail()
		}
		return nil
	}
	var r struct {
		Version       int    `json:"version"`
		Date          string `json:"date"`
		Status        string `json:"status"`
		Level         string `json:"verification_level"`
		Trust         string `json:"trust_assumption"`
		IntervalStart string `json:"interval_start"`
		IntervalEnd   string `json:"interval_end"`
		Opening       struct {
			Height uint64 `json:"height"`
			Hash   string `json:"hash"`
		} `json:"opening_anchor"`
		Closing struct {
			Height uint64 `json:"height"`
			Hash   string `json:"hash"`
		} `json:"closing_witness"`
		Vaults    []lpodDailyVault    `json:"vaults"`
		Positions []lpodDailyPosition `json:"positions"`
		Burns     map[string]string   `json:"burns_napro"`
		Coverage  struct {
			Blocks    uint64 `json:"blocks"`
			First     uint64 `json:"first_height"`
			Last      uint64 `json:"last_height"`
			Vaults    int    `json:"vaults"`
			Positions int    `json:"positions"`
			FirstTS   string `json:"first_timestamp"`
			LastTS    string `json:"last_timestamp"`
		} `json:"coverage"`
		Certificate struct {
			Height uint64 `json:"height"`
			Hash   string `json:"block_hash"`
		} `json:"certificate"`
	}
	if err := json.Unmarshal(j.Result, &r); err != nil || r.Version != 1 || r.Date != j.Date || r.Level != "operator_attested" || r.Trust == "" {
		return fail()
	}
	if j.State != "complete" {
		if r.Status != "incomplete" {
			return fail()
		}
		return nil
	}
	if r.Status != "complete" || p == nil || p.Certificate == nil ||
		r.Coverage.Blocks != p.Included || r.Coverage.First != p.First || r.Coverage.Last != p.Last ||
		r.Coverage.Vaults != len(p.Vaults) || r.Coverage.Positions != len(p.Positions) ||
		len(r.Vaults) != len(p.Vaults) || len(r.Positions) != len(p.Positions) ||
		r.Certificate.Height != p.Certificate.Height || r.Certificate.Hash != hex.EncodeToString(p.Certificate.BlockHash[:]) {
		return fail()
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return err
	}
	end := time.Date(date.Year(), date.Month(), date.Day(), 20, 0, 0, 0, loc)
	if r.IntervalStart != end.AddDate(0, 0, -1).Format(time.RFC3339Nano) || r.IntervalEnd != end.Format(time.RFC3339Nano) ||
		r.Opening.Height != p.AnchorHeight || r.Opening.Hash != hex.EncodeToString(p.Anchor[:]) ||
		r.Closing.Height != p.WitnessHeight || r.Closing.Hash != hex.EncodeToString(p.Witness[:]) ||
		r.Coverage.FirstTS != time.Unix(0, p.FirstTS).Format(time.RFC3339Nano) ||
		r.Coverage.LastTS != time.Unix(0, p.LastTS).Format(time.RFC3339Nano) {
		return fail()
	}
	matches := func(text string, value uint64) bool { return text == strconv.FormatUint(value, 10) }
	seen := make(map[string]bool)
	for _, v := range r.Vaults {
		values, ok := p.Vaults[v.ID]
		if !ok || seen[v.ID] || !matches(v.Accrued, values[0]) || !matches(v.ActualLeaderPaid, values[1]) ||
			!matches(v.GuardianPaid, values[2]) || !matches(v.Unfunded, values[3]) {
			return fail()
		}
		seen[v.ID] = true
	}
	seen = make(map[string]bool)
	for _, v := range r.Positions {
		values, ok := p.Positions[v.ID]
		if !ok || seen[v.ID] || values.Vault != v.EffectiveVault || values.After != v.EffectiveVaultAfter ||
			!matches(v.Accrued, values.Accrued) || !matches(v.Paid, values.Paid) || !matches(v.PrincipalReturned, values.Principal) {
			return fail()
		}
		seen[v.ID] = true
	}
	for i, name := range []string{"protocol_base_fee", "signed_intentional", "avm_gas", "total"} {
		if !matches(r.Burns[name], p.Burns[i]) {
			return fail()
		}
	}
	return nil
}
