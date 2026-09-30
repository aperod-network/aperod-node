package main

import (
 "compress/gzip"
 "io"
 "encoding/json"
 "strings"
 "crypto/sha256"
 "fmt"
 "os"
 "testing"
 "path/filepath"
 "time"
 "log/slog"

 "github.com/aperod/aperod/config"
 "github.com/aperod/aperod/core"
 "github.com/aperod/aperod/crypto"
 "github.com/aperod/aperod/store"
)

func TestBootstrapDatabaseBackedSpendReplay(t *testing.T) {
for _, variant := range []string{"tip-spend", "already-spent-at-anchor", "duplicate-gap-spend"} {
t.Run(variant, func(t *testing.T) {
db, err := store.Open(t.TempDir()); if err != nil { t.Fatal(err) }; defer db.Close()
priv,pub,err := crypto.GenerateValidatorKey(); if err != nil { t.Fatal(err) }
owner,err := crypto.GenerateWalletKeys(); if err != nil { t.Fatal(err) }
ring := make([]crypto.RingMember,crypto.RingSize)
ring[0]=owner.Spend.Public
for i:=1;i<len(ring);i++ { decoy,err:=crypto.GenerateWalletKeys(); if err!=nil { t.Fatal(err) }; ring[i]=decoy.Spend.Public }
sig,err := crypto.MLSAGSign(crypto.Hash32{},ring,0,owner.Spend.Private); if err != nil { t.Fatal(err) }
commit,err := crypto.Commit(1000,crypto.BlindFactor{}); if err != nil { t.Fatal(err) }
source := &core.UTXO{TxHash:crypto.HashStr("anchor-source"),OneTimePub:owner.Spend.Public,AmountCommit:commit,BlockHeight:1}
if err := db.PutUTXO(source.TxHash,0,&store.StoredUTXO{TxHash:source.TxHash,OneTimePub:source.OneTimePub,AmountCommit:source.AmountCommit,BlockHeight:1}); err != nil { t.Fatal(err) }
if err := db.MarkUTXOSpent(source.TxHash,0); err != nil { t.Fatal(err) }
if err := db.MarkKeyImageSpent(sig.KeyImage); err != nil { t.Fatal(err) }
anchor := makeSignedBlk(t,1,crypto.Hash32{},priv,pub); putRawBlk(t,db,anchor)
next := makeSignedBlk(t,2,anchor.Hash(),priv,pub)
next.Txs = []core.Transaction{{Inputs:[]core.RingInput{{KeyImage:sig.KeyImage,Ring:ring,AmountCommit:commit}}}}
next.Header.MerkleRoot = core.MerkleRoot(next.Txs)
if err := next.Header.Sign(priv); err != nil { t.Fatal(err) }; putRawBlk(t,db,next)
tip := uint64(2)
if variant == "duplicate-gap-spend" {
repeat := makeSignedBlk(t,3,next.Hash(),priv,pub); repeat.Txs = next.Txs
repeat.Header.MerkleRoot = core.MerkleRoot(repeat.Txs)
if err := repeat.Header.Sign(priv); err != nil { t.Fatal(err) }; putRawBlk(t,db,repeat); tip=3
}
snap := &startupSnapshot{TipHeight:1,UTXOs:core.UTXOSnapshot{ActiveUTXOs:[]*core.UTXO{source}}}
if variant == "already-spent-at-anchor" { snap.UTXOs.KeyImages=[]crypto.KeyImage{sig.KeyImage} }
utxos:=core.NewUTXOSetWithDB(db); utxos.RestoreFromSnapshot(snap.UTXOs)
callbacks:=0; utxos.OnUTXOSpent=func(crypto.Hash32,uint32){ callbacks++ }
registry:=core.NewValidatorRegistry(); registry.InitFromGenesis([]crypto.ValidatorPubKey{pub},core.MinStakeNAPR*10)
tipHash,found,err:=db.GetCanonicalHash(tip);if err!=nil || !found { t.Fatal("missing fixture tip") }
if err:=db.PutTip(tipHash,tip);err!=nil { t.Fatal(err) }
err=replayOperatorRegistryBootstrap(db,utxos,registry,snap,tip,silentLog())
if variant=="tip-spend" {
if err!=nil { t.Fatal(err) }; if utxos.Count()!=0 { t.Fatal("spend did not consume output") }
} else if err==nil { t.Fatal("duplicate spend accepted") }
if callbacks!=0 { t.Fatal("replay invoked persistent callback") }
if !utxos.IsSpent(sig.KeyImage) { t.Fatal("durable tip backend was not restored") }
spent,err:=db.IsKeyImageSpent(sig.KeyImage); if err!=nil || !spent { t.Fatal("durable key image altered") }
})
}
}

func TestBootstrapWrongRoundSlot(t *testing.T) {
db,err:=store.Open(t.TempDir()); if err!=nil { t.Fatal(err) }; defer db.Close()
priv1,pub1,err:=crypto.GenerateValidatorKey(); if err!=nil { t.Fatal(err) }
priv2,pub2,err:=crypto.GenerateValidatorKey(); if err!=nil { t.Fatal(err) }
registry:=core.NewValidatorRegistry(); registry.InitFromGenesis([]crypto.ValidatorPubKey{pub1,pub2},core.MinStakeNAPR*10)
active:=registry.GetActiveValidators()
wrongPriv,wrongPub:=priv1,pub1
if active[0].Equals(pub1) { wrongPriv,wrongPub=priv2,pub2 }
anchor:=makeSignedBlk(t,1,crypto.Hash32{},priv1,pub1); putRawBlk(t,db,anchor)
block:=makeSignedBlk(t,2,anchor.Hash(),wrongPriv,wrongPub); block.Header.Round=0
if err:=block.Header.Sign(wrongPriv); err!=nil { t.Fatal(err) }; putRawBlk(t,db,block)
if err:=db.PutTip(block.Hash(),2);err!=nil { t.Fatal(err) }
if err:=replayOperatorRegistryBootstrap(db,core.NewUTXOSet(),registry,&startupSnapshot{TipHeight:1},2,silentLog()); err==nil { t.Fatal("valid member at wrong round slot accepted") }
}

func TestBootstrapAuthenticationBeforeDecompressionAndNullEntries(t *testing.T) {
db,err:=store.Open(t.TempDir()); if err!=nil { t.Fatal(err) }; defer db.Close()
path:=filepath.Join(t.TempDir(),"bad.gz")
if err:=os.WriteFile(path,[]byte("not gzip"),0600); err!=nil { t.Fatal(err) }
c:=&config.RegistryBootstrapConfig{Trust:"operator_attested",SnapshotFile:path,SHA256:fmt.Sprintf("%064x",0),Height:1}
if _,err:=loadOperatorRegistryBootstrap(c,true,db,1); err==nil || !strings.Contains(err.Error(),"before decompression") { t.Fatalf("decompressed before authenticating: %v",err) }
for _,state:=range []core.UTXOSnapshot{
{ActiveUTXOs:[]*core.UTXO{nil}}, {StakedUTXOs:[]*core.UTXO{nil}}, {SpentDecoys:[]*core.UTXO{nil}},
{RollbackJournal:[]core.RollbackJournalEntry{{Height:1}}},
} {
if err:=validateBootstrapUTXOs(&startupSnapshot{TipHeight:1,UTXOs:state}); err==nil { t.Fatal("null entry accepted") }
}
}

// Optional real-archive test: read-only DB, no node, no signing or store writes.
func TestBootstrapRealArchiveReadOnly(t *testing.T) {
started:=time.Now()
path:=os.Getenv("LPOD_REAL_ARCHIVE_DB")
if path=="" { t.Skip("set LPOD_REAL_ARCHIVE_DB and LPOD_REAL_ANCHOR_SNAPSHOT for explicit offline replay") }
snapshot:=os.Getenv("LPOD_REAL_ANCHOR_SNAPSHOT")
db,err:=store.OpenReadOnly(path); if err!=nil { t.Fatal(err) }; defer db.Close()
_,tip,err:=db.GetTip(); if err!=nil { t.Fatal(err) }

kiCount:=0
if err:=db.IterKeyImages(func(crypto.KeyImage)error{kiCount++;return nil}); err!=nil { t.Fatal(err) }
t.Logf("durable tip=%d key_images=%d",tip,kiCount)
c:=&config.RegistryBootstrapConfig{Trust:"operator_attested",SnapshotFile:snapshot,SHA256:"2e377c4e72a0d45b0c5be15c65f89ec5f317dee9636427213e9c2fbe20d62380",Height:2492066}
c.KeyImageFile=os.Getenv("LPOD_REAL_KEY_IMAGE_ANCHOR")
c.KeyImageSHA256=os.Getenv("LPOD_REAL_KEY_IMAGE_SHA256")
snap,err:=loadOperatorRegistryBootstrap(c,true,db,tip); if err!=nil { t.Fatal(err) }
t.Logf("authenticated operator-selected snapshot: height=%d active=%d staked=%d decoys=%d key_images=%d tip=%d",snap.TipHeight,len(snap.UTXOs.ActiveUTXOs),len(snap.UTXOs.StakedUTXOs),len(snap.UTXOs.SpentDecoys),len(snap.UTXOs.KeyImages),tip)
t.Logf("snapshot elapsed=%s",time.Since(started))
hash,_,err:=db.GetTip(); if err!=nil { t.Fatal(err) }
cached,present,err:=db.LoadActiveUTXOCount(fmt.Sprintf("%x",hash))
t.Logf("tip active-count metadata: present=%t count=%d error=%v",present,cached,err)
registry:=core.NewValidatorRegistry(); registry.RestoreFromSnapshot(snap.Registry)
utxos:=core.NewUTXOSetWithDB(db); utxos.RestoreFromSnapshot(snap.UTXOs); registry.SetUTXOSet(utxos)
if err:=replayOperatorRegistryBootstrap(db,utxos,registry,snap,tip,slog.Default()); err!=nil { t.Fatal(err) }
t.Logf("real operator-attested replay PASS: blocks=%d tip=%d active_utxos=%d elapsed=%s",tip-snap.TipHeight,tip,utxos.Count(),time.Since(started))
}

func TestBootstrapCopiedDBInventory(t *testing.T) {
paths:=os.Getenv("LPOD_BOOTSTRAP_INVENTORY")
if paths=="" { t.Skip("explicit copied databases only") }
for _,path:=range strings.Split(paths,":") {
db,err:=store.OpenReadOnly(path)
if err!=nil { t.Logf("%s: %v",path,err);continue }
hash,height,err:=db.GetTip()
count:=0
kiErr:=db.IterKeyImages(func(crypto.KeyImage)error{count++;return nil})
t.Logf("%s tip=%d hash=%x key_images=%d errors=%v/%v",path,height,hash,count,err,kiErr)
db.Close()
}
}

func TestBootstrapUTXOValidationTrustBoundary(t *testing.T) {
for _,variant:=range []string{"pinned-missing-row","unanchored-missing-row","conflicting-row","spent-marker"} {
t.Run(variant,func(t *testing.T){
db,err:=store.Open(t.TempDir());if err!=nil { t.Fatal(err) };defer db.Close()
original:=&core.UTXO{TxHash:crypto.HashStr("pinned-output"),BlockHeight:1}
snap:=&startupSnapshot{TipHeight:1,UTXOs:core.UTXOSnapshot{ActiveUTXOs:[]*core.UTXO{original}}}
utxos:=core.NewUTXOSet();utxos.RestoreFromSnapshot(snap.UTXOs)
switch variant {
case "unanchored-missing-row":snap.UTXOs.ActiveUTXOs=nil
case "conflicting-row":
if err:=db.PutUTXO(original.TxHash,0,&store.StoredUTXO{TxHash:original.TxHash,BlockHeight:2});err!=nil { t.Fatal(err) }
case "spent-marker":
if err:=db.MarkUTXOSpent(original.TxHash,0);err!=nil { t.Fatal(err) }
}
err=validateBootstrapTip(db,utxos,snap,silentLog())
if variant=="pinned-missing-row" { if err!=nil { t.Fatal(err) } } else if err==nil { t.Fatal("conflicting or unanchored output accepted") }
})
}
}

func TestBootstrapExpandedLimit(t *testing.T) {
// Exercise the same limit-reader failure with a small budget, not a GB fixture.
var encoded strings.Builder
w:=gzip.NewWriter(&encoded)
if _,err:=w.Write([]byte(strings.Repeat(" ",4096))); err!=nil { t.Fatal(err) }
if err:=w.Close(); err!=nil { t.Fatal(err) }
r,err:=gzip.NewReader(strings.NewReader(encoded.String())); if err!=nil { t.Fatal(err) }; defer r.Close()
limited:=&io.LimitedReader{R:r,N:100}
if err:=json.NewDecoder(limited).Decode(new(startupSnapshot)); err==nil || limited.N!=0 { t.Fatal("expanded bound not enforced") }
var entries []*core.UTXO
if err:=decodeBootstrapArrayBounded(json.NewDecoder(strings.NewReader("[null,null,null]")),&entries,2);err==nil || len(entries)!=2 { t.Fatal("array entry bound not enforced") }
}

func TestBootstrapSnapshotDecoderRejectsAmbiguousInput(t *testing.T) {
 for _, raw := range []string{
  `{}`, `{"v":2,"tip_height":1,"tip_hash":"x","registry":{},"utxos":{}}`,
  `{"v":2,"v":2}`, `{"utxos":{"active_utxos":[],"active_utxos":[]}}`,
  `{"unexpected":true}`, `{"utxos":{"unknown":[]}}`,
  `{"utxos":{"active_utxos":{}}}`, `{"v":`,
 } {
  if err := decodeBootstrapSnapshot(json.NewDecoder(strings.NewReader(raw)), new(startupSnapshot)); err == nil { t.Fatalf("accepted %s", raw) }
 }
}

func TestOperatorRegistryBootstrapPinsStateAndTrust(t *testing.T) {
 db, err := store.Open(t.TempDir())
 if err != nil { t.Fatal(err) }
 defer db.Close()
 priv, pub, err := crypto.GenerateValidatorKey()
 if err != nil { t.Fatal(err) }
 block := makeSignedBlk(t, 1, crypto.Hash32{}, priv, pub)
 putRawBlk(t, db, block)
 registry := core.NewValidatorRegistry()
 registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
 dir := t.TempDir()
 snap := startupSnapshot{Version:snapVersion, TipHeight:1, TipHashHex:fmt.Sprintf("%x", block.Hash()), Registry:registry.TakeSnapshot()}
 if err := saveStartupSnapshot(dir, snap); err != nil { t.Fatal(err) }
 path := snapshotPath(dir, 1)
 raw, err := os.ReadFile(path)
 if err != nil { t.Fatal(err) }
 c := config.RegistryBootstrapConfig{Trust:"operator_attested", SnapshotFile:path, SHA256:fmt.Sprintf("%x", sha256.Sum256(raw)), Height:1}
 if _, err := loadOperatorRegistryBootstrap(&c, true, db, 1); err != nil { t.Fatal(err) }
 for _, name := range []string{"validator", "missing trust", "wrong digest", "future", "wrong height", "wrong canonical hash"} {
  t.Run(name, func(t *testing.T) {
   candidate := c
   nonValidator, tip := true, uint64(1)
   switch name {
   case "validator": nonValidator = false
   case "missing trust": candidate.Trust = ""
   case "wrong digest": candidate.SHA256 = fmt.Sprintf("%064x", 1)
   case "future": tip = 0
   case "wrong height": candidate.Height = 2; tip = 2
   case "wrong canonical hash":
    other := makeSignedBlk(t, 1, crypto.HashBytes([]byte("other")), priv, pub)
    putRawBlk(t, db, other)
   }
   if _, err := loadOperatorRegistryBootstrap(&candidate, nonValidator, db, tip); err == nil { t.Fatal("unsafe anchor accepted") }
  })
 }
}

func TestBootstrapRegistryRejectsMissingAndSeededMembership(t *testing.T) {
 if err := validateBootstrapRegistry(core.RegistrySnapshot{}); err == nil { t.Fatal("empty registry accepted") }
 pub := newLPoDAuthority(t)
 for _, entry := range []*core.ValidatorEntry{nil, {PubKey:pub, Status:core.ValidatorActive, Seeded:true}, {PubKey:make([]byte,32), Status:core.ValidatorActive}} {
  if err := validateBootstrapRegistry(core.RegistrySnapshot{Validators:map[string]*core.ValidatorEntry{pub.Hex():entry}}); err == nil { t.Fatal("invalid registry accepted") }
 }
}

func TestOperatorRegistryReplayEpochAndUnauthorizedProposer(t *testing.T) {
 for _, unauthorized := range []bool{false, true} {
  t.Run(fmt.Sprint(unauthorized), func(t *testing.T) {
   db, err := store.Open(t.TempDir())
   if err != nil { t.Fatal(err) }
   defer db.Close()
   priv, pub, err := crypto.GenerateValidatorKey()
   if err != nil { t.Fatal(err) }
   pending := newLPoDAuthority(t)
   registry := core.NewValidatorRegistry()
   registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
   state := registry.TakeSnapshot()
   state.Validators[pending.Hex()] = &core.ValidatorEntry{PubKey:pending, Status:core.ValidatorPending, StakeNAPR:core.MinStakeNAPR*10, ActivationEpoch:1}
   registry.RestoreFromSnapshot(state)
   anchor := makeSignedBlk(t, core.EpochLength-1, crypto.Hash32{}, priv, pub)
   putRawBlk(t, db, anchor)
   if unauthorized {
    priv, pub, err = crypto.GenerateValidatorKey()
    if err != nil { t.Fatal(err) }
   }
   next := makeSignedBlk(t, core.EpochLength, anchor.Hash(), priv, pub)
   putRawBlk(t, db, next)
   if err:=db.PutTip(next.Hash(),core.EpochLength);err!=nil { t.Fatal(err) }
   snap := &startupSnapshot{TipHeight:core.EpochLength-1}
   err = replayOperatorRegistryBootstrap(db, core.NewUTXOSet(), registry, snap, core.EpochLength, silentLog())
   if unauthorized {
    if err == nil { t.Fatal("nonmember proposer accepted") }
   } else {
    if err != nil { t.Fatal(err) }
    if !registry.IsActive(pending) { t.Fatal("epoch activation was not replayed") }
   }
  })
 }
}

func TestOperatorRegistryReplayRejectsBrokenContinuity(t *testing.T) {
 for _, variant := range []string{"missing", "height", "parent", "signature", "merkle", "stake"} {
  t.Run(variant, func(t *testing.T) {
   db, err := store.Open(t.TempDir())
   if err != nil { t.Fatal(err) }
   defer db.Close()
   priv, pub, err := crypto.GenerateValidatorKey()
   if err != nil { t.Fatal(err) }
   registry := core.NewValidatorRegistry()
   registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
   anchor := makeSignedBlk(t, 1, crypto.Hash32{}, priv, pub)
   putRawBlk(t, db, anchor)
   block := makeSignedBlk(t, 2, anchor.Hash(), priv, pub)
   switch variant {
   case "height": block.Header.Height = 3
   case "parent": block.Header.PrevHash = crypto.HashBytes([]byte("wrong"))
   case "signature": block.Header.ValidatorPub = newLPoDAuthority(t)
   case "merkle": block.Header.MerkleRoot = crypto.HashBytes([]byte("wrong"))
   case "stake":
    block.Txs = []core.Transaction{{Version:core.TxVersionStake, Extra:[]byte{1}}}
    block.Header.MerkleRoot = core.MerkleRoot(block.Txs)
   }
   if variant != "signature" {
    if err := block.Header.Sign(priv); err != nil { t.Fatal(err) }
   }
   if variant != "missing" {
    raw, err := json.Marshal(block)
    if err != nil { t.Fatal(err) }
    if err := db.PutRawBlock(block.Hash(), 2, raw); err != nil { t.Fatal(err) }
   }
   if err:=db.PutTip(block.Hash(),2);err!=nil { t.Fatal(err) }
   if err := replayOperatorRegistryBootstrap(db, core.NewUTXOSet(), registry, &startupSnapshot{TipHeight:1}, 2, silentLog()); err == nil { t.Fatal("invalid replay accepted") }
  })
 }
}