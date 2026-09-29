package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// LPoDBodyCoverageReport describes a read-only scan of canonical historical
// block bodies. CoinbaseOutputs is only a count of outputs, not an amount or
// supply proof.
type LPoDBodyCoverageReport struct {
	TipHeight       uint64  `json:"tip_height"`
	ThroughHeight   uint64  `json:"through_height"`
	CheckedBlocks   uint64  `json:"checked_blocks"`
	CoinbaseOutputs uint64  `json:"coinbase_outputs"`
	Complete        bool    `json:"complete"`
	FirstIssue      string  `json:"first_issue,omitempty"`
	IssueHeight     *uint64 `json:"issue_height,omitempty"`
	IssueKind       string  `json:"issue_kind,omitempty"`
	Notice          string  `json:"notice"`
}

const lpodAuditNotice = "This scan establishes only structural presence and linkage. It does not authenticate signatures, proofs or full-body integrity; those require an independently attested LPoDBodyRootStep plus exact issuance openings. Coverage is necessary but insufficient to activate LPoD and is not a supply proof."

// AuditLPoDBodyCoverageReadOnly scans canonical full block bodies from genesis
// through throughHeight, inclusive. The database must be an explicitly
// supplied copy; this function opens it with LevelDB ReadOnly and never repairs
// or mutates data.
func AuditLPoDBodyCoverageReadOnly(path string, throughHeight uint64) (*LPoDBodyCoverageReport, error) {
	report := &LPoDBodyCoverageReport{
		ThroughHeight: throughHeight,
		Notice:        lpodAuditNotice,
	}
	db, err := OpenLPoDAuditReadOnly(path)
	if err != nil {
		return report, err
	}
	defer db.Close()

	tipHashBytes, err := db.GetMeta("tip/hash")
	if err != nil {
		return report, fmt.Errorf("lpod audit: read tip hash: %w", err)
	}
	if len(tipHashBytes) != len(crypto.Hash32{}) {
		return report, fmt.Errorf("lpod audit: tip hash metadata malformed: got %d bytes", len(tipHashBytes))
	}
	var tipHash crypto.Hash32
	copy(tipHash[:], tipHashBytes)
	if tipHash == (crypto.Hash32{}) {
		return report, fmt.Errorf("lpod audit: tip hash metadata is zero")
	}
	tipHeightBytes, err := db.GetMeta("tip/height")
	if err != nil {
		return report, fmt.Errorf("lpod audit: read tip height: %w", err)
	}
	if len(tipHeightBytes) != 8 {
		return report, fmt.Errorf("lpod audit: tip height metadata malformed: got %d bytes", len(tipHeightBytes))
	}
	report.TipHeight = binary.LittleEndian.Uint64(tipHeightBytes)
	if throughHeight > report.TipHeight {
		return report, fmt.Errorf("lpod audit: through-height %d exceeds tip height %d", throughHeight, report.TipHeight)
	}
	indexedTip, found, err := db.GetCanonicalHash(report.TipHeight)
	if err != nil {
		return report, fmt.Errorf("lpod audit: read tip height index: %w", err)
	}
	if !found || indexedTip != tipHash {
		return report, fmt.Errorf("lpod audit: tip metadata does not match canonical height index at %d", report.TipHeight)
	}

	var parent crypto.Hash32
	for height := uint64(0); ; height++ {
		indexedHash, found, indexErr := db.GetCanonicalHash(height)
		if indexErr != nil {
			return lpodAuditIssue(report, height, "noncanonical", fmt.Sprintf("height index read failed: %v", indexErr))
		}
		if !found {
			return lpodAuditIssue(report, height, "missing", "canonical height index entry is missing")
		}

		raw, readErr := db.GetRawBlock(indexedHash)
		if readErr != nil {
			return lpodAuditIssue(report, height, "missing", fmt.Sprintf("block body read failed: %v", readErr))
		}
		if raw == nil {
			return lpodAuditIssue(report, height, "missing-or-pruned", "full block body is missing")
		}

		if isPrunedStoredBlock(raw) {
			return lpodAuditIssue(report, height, "pruned", "stored block has no full transaction body")
		}
		block, unmarshalErr := decodeLPoDAuditBlock(raw)
		if unmarshalErr != nil {
			return lpodAuditIssue(report, height, "noncanonical", fmt.Sprintf("body is not a full canonical block: %v", unmarshalErr))
		}
		if block.Header.Height != height {
			return lpodAuditIssue(report, height, "noncanonical", fmt.Sprintf("header height is %d", block.Header.Height))
		}
		if indexedHash != block.Hash() {
			return lpodAuditIssue(report, height, "noncanonical", "height index hash does not match full block header hash")
		}
		if block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			return lpodAuditIssue(report, height, "noncanonical", "header merkle root does not match transaction body")
		}
		if height > 0 && block.Header.PrevHash != parent {
			return lpodAuditIssue(report, height, "noncanonical", "header parent hash does not match previous canonical block")
		}

		parent = block.Hash()
		report.CheckedBlocks++
		for ti := range block.Txs {
			tx := &block.Txs[ti]
			if !tx.IsCoinbase() {
				continue
			}
			if uint64(len(tx.Outputs)) > ^uint64(0)-report.CoinbaseOutputs {
				return lpodAuditIssue(report, height, "noncanonical", "coinbase output count overflow")
			}
			report.CoinbaseOutputs += uint64(len(tx.Outputs))
		}
		if height == throughHeight {
			break
		}
	}
	report.Complete = true
	return report, nil
}

func lpodAuditIssue(report *LPoDBodyCoverageReport, height uint64, kind, detail string) (*LPoDBodyCoverageReport, error) {
	report.IssueHeight = &height
	report.IssueKind = kind
	report.FirstIssue = detail
	return report, fmt.Errorf("lpod audit: %s at height %d: %s", kind, height, detail)
}

func isPrunedStoredBlock(raw []byte) bool {
	// core.Block has no JSON tags, so encoding/json emits "Header" and "Txs".
	// PruneBlocksOlderThan writes the distinct StoredBlock form whose first
	// field is tagged "height". Check that short prefix before decoding: building
	// a map for every full block would needlessly decode millions of bodies twice.
	raw = bytes.TrimSpace(raw)
	const firstPrunedField = `{"height"`
	if !bytes.HasPrefix(raw, []byte(firstPrunedField)) {
		return false
	}
	if !bytes.Contains(raw, []byte(`"tx_count"`)) {
		return false
	}
	for i := len(firstPrunedField); i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ':':
			return true
		default:
			return false
		}
	}
	return false
}

// decodeLPoDAuditBlock decodes the full body once while requiring the exact
// top-level fields emitted by json.Marshal(core.Block). In particular, an
// omitted Txs field must not be accepted as an empty transaction list.
func decodeLPoDAuditBlock(raw []byte) (*core.Block, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := first.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("block JSON must be an object")
	}

	block := &core.Block{}
	hasHeader, hasTxs := false, false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		field, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("block JSON object key is not a string")
		}
		switch field {
		case "Header":
			if hasHeader {
				return nil, fmt.Errorf("duplicate Header field")
			}
			var header json.RawMessage
			if err := decoder.Decode(&header); err != nil {
				return nil, err
			}
			if len(bytes.TrimSpace(header)) == 0 || bytes.TrimSpace(header)[0] != '{' {
				return nil, fmt.Errorf("Header field must be an object")
			}
			if err := json.Unmarshal(header, &block.Header); err != nil {
				return nil, err
			}
			hasHeader = true
		case "Txs":
			if hasTxs {
				return nil, fmt.Errorf("duplicate Txs field")
			}
			if err := decoder.Decode(&block.Txs); err != nil {
				return nil, err
			}
			hasTxs = true
		default:
			return nil, fmt.Errorf("unexpected top-level block field %q", field)
		}
	}
	last, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := last.(json.Delim); !ok || delimiter != '}' {
		return nil, fmt.Errorf("block JSON object was not closed")
	}
	if !hasHeader || !hasTxs {
		return nil, fmt.Errorf("block JSON must explicitly contain Header and Txs fields")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON after block object")
		}
		return nil, err
	}
	return block, nil
}
