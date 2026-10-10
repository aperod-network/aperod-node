package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func samplingFixture(id byte) *UTXO {
	return &UTXO{
		TxHash: crypto.Hash32{id}, OneTimePub: crypto.Point32{id},
		AmountCommit: crypto.Commitment{id, 0xa5},
	}
}

func TestDecoySamplingUniformOrderedSelections(t *testing.T) {
	// Enumerate all equiprobable draws for bounds 4, 3, 2, rather than a
	// probabilistic frequency assertion. Each of the 24 ordered triples
	// must occur exactly once and retain the key/commitment pairing.
	outcomes := make(map[string]int)
	for a := byte(0); a < 4; a++ {
		for b := byte(0); b < 3; b++ {
			for c := byte(0); c < 2; c++ {
				pool := []*UTXO{samplingFixture(1), samplingFixture(2), samplingFixture(3), samplingFixture(4)}
				got, err := sampleDecoyCandidates(pool, 3, bytes.NewReader([]byte{a, b, c}))
				if err != nil || len(got) != 3 {
					t.Fatalf("selection: %v %v", got, err)
				}
				seen := map[crypto.Point32]bool{}
				for _, d := range got {
					if seen[d.OneTimePub] || d.AmountCommit != samplingFixture(d.OneTimePub[0]).AmountCommit {
						t.Fatal("repeated key or altered commitment")
					}
					seen[d.OneTimePub] = true
				}
				outcomes[fmt.Sprint(got[0].OneTimePub[0], got[1].OneTimePub[0], got[2].OneTimePub[0])]++
			}
		}
	}
	if len(outcomes) != 24 {
		t.Fatalf("only %d ordered outcomes", len(outcomes))
	}
	for _, count := range outcomes {
		if count != 1 {
			t.Fatal("unequal outcome multiplicity")
		}
	}
}

func TestDecoySamplingRejectsOutOfRangeEntropy(t *testing.T) {
	reader := bytes.NewReader([]byte{0xff, 2})
	got, err := sampleDecoyCandidates([]*UTXO{samplingFixture(1), samplingFixture(2), samplingFixture(3)}, 1, reader)
	if err != nil || len(got) != 1 || got[0].OneTimePub[0] != 3 || reader.Len() != 0 {
		t.Fatalf("rejection sampling changed: %v %v", got, err)
	}
}

func TestDecoySamplingEntropyFailureDiscardsPartialSelection(t *testing.T) {
	for _, reader := range []io.Reader{nil, bytes.NewReader(nil), bytes.NewReader([]byte{1})} {
		pool := []*UTXO{samplingFixture(1), samplingFixture(2), samplingFixture(3)}
		before := map[crypto.Point32]UTXO{}
		for _, u := range pool {
			before[u.OneTimePub] = *u
		}
		got, err := sampleDecoyCandidates(pool, 2, reader)
		if err == nil || got != nil {
			t.Fatalf("entropy failure returned partial output: %v %v", got, err)
		}
		for _, u := range pool {
			if *u != before[u.OneTimePub] {
				t.Fatal("sampling modified an underlying UTXO")
			}
		}
	}
}

func TestDecoySelectorsFiltersCountsAndFailures(t *testing.T) {
	for _, clsag := range []bool{false, true} {
		t.Run(fmt.Sprintf("clsag=%t", clsag), func(t *testing.T) {
			set := NewUTXOSet()
			for id := byte(1); id <= 5; id++ {
				set.spentPubKeys[crypto.Point32{id}] = samplingFixture(id)
			}
			set.spentPubKeys[crypto.Point32{2}].ProtocolLocked = true
			set.byPubKey[crypto.Point32{6}] = samplingFixture(6)
			set.byPubKey[crypto.Point32{7}] = samplingFixture(7)
			set.byPubKey[crypto.Point32{7}].ProtocolLocked = true
			// Same key with a stale spent commitment: active lookup must win.
			set.byPubKey[crypto.Point32{3}] = samplingFixture(3)
			set.byPubKey[crypto.Point32{3}].AmountCommit[1] = 0xb6
			exclude := map[crypto.Point32]bool{{1}: true, {6}: true}
			sample := set.sampleDecoys
			if clsag {
				sample = set.sampleCLSAGDecoys
			}
			for _, count := range []int{-1, 0, 1, 3, 99} {
				got, err := sample(count, exclude, bytes.NewReader(make([]byte, 100)))
				if err != nil {
					t.Fatal(err)
				}
				want := count
				if want < 0 {
					want = 0
				}
				if want > 3 {
					want = 3
				}
				if len(got) != want {
					t.Fatalf("count=%d got=%d want=%d", count, len(got), want)
				}
				seen := map[crypto.Point32]bool{}
				for _, d := range got {
					if seen[d.OneTimePub] || exclude[d.OneTimePub] || d.OneTimePub[0] == 2 || d.OneTimePub[0] == 7 {
						t.Fatal("repeated, excluded, active-only or locked candidate")
					}
					seen[d.OneTimePub] = true
					expected := set.spentPubKeys[d.OneTimePub]
					if clsag && set.byPubKey[d.OneTimePub] != nil {
						expected = set.byPubKey[d.OneTimePub]
					}
					if d.AmountCommit != expected.AmountCommit {
						t.Fatal("canonical pair changed")
					}
				}
			}
			got, err := sample(2, exclude, bytes.NewReader([]byte{0}))
			if !errors.Is(err, io.EOF) || got != nil {
				t.Fatalf("selector did not fail closed: %v %v", got, err)
			}
			empty, err := sample(5, map[crypto.Point32]bool{{1}: true, {3}: true, {4}: true, {5}: true, {6}: true}, nil)
			if err != nil || empty != nil {
				t.Fatalf("empty pool should not consume entropy: %v %v", empty, err)
			}
		})
	}
}

type samplingStore struct {
	result []DecoyUTXO
	err    error
	calls  int
}

func (*samplingStore) LookupRingMember(crypto.Point32) (*UTXO, error) { return nil, nil }
func (s *samplingStore) SampleRingMembers(_ int, _ map[crypto.Point32]bool) ([]DecoyUTXO, error) {
	s.calls++
	return s.result, s.err
}

func TestDecoySamplingPersistentStoreNoMemoryFallback(t *testing.T) {
	set := NewUTXOSet()
	set.spentPubKeys[crypto.Point32{1}] = samplingFixture(1)
	backend := &samplingStore{err: errors.New("synthetic store entropy failure")}
	set.ringMembers = backend
	if got := set.SampleCLSAGDecoys(2, nil); got != nil {
		t.Fatal("store error used a memory fallback")
	}
	if got := set.SampleCLSAGDecoys(-1, nil); got != nil || backend.calls != 1 {
		t.Fatal("nonpositive request reached persistent store")
	}
	backend.err = nil
	backend.result = []DecoyUTXO{{OneTimePub: crypto.Point32{8}, AmountCommit: crypto.Commitment{9}}}
	if got := set.SampleCLSAGDecoys(2, nil); !reflect.DeepEqual(got, backend.result) {
		t.Fatal("persistent canonical pairs changed")
	}
}

func TestDecoySamplingBuildersAbortOnEntropyError(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.AddressFromKeys(crypto.TestnetByte, keys)
	const amount = uint64(10_000_000_000)
	mint, err := BuildMintTx(addr, amount, 1)
	if err != nil {
		t.Fatal(err)
	}
	blind, err := crypto.DeterministicMintBlindV2(keys.Spend.Public, amount, 1)
	if err != nil {
		t.Fatal(err)
	}
	owned := []OwnedUTXO{{
		UTXO: UTXO{TxHash: mint.Hash(), OneTimePub: mint.Outputs[0].OneTimePub,
			TxPubKey: mint.Outputs[0].TxPubKey, AmountCommit: mint.Outputs[0].AmountCommit},
		HsScalar: crypto.ScalarFromUint64(1), Amount: amount, Blind: blind,
	}}
	for _, version := range []TxVersion{TxVersionBase, TxVersionCLSAG} {
		for _, multi := range []bool{false, true} {
			t.Run(fmt.Sprintf("version=%d/multi=%t", version, multi), func(t *testing.T) {
				set := NewUTXOSet()
				for id := byte(1); id <= 20; id++ {
					set.spentPubKeys[crypto.Point32{id}] = samplingFixture(id)
				}
				builder := NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, owned, 1).
					WithVersion(version).WithDecoySet(set)
				builder.decoyEntropy = bytes.NewReader([]byte{0})
				if multi {
					result, err := builder.BuildMulti([]BatchRecipient{{Address: addr, AmountNAPR: 1_000_000}}, addr)
					if result != nil || err == nil || !strings.Contains(err.Error(), "sample chain decoys") {
						t.Fatalf("multi builder did not abort: %v %v", result, err)
					}
				} else {
					result, err := builder.Build(1_000_000, addr, addr)
					if result != nil || err == nil || !strings.Contains(err.Error(), "sample chain decoys") {
						t.Fatalf("single builder did not abort: %v %v", result, err)
					}
				}
			})
		}
	}
}
