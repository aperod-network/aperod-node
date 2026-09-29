package main

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestHistoricalHeaderHashCandidates(t *testing.T) {
	var previous, merkle crypto.Hash32
	for i := range previous {
		previous[i] = byte(i)
		merkle[i] = byte(31 - i)
	}
	header := core.BlockHeader{
		Height:       0x0807060504030201,
		PrevHash:     previous,
		MerkleRoot:   merkle,
		Timestamp:    0x1817161514131211,
		Round:        0x24232221,
		ValidatorPub: []byte{0xaa, 0xbb, 0xcc},
		OraclePrice:  0x3837363534333231,
		BaseFee:      0x4847464544434241,
	}

	tests := []struct {
		name    string
		oracle  bool
		baseFee bool
		wantHex string
	}{
		{name: "before oracle and base fee", wantHex: "6830cc0762c344151043454bf08296a1878b21b5588a69bf9eda9cfd71daf4b9"},
		{name: "oracle only", oracle: true, wantHex: "a4881328e5351467d4e08a097952d1626d809d368d4982e463f0e7531fea8233"},
		{name: "both fields", oracle: true, baseFee: true, wantHex: "dfee4a73dd03ddcd9f9696a777e94d83459084bdb180242be899e9e4301c7197"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := historicalHeaderHash(header, test.oracle, test.baseFee)
			want, err := hex.DecodeString(test.wantHex)
			if err != nil {
				t.Fatal(err)
			}
			if string(got[:]) != string(want) {
				t.Fatalf("candidate hash = %x, want %x", got, want)
			}
		})
	}
}
