package main

import (
"bytes"
"crypto/sha256"
"encoding/hex"
"encoding/json"
"fmt"
"io"
"os"

"github.com/aperod/aperod/crypto"
)

type bootstrapKeyImageAnchor struct {
Height uint64 `json:"height"`
BlockHash string `json:"block_hash"`
KeyImages []crypto.KeyImage `json:"key_images"`
}

// This file is an operator-attested public state export, not a signature or an
// independently authenticated historical spend proof. The operator must derive
// it from a preserved copied DB whose durable tip is exactly this anchor.
func loadBootstrapKeyImageAnchor(path,digest string,height uint64,hash string)([]crypto.KeyImage,error) {
expected,err:=hex.DecodeString(digest)
if err!=nil || len(expected)!=32 { return nil,fmt.Errorf("key-image anchor requires SHA-256") }
f,err:=os.Open(path);if err!=nil { return nil,err };defer f.Close()
const maxBytes=64<<20
info,err:=f.Stat()
if err!=nil || !info.Mode().IsRegular() || info.Size()>maxBytes { return nil,fmt.Errorf("key-image anchor must be a regular file <=64 MiB") }
raw,err:=io.ReadAll(io.LimitReader(f,maxBytes+1))
if err!=nil || len(raw)>maxBytes { return nil,fmt.Errorf("key-image anchor read failed or exceeds 64 MiB") }
actual:=sha256.Sum256(raw)
if !bytes.Equal(actual[:],expected) { return nil,fmt.Errorf("key-image anchor digest mismatch") }
var anchor bootstrapKeyImageAnchor
d:=json.NewDecoder(bytes.NewReader(raw))
seen:=make(map[string]bool)
err=decodeBootstrapObject(d,func(key string)error{
seen[key]=true
switch key {
case "height":return d.Decode(&anchor.Height)
case "block_hash":return d.Decode(&anchor.BlockHash)
case "key_images":return decodeBootstrapArrayBounded(d,&anchor.KeyImages,250000)
default:return fmt.Errorf("unknown key-image anchor field %q",key)
}
})
if err!=nil { return nil,err }
if err:=d.Decode(new(any));err!=io.EOF { return nil,fmt.Errorf("key-image anchor trailing data") }
if !seen["height"] || !seen["block_hash"] || !seen["key_images"] ||
anchor.Height!=height || anchor.BlockHash!=hash || len(anchor.KeyImages)>250000 {
return nil,fmt.Errorf("key-image anchor required fields/height/hash/count mismatch")
}
images:=make(map[crypto.KeyImage]bool,len(anchor.KeyImages))
for _,image:=range anchor.KeyImages {
canonical,err:=crypto.CanonicalKeyImage(image)
if err!=nil || canonical!=image || images[image] { return nil,fmt.Errorf("key-image anchor contains invalid or duplicate image") }
images[image]=true
}
return anchor.KeyImages,nil
}