// mint-record-audit verifies old operator records against a CLOSED database
// copy. It never updates amounts, repairs state or remints missing outputs.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type record struct {
	ID            int    `json:"id"`
	Address       string `json:"address"`
	TxHash        string `json:"tx_hash"`
	OutputIndex   uint32 `json:"output_index"`
	CandidateAPRO string `json:"candidate_apro"`
	BlindHex      string `json:"blind_hex"`
	// Optional public output material, e.g. from a pinned node response. Its
	// opening alone is NEVER a canonical membership or unspentness claim.
	PublicCommit    string `json:"public_commit_hex,omitempty"`
	PublicHeight    uint64 `json:"public_height,omitempty"`
	WholeOpeningCap uint64 `json:"whole_opening_cap,omitempty"`
}
type evidence struct {
	ID             int     `json:"id"`
	Status         string  `json:"status"`
	Amount         *uint64 `json:"verified_amount_napro,string"`
	Height         uint64  `json:"stored_output_height"`
	CanonicalBody  bool    `json:"canonical_body_verified"`
	MaterialSource string  `json:"material_source"`
}

func amountCandidate(s string) (uint64, bool) {
	x, ok := new(big.Rat).SetString(s)
	if !ok || x.Sign() < 0 {
		return 0, false
	}
	x.Mul(x, new(big.Rat).SetInt64(100000000))
	if !x.IsInt() || !x.Num().IsUint64() {
		return 0, false
	}
	return x.Num().Uint64(), true
}

// A display value is only a search hint. Accept it ONLY when the derived blind
// and actual output commitment both match. Never trust the SQL float itself.
func recoverAmount(r record, u *store.StoredUTXO, maxWhole uint64, extra []uint64) (*uint64, string) {
	_, pub, _, err := crypto.DecodeAddress(crypto.Address(r.Address))
	if err != nil {
		return nil, "invalid_address"
	}
	raw, err := hex.DecodeString(r.BlindHex)
	if err != nil || len(raw) != 32 {
		return nil, "missing_blind_evidence"
	}
	var blind crypto.BlindFactor
	copy(blind[:], raw)
	verify := func(n uint64) bool {
		b, err := crypto.DeterministicMintBlind(pub, n)
		if err != nil || !bytes.Equal(b[:], blind[:]) {
			return false
		}
		c, err := crypto.Commit(n, b)
		return err == nil && c == u.AmountCommit
	}
	candidates := append([]uint64(nil), extra...)
	if n, ok := amountCandidate(r.CandidateAPRO); ok {
		candidates = append(candidates, n)
	}
	for _, n := range candidates {
		// Old releases also used random blinds. A candidate opening with the
		// recorded blind is sufficient when the ACTUAL commitment matches.
		c, err := crypto.Commit(n, blind)
		if (err == nil && c == u.AmountCommit) || verify(n) {
			v := n
			return &v, "opening_verified"
		}
	}
	for whole := uint64(0); whole <= maxWhole; whole++ {
		n := whole * 100000000
		if verify(n) {
			v := n
			return &v, "opening_verified"
		}
	}
	if r.WholeOpeningCap != 0 {
		n, err := wholeOpening(u.AmountCommit, blind, r.WholeOpeningCap)
		if err != nil {
			return nil, "whole_opening_search_invalid"
		}
		if n != nil {
			return n, "opening_verified"
		}
	}
	return nil, "opening_unresolved"
}

func audit(d *store.DB, r record, maxWhole uint64, extra []uint64) evidence {
	out := evidence{ID: r.ID}
	raw, err := hex.DecodeString(r.TxHash)
	if err != nil || len(raw) != 32 {
		out.Status = "invalid_transaction_identity"
		return out
	}
	var hash crypto.Hash32
	copy(hash[:], raw)
	u, err := d.GetUTXO(hash, r.OutputIndex)
	if err != nil {
		out.Status = "output_read_failed"
		return out
	}
	if u == nil {
		if r.PublicCommit == "" {
			out.Status = "output_absent_not_reminted"
			return out
		}
		material, err := hex.DecodeString(r.PublicCommit)
		if err != nil || len(material) != 32 {
			out.Status = "invalid_public_material"
			return out
		}
		u = &store.StoredUTXO{BlockHeight: r.PublicHeight}
		copy(u.AmountCommit[:], material)
		out.MaterialSource = "caller_public_material_not_snapshot_output"
	} else {
		out.MaterialSource = "closed_database_output"
	}
	out.Height = u.BlockHeight
	out.Amount, out.Status = recoverAmount(r, u, maxWhole, extra)
	if out.Amount == nil {
		return out
	}
	block, err := d.GetBlockByHeight(u.BlockHeight)
	if err != nil || block == nil {
		out.Status = "opening_verified_body_unavailable"
		return out
	}
	for _, rawTx := range block.TxData {
		var tx core.Transaction
		if json.Unmarshal(rawTx, &tx) != nil || tx.Hash() != hash {
			continue
		}
		if int(r.OutputIndex) >= len(tx.Outputs) || tx.Outputs[r.OutputIndex].AmountCommit != u.AmountCommit {
			out.Status = "canonical_output_mismatch"
			return out
		}
		out.CanonicalBody = true
		out.Status = "canonical_opening_verified_spentness_unknown"
		return out
	}
	out.Status = "opening_verified_transaction_not_in_canonical_body"
	return out
}

func run() error {
	path := flag.String("copied-db", "", "closed, consistent LevelDB COPY only")
	max := flag.Uint64("max-whole-apro", 2000000, "bounded whole-APRO search, at most 2000000")
	extraText := flag.String("extra-candidates-napro", "[]", "JSON array of decimal STRINGS; search hints, not evidence")
	flag.Parse()
	if *path == "" || *max > 2000000 {
		return fmt.Errorf("closed copy and bounded search required")
	}
	target, err := filepath.EvalSymlinks(*path)
	if err != nil {
		return err
	}
	if target == "/opt/aperod/data/testnet/chain.db" {
		return fmt.Errorf("refusing live production database")
	}
	var extraStrings []string
	if err := json.Unmarshal([]byte(*extraText), &extraStrings); err != nil || len(extraStrings) > 100 {
		return fmt.Errorf("invalid candidate list")
	}
	var extra []uint64
	for _, s := range extraStrings {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != s {
			return fmt.Errorf("invalid candidate")
		}
		extra = append(extra, n)
	}
	var rows []record
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1048577))
	if err != nil || len(data) > 1048576 {
		return fmt.Errorf("input too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil || len(rows) > 100 {
		return fmt.Errorf("invalid bounded input")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("trailing input")
	}
	d, err := store.OpenReadOnly(target)
	if err != nil {
		return err
	}
	defer d.Close()
	results := make([]evidence, 0, len(rows))
	for _, r := range rows {
		results = append(results, audit(d, r, *max, extra))
	}
	// No addresses, blind scalars, keys or input records are emitted.
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"scope":                 "read-only mint opening audit; not a global issuance or wallet spentness proof",
		"activation_authorized": false, "records": results,
	})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mint record audit:", err)
		os.Exit(1)
	}
}
