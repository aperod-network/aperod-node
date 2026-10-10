package main

import (
"errors"
"testing"

"github.com/aperod/aperod/core"
"github.com/aperod/aperod/crypto"
"github.com/aperod/aperod/store"
)

type failingBootstrapSpentLookup struct {
*store.DB
err error
calls int
}
func (s *failingBootstrapSpentLookup) IsUTXOSpentChecked(crypto.Hash32,uint32)(bool,error) {
s.calls++
return false,s.err
}

func TestBootstrapSpentLookupErrorsFailClosed(t *testing.T) {
for _,durableRow:=range []bool{false,true} {
name:="anchor-only"
if durableRow { name="durable-row" }
t.Run(name,func(t *testing.T){
db,err:=store.Open(t.TempDir());if err!=nil { t.Fatal(err) };defer db.Close()
u:=&core.UTXO{TxHash:crypto.HashStr("lookup-error"),BlockHeight:1}
snap:=&startupSnapshot{TipHeight:1,UTXOs:core.UTXOSnapshot{ActiveUTXOs:[]*core.UTXO{u}}}
utxos:=core.NewUTXOSet();utxos.RestoreFromSnapshot(snap.UTXOs)
if durableRow {
if err:=db.PutUTXO(u.TxHash,0,&store.StoredUTXO{TxHash:u.TxHash,BlockHeight:1});err!=nil { t.Fatal(err) }
}
sentinel:=errors.New("injected spent-marker storage failure")
faulty:=&failingBootstrapSpentLookup{DB:db,err:sentinel}
if err:=validateBootstrapTip(faulty,utxos,snap,silentLog());!errors.Is(err,sentinel) { t.Fatalf("lookup error not propagated: %v",err) }
if faulty.calls!=1 { t.Fatalf("expected lookup in %s path; got %d",name,faulty.calls) }
})
}
}

func TestBootstrapCapturedTipBinding(t *testing.T) {
for _,variant:=range []string{"unchanged","height","hash","index","closed"} {
t.Run(variant,func(t *testing.T){
db,err:=store.Open(t.TempDir());if err!=nil { t.Fatal(err) };defer db.Close()
hash,other:=crypto.HashStr("captured"),crypto.HashStr("changed")
if err:=db.PutRawBlock(hash,10,[]byte("{}"));err!=nil { t.Fatal(err) }
if err:=db.PutTip(hash,10);err!=nil { t.Fatal(err) }
switch variant {
case "height":err=db.PutTip(hash,11)
case "hash":err=db.PutTip(other,10)
case "index":err=db.PutRawBlock(other,10,[]byte("{}"))
case "closed":err=db.Close()
}
if err!=nil { t.Fatal(err) }
err=requireBootstrapTipUnchanged(db,hash,10)
if variant=="unchanged" { if err!=nil { t.Fatal(err) } } else if err==nil { t.Fatal("changed/unreadable tip accepted") }
})
}
}