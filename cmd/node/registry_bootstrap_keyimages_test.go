package main

import (
"crypto/sha256"
"encoding/json"
"fmt"
"os"
"path/filepath"
"testing"

"github.com/aperod/aperod/crypto"
"github.com/aperod/aperod/store"
)

func TestBootstrapKeyImageAnchorBinding(t *testing.T) {
for _,variant:=range []string{"valid","height","hash","digest","missing","duplicate","invalid-image"} {
t.Run(variant,func(t *testing.T){
anchor:=bootstrapKeyImageAnchor{Height:10,BlockHash:"abc",KeyImages:[]crypto.KeyImage{}}
switch variant {
case "height":anchor.Height++
case "hash":anchor.BlockHash="wrong"
case "invalid-image":anchor.KeyImages=[]crypto.KeyImage{{}}
}
raw,err:=json.Marshal(anchor);if err!=nil { t.Fatal(err) }
if variant=="missing" { raw=[]byte(`{"height":10,"block_hash":"abc"}`) }
if variant=="duplicate" { raw=[]byte(`{"height":10,"height":10,"block_hash":"abc","key_images":[]}`) }
path:=filepath.Join(t.TempDir(),"anchor.json")
if err:=os.WriteFile(path,raw,0600);err!=nil { t.Fatal(err) }
digest:=fmt.Sprintf("%x",sha256.Sum256(raw))
if variant=="digest" { digest=fmt.Sprintf("%064x",1) }
_,err=loadBootstrapKeyImageAnchor(path,digest,10,"abc")
if variant=="valid" { if err!=nil { t.Fatal(err) } } else if err==nil { t.Fatal("invalid anchor accepted") }
})
}
}

// Explicit diagnostic export from an existing COPY, never a live database.
// No membership/signature is generated; only existing public spent images.
func TestBootstrapExportCopiedAnchorKeyImages(t *testing.T) {
source,output:=os.Getenv("LPOD_ANCHOR_EXPORT_DB"),os.Getenv("LPOD_ANCHOR_EXPORT_FILE")
if source=="" || output=="" { t.Skip("explicit copied DB and output required") }
db,err:=store.OpenReadOnly(source);if err!=nil { t.Fatal(err) };defer db.Close()
hash,height,err:=db.GetTip();if err!=nil { t.Fatal(err) }
if height!=2492066 || fmt.Sprintf("%x",hash)!="cc84407987fc753350d84b8e38d5b3b67dec0c8374e58f11ca166d8498d2364c" {
t.Fatal("copied DB is not the exact known snapshot anchor")
}
anchor:=bootstrapKeyImageAnchor{Height:height,BlockHash:fmt.Sprintf("%x",hash),KeyImages:[]crypto.KeyImage{}}
if err:=db.IterKeyImages(func(image crypto.KeyImage)error{anchor.KeyImages=append(anchor.KeyImages,image);return nil});err!=nil { t.Fatal(err) }
raw,err:=json.Marshal(anchor);if err!=nil { t.Fatal(err) }
// O_EXCL prevents accidentally replacing previously pinned evidence.
f,err:=os.OpenFile(output,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600);if err!=nil { t.Fatal(err) }
_,err=f.Write(raw);closeErr:=f.Close();if err!=nil || closeErr!=nil { t.Fatalf("write %v/%v",err,closeErr) }
digest:=fmt.Sprintf("%x",sha256.Sum256(raw))
if _,err:=loadBootstrapKeyImageAnchor(output,digest,height,anchor.BlockHash);err!=nil { t.Fatal(err) }
t.Logf("copied anchor height=%d hash=%s key_images=%d export_sha256=%s",height,anchor.BlockHash,len(anchor.KeyImages),digest)
}