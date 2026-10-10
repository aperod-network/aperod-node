package main

import (
 "compress/gzip"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "io"
 "log/slog"
 "os"

 "github.com/aperod/aperod/config"
 "github.com/aperod/aperod/core"
 "github.com/aperod/aperod/crypto"
 "github.com/aperod/aperod/store"
)

const bootstrapMaxCompressedBytes int64 = 512 << 20
const bootstrapMaxExpandedBytes int64 = 2 << 30
// Historical snapshots include millions of key images. The expanded-byte
// ceiling is independent of (and usually tighter than) this entry ceiling.
const bootstrapMaxArrayEntries = 8_000_000

// This opt-in pins the entire operator-selected state, not merely a list of
// signers. Neither a witness signature nor the DB index authenticates that
// operator's historical committee assertion.
func loadOperatorRegistryBootstrap(c *config.RegistryBootstrapConfig, nonValidator bool, db *store.DB, tip uint64) (*startupSnapshot, error) {
 if c == nil { return nil, nil }
 if !nonValidator || c.Trust != "operator_attested" || c.Height == 0 || c.Height > tip {
  return nil, fmt.Errorf("registry bootstrap requires non-validator mode, operator_attested trust, and a nonzero height at or below tip")
 }
 digest, err := hex.DecodeString(c.SHA256)
 if err != nil || len(digest) != sha256.Size { return nil, fmt.Errorf("registry bootstrap requires an exact snapshot SHA-256") }
 f, err := os.Open(c.SnapshotFile)
 if err != nil { return nil, fmt.Errorf("registry bootstrap: %w", err) }
 h := sha256.New()
 defer f.Close()
info, err := f.Stat()
if err != nil || !info.Mode().IsRegular() || info.Size() > bootstrapMaxCompressedBytes {
return nil, fmt.Errorf("registry bootstrap requires a regular compressed snapshot <=512 MiB")
}
// Authenticate compressed bytes BEFORE spending memory/CPU on decompression.
if _, err := io.Copy(h, io.LimitReader(f, bootstrapMaxCompressedBytes+1)); err != nil {
return nil, err
}
if hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(digest) {
return nil, fmt.Errorf("registry bootstrap snapshot digest mismatch before decompression")
}
if _, err := f.Seek(0, io.SeekStart); err != nil { return nil, err }
h.Reset()
 reader := io.TeeReader(io.LimitReader(f, bootstrapMaxCompressedBytes+1), h)
 gz, err := gzip.NewReader(reader)
 if err != nil { return nil, fmt.Errorf("registry bootstrap gzip: %w", err) }
 defer gz.Close()
 snap := new(startupSnapshot)
 expanded := &io.LimitedReader{R:gz, N:bootstrapMaxExpandedBytes+1}
 decoder := json.NewDecoder(expanded)
 if err := decodeBootstrapSnapshot(decoder, snap); err != nil { return nil, fmt.Errorf("registry bootstrap snapshot decode: %w", err) }
 if err := decoder.Decode(new(any)); err != io.EOF { return nil, fmt.Errorf("registry bootstrap snapshot trailing data or checksum failure") }
if expanded.N <= 0 { return nil, fmt.Errorf("registry bootstrap expanded snapshot exceeds 2 GiB") }
 if _, err := io.Copy(io.Discard, reader); err != nil { return nil, err }
 if hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(digest) { return nil, fmt.Errorf("registry bootstrap snapshot digest mismatch") }
 if snap.Version != snapVersion || snap.TipHeight != c.Height { return nil, fmt.Errorf("registry bootstrap snapshot version/height mismatch") }
 hash, found, err := db.GetCanonicalHash(c.Height)
 if err != nil || !found || snap.TipHashHex != fmt.Sprintf("%x", hash[:]) {
  return nil, fmt.Errorf("registry bootstrap snapshot does not match canonical index at height %d", c.Height)
 }
 if err := validateBootstrapRegistry(snap.Registry); err != nil { return nil, err }
if err := validateBootstrapUTXOs(snap); err != nil { return nil, err }
if c.KeyImageFile!="" || c.KeyImageSHA256!="" {
images,err:=loadBootstrapKeyImageAnchor(c.KeyImageFile,c.KeyImageSHA256,snap.TipHeight,snap.TipHashHex)
if err!=nil { return nil,err }
if len(snap.UTXOs.KeyImages)!=0 {
if len(images)!=len(snap.UTXOs.KeyImages) { return nil,fmt.Errorf("snapshot and key-image anchor disagree") }
set:=make(map[crypto.KeyImage]bool,len(images))
for _,image:=range images { set[image]=true }
for _,image:=range snap.UTXOs.KeyImages { if !set[image] { return nil,fmt.Errorf("snapshot and key-image anchor disagree") } }
}
snap.UTXOs.KeyImages=images
}
// Count metadata is only an anchor-height hint. Never compare the old snapshot
// to tip-height counts. Final replay state is checked against actual tip rows.
count, hasCount, err := db.LoadActiveUTXOCount(snap.TipHashHex)
if err != nil || (hasCount && count != len(snap.UTXOs.ActiveUTXOs)) {
return nil, fmt.Errorf("registry bootstrap anchor-height UTXO count metadata mismatch or read failure")
}
 return snap, nil
}

func validateBootstrapRegistry(snap core.RegistrySnapshot) error {
 for key, entry := range snap.Validators {
  if entry == nil || key != hex.EncodeToString(entry.PubKey) || len(entry.PubKey) != 32 || isZeroLPoDAuthority(entry.PubKey) || entry.Seeded {
   return fmt.Errorf("registry bootstrap contains malformed or observed-producer-seeded membership")
  }
 }
 registry := core.NewValidatorRegistry()
 registry.RestoreFromSnapshot(snap)
 return guardLPoDConsensusAuthorities(registry.GetActiveValidators())
}

// Replay is relative to the operator's pinned state. It proves continuity under
// that assumption, not pre-anchor issuance or independently certified finality.
func replayOperatorRegistryBootstrap(db *store.DB, utxos *core.UTXOSet, registry *core.ValidatorRegistry, snap *startupSnapshot, tip uint64, log *slog.Logger) error {
capturedHash,capturedHeight,err:=db.GetTip()
if err!=nil || capturedHeight!=tip || capturedHash==(crypto.Hash32{}) {
return fmt.Errorf("registry bootstrap initial durable tip unavailable or height changed: %v",err)
}
if err:=requireBootstrapTipUnchanged(db,capturedHash,capturedHeight);err!=nil { return err }
return utxos.WithAnchoredReplayKeyImages(snap.UTXOs.KeyImages, func() error {
 previous, found, err := db.GetCanonicalHash(snap.TipHeight)
 if err != nil || !found { return fmt.Errorf("registry bootstrap anchor index unavailable") }
 for height := snap.TipHeight+1; height <= tip; height++ {
  raw, err := db.GetRawBlockByHeight(height)
  if err != nil || raw == nil { return fmt.Errorf("registry bootstrap missing block %d", height) }
  var block core.Block
  if err := json.Unmarshal(raw, &block); err != nil { return fmt.Errorf("registry bootstrap decode block %d: %w", height, err) }
  index, found, err := db.GetCanonicalHash(height)
  if err != nil || !found || block.Header.Height != height || block.Hash() != index || block.Header.PrevHash != previous ||
   !block.Header.VerifySignature() || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
   return fmt.Errorf("registry bootstrap block %d fails canonical continuity/signature/Merkle checks", height)
  }
  if !bootstrapProposerAt(registry.GetActiveValidators(), block.Header.ValidatorPub, block.Header.Round) {
   return fmt.Errorf("registry bootstrap block %d proposer is not the restored committee round-slot leader", height)
  }
// Stake replay's historical helper omits collateral checks. This restricted
// operator mode does not claim to validate stake changes: reject ALL of them.
for _, tx := range block.Txs {
if tx.IsStake() { return fmt.Errorf("registry bootstrap unsupported post-anchor stake transaction at %d", height) }
}
if err := utxos.ApplyBlock(&block); err != nil { return fmt.Errorf("registry bootstrap state replay at %d: %w", height, err) }
if height%core.EpochLength == 0 { registry.UpdateEpoch(height) }
  previous = index
 }
if err := guardLPoDConsensusAuthorities(registry.GetActiveValidators()); err != nil { return err }
if previous!=capturedHash { return fmt.Errorf("registry bootstrap replay endpoint differs from captured durable tip") }
if err := validateBootstrapTip(db, utxos, snap, log); err != nil { return err }
if err:=requireBootstrapTipUnchanged(db,capturedHash,capturedHeight);err!=nil { return err }
log.Info("operator-attested replay complete; key-image set matches durable tip; active-state completeness derives from pinned anchor and replay, not legacy disk indexes", "tip_height", tip,"active_utxos",utxos.Count())
return nil
})
}

func requireBootstrapTipUnchanged(db *store.DB, hash crypto.Hash32, height uint64) error {
current,currentHeight,err:=db.GetTip()
if err!=nil { return fmt.Errorf("registry bootstrap read durable tip: %w",err) }
if current!=hash || currentHeight!=height { return fmt.Errorf("registry bootstrap durable tip changed during replay") }
indexed,found,err:=db.GetCanonicalHash(height)
if err!=nil { return fmt.Errorf("registry bootstrap read tip index: %w",err) }
if !found || indexed!=hash { return fmt.Errorf("registry bootstrap captured tip no longer matches canonical index") }
return nil
}

func bootstrapProposerAt(active []crypto.ValidatorPubKey, proposer crypto.ValidatorPubKey, round uint32) bool {
// GetActiveValidators supplies the same deterministic ordering as live engine.
return len(active)>0 && active[int(round)%len(active)].Equals(proposer)
}

// Decode large UTXO arrays one entry at a time. A single Decode of the entire
// snapshot buffers gigabytes of JSON in addition to the resulting state.
func decodeBootstrapObject(d *json.Decoder, field func(string) error) error {
 token, err := d.Token()
 if err != nil { return err }
 if token != json.Delim('{') { return fmt.Errorf("expected snapshot object") }
 seen := make(map[string]bool)
 for d.More() {
if len(seen) >= 4096 { return fmt.Errorf("snapshot object exceeds field bound") }
  token, err := d.Token()
  if err != nil { return err }
  key, ok := token.(string)
  if !ok || seen[key] { return fmt.Errorf("invalid or duplicate snapshot field") }
  seen[key] = true
  if err := field(key); err != nil { return err }
 }
 _, err = d.Token()
 return err
}

func decodeBootstrapArray[T any](d *json.Decoder, dst *[]T) error {
return decodeBootstrapArrayBounded(d,dst,bootstrapMaxArrayEntries)
}

func decodeBootstrapArrayBounded[T any](d *json.Decoder, dst *[]T, limit int) error {
 token, err := d.Token()
 if err != nil { return err }
 if token == nil { return nil }
 if token != json.Delim('[') { return fmt.Errorf("expected snapshot array") }
 for d.More() {
if len(*dst) >= limit { return fmt.Errorf("snapshot array exceeds entry bound") }
  var entry T
  if err := d.Decode(&entry); err != nil { return err }
  *dst = append(*dst, entry)
 }
 _, err = d.Token()
 return err
}

func decodeBootstrapSnapshot(d *json.Decoder, snap *startupSnapshot) error {
seen := make(map[string]bool)
err := decodeBootstrapObject(d, func(key string) error {
seen[key] = true
  switch key {
  case "v": return d.Decode(&snap.Version)
  case "tip_height": return d.Decode(&snap.TipHeight)
  case "tip_hash": return d.Decode(&snap.TipHashHex)
  case "tx_total": return d.Decode(&snap.TxTotal)
  case "saved_at": return d.Decode(&snap.SavedAt)
  case "registry": return decodeBootstrapRegistry(d, &snap.Registry)
  case "utxos":
   return decodeBootstrapUTXOObject(d, snap)
  default: return fmt.Errorf("unknown snapshot field %q", key)
  }
 })
if err != nil { return err }
for _, key := range []string{"v","tip_height","tip_hash","utxos","registry"} {
if !seen[key] { return fmt.Errorf("missing required snapshot field %q",key) }
}
return nil
}

func decodeBootstrapRegistry(d *json.Decoder, registry *core.RegistrySnapshot) error {
seen:=make(map[string]bool)
err:=decodeBootstrapObject(d,func(key string) error {
seen[key]=true
switch key {
case "dynamic_min_napr": return d.Decode(&registry.DynamicMinNAPR)
case "validators":
registry.Validators=make(map[string]*core.ValidatorEntry)
return decodeBootstrapObject(d,func(key string) error {
var entry *core.ValidatorEntry
if err:=d.Decode(&entry); err!=nil { return err }
registry.Validators[key]=entry
return nil
})
default:return fmt.Errorf("unknown registry field %q",key)
}
})
if err!=nil { return err }
if !seen["validators"] || !seen["dynamic_min_napr"] { return fmt.Errorf("missing required registry fields") }
return nil
}

func decodeBootstrapUTXOObject(d *json.Decoder, snap *startupSnapshot) error {
seen := make(map[string]bool)
err := decodeBootstrapObject(d, func(key string) error {
seen[key] = true
    switch key {
    case "active_utxos": return decodeBootstrapArray(d, &snap.UTXOs.ActiveUTXOs)
    case "staked_utxos": return decodeBootstrapArray(d, &snap.UTXOs.StakedUTXOs)
    case "spent_decoys": return decodeBootstrapArray(d, &snap.UTXOs.SpentDecoys)
    case "key_images": return decodeBootstrapArray(d, &snap.UTXOs.KeyImages)
    case "rollback_journal": return decodeBootstrapArray(d, &snap.UTXOs.RollbackJournal)
    default: return fmt.Errorf("unknown snapshot UTXO field %q", key)
    }
   })
if err != nil { return err }
for _, key := range []string{"active_utxos","staked_utxos","spent_decoys","key_images"} {
if !seen[key] { return fmt.Errorf("missing required UTXO field %q",key) }
}
return nil
}

func validateBootstrapUTXOs(snap *startupSnapshot) error {
seen := make(map[core.UTXOKey]bool)
for _, list := range [][]*core.UTXO{snap.UTXOs.ActiveUTXOs, snap.UTXOs.StakedUTXOs, snap.UTXOs.SpentDecoys} {
for _, u := range list {
if u == nil || u.BlockHeight > snap.TipHeight { return fmt.Errorf("snapshot contains null or future UTXO") }
key := core.UTXOKey{TxHash:u.TxHash, OutputIndex:u.OutputIndex}
if seen[key] { return fmt.Errorf("snapshot contains duplicate UTXO identity") }
seen[key] = true
}
}
for _, entry := range snap.UTXOs.RollbackJournal {
if entry.UTXO == nil || entry.Height > snap.TipHeight || entry.UTXO.BlockHeight > entry.Height { return fmt.Errorf("snapshot contains invalid rollback entry") }
}
return nil
}

// u/ is neither a complete nor an active-only index on legacy archives. Existing
// rows must match exactly. A missing PRE-anchor row requires exact equality to
// a still-active entry in the pinned snapshot; missing POST-anchor rows fail.
// Completeness is the operator-attested anchor + checked replay, not a claim
// about u/ or su/ cardinality. No tolerance or repair is applied to conflicts.
type bootstrapTipStore interface {
IterUTXOs(func(*store.StoredUTXO) error) error
IsUTXOSpentChecked(crypto.Hash32,uint32) (bool,error)
IterKeyImages(func(crypto.KeyImage) error) error
}

func validateBootstrapTip(db bootstrapTipStore, utxos *core.UTXOSet, snap *startupSnapshot, log *slog.Logger) error {
count := 0
matched:=make(map[core.UTXOKey]bool)
err := db.IterUTXOs(func(stored *store.StoredUTXO) error {
u := utxos.Get(stored.TxHash, stored.OutputIndex)
if u==nil { return nil } // historical retained row, NOT an active-set claim
spent,err:=db.IsUTXOSpentChecked(stored.TxHash,stored.OutputIndex)
if err!=nil { return fmt.Errorf("replayed active UTXO spent-marker lookup: %w",err) }
if spent { return fmt.Errorf("replayed active UTXO conflicts with durable spent marker") }
if u.OneTimePub != stored.OneTimePub || u.TxPubKey != stored.TxPubKey ||
u.AmountCommit != stored.AmountCommit || u.EncAmount != stored.EncAmount ||
u.BlockHeight != stored.BlockHeight || u.ProtocolLocked != stored.ProtocolLocked {
return fmt.Errorf("registry bootstrap replayed active UTXO differs from durable tip: tx=%x index=%d stored_height=%d absent_from_replay=%t",stored.TxHash,stored.OutputIndex,stored.BlockHeight,u==nil)
}
count++
key:=core.UTXOKey{TxHash:stored.TxHash,OutputIndex:stored.OutputIndex}
if matched[key] { return fmt.Errorf("duplicate durable UTXO identity") }
matched[key]=true
return nil
})
if err != nil { return err }
anchorOnly:=0
for _,original:=range snap.UTXOs.ActiveUTXOs {
key:=core.UTXOKey{TxHash:original.TxHash,OutputIndex:original.OutputIndex}
if matched[key] { continue }
current:=utxos.Get(original.TxHash,original.OutputIndex)
if current==nil { continue } // consumed by a checked post-anchor block
if original.BlockHeight>snap.TipHeight || *original!=*current {
return fmt.Errorf("missing durable UTXO is not identical to pinned pre-anchor state")
}
spent,err:=db.IsUTXOSpentChecked(original.TxHash,original.OutputIndex)
if err!=nil { return fmt.Errorf("pinned active UTXO spent-marker lookup: %w",err) }
if spent { return fmt.Errorf("pinned active UTXO conflicts with durable spent marker") }
anchorOnly++
}
if count+anchorOnly!=utxos.Count() { return fmt.Errorf("replayed active state contains outputs backed by neither durable rows nor the pinned anchor") }
log.Warn("operator-attested UTXO validation: legacy disk index is not an independent active-set count",
"active_count",utxos.Count(),"durable_rows_matched",count,"pinned_anchor_only",anchorOnly)
keyImages := 0
if err := db.IterKeyImages(func(image crypto.KeyImage) error {
if !utxos.IsSpent(image) { return fmt.Errorf("registry bootstrap durable tip key image absent from anchor+replay") }
keyImages++
return nil
}); err != nil { return err }
if keyImages != utxos.KeyImagesCount() { return fmt.Errorf("registry bootstrap anchor+replay key-image count differs from durable tip") }
return nil
}