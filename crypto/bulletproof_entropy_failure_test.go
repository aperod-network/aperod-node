package crypto

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// No global RNG hooks: each subtest has its own finite, deterministic source.
// The fixed nonzero scalars are for isolated tests only, never wallet entropy.
func bpTestBlind(call int) BlindFactor {
	var blind BlindFactor
	blind[0] = byte(call%250 + 1)
	return blind
}

func bpEntropyStage(bits, call int) string {
	if call == 0 {
		return "alpha"
	}
	if call <= 2*bits {
		side := "sL"
		if call%2 == 0 {
			side = "sR"
		}
		return fmt.Sprintf("%s[%d]", side, (call-1)/2)
	}
	return []string{"rho", "tau1", "tau2"}[call-2*bits-1]
}

func TestBulletproofRejectsEveryEntropyFailure(t *testing.T) {
	failure := errors.New("isolated entropy failure")
	for _, bits := range []uint8{16, 64} {
		draws := 2*int(bits) + 4
		for call := 0; call < draws; call++ {
			for _, invalidScalar := range []bool{false, true} {
				name := fmt.Sprintf("%d/%s/noncanonical=%t", bits, bpEntropyStage(int(bits), call), invalidScalar)
				t.Run(name, func(t *testing.T) {
					count := 0
					source := func() (BlindFactor, error) {
						current := count
						count++
						if current == call {
							if !invalidScalar {
								return BlindFactor{}, failure
							}
							var invalid BlindFactor
							for i := range invalid {
								invalid[i] = 0xff
							}
							return invalid, nil
						}
						if current > call {
							t.Fatal("continued drawing entropy after failure")
						}
						return bpTestBlind(current), nil
					}
					proof, err := proveRangeBits(42, bpTestBlind(200), bits, source)
					if err == nil || proof != nil {
						t.Fatalf("failure must return error and no partial proof: proof=%v, err=%v", proof != nil, err)
					}
					if !strings.Contains(err.Error(), "bulletproof "+bpEntropyStage(int(bits), call)) {
						t.Fatalf("wrong failure stage: %v", err)
					}
					if !invalidScalar && !errors.Is(err, failure) {
						t.Fatalf("entropy error was not preserved: %v", err)
					}
					if count != call+1 {
						t.Fatalf("got %d entropy draws, want %d", count, call+1)
					}
				})
			}
		}
		// Successful controls traverse the same path through the final draw
		// and are independently checked by the strict proof verifier.
		t.Run(fmt.Sprintf("%d/valid_control", bits), func(t *testing.T) {
			count := 0
			proof, err := proveRangeBits(42, bpTestBlind(200), bits, func() (BlindFactor, error) {
				blind := bpTestBlind(count)
				count++
				return blind, nil
			})
			if err != nil || proof == nil {
				t.Fatalf("valid proof failed: %v", err)
			}
			if count != draws {
				t.Fatalf("got %d entropy draws, want %d", count, draws)
			}
			ok, err := VerifyRangeStrict(proof)
			if err != nil || !ok {
				t.Fatalf("valid strict control rejected: %v", err)
			}
		})
	}
}

func TestBulletproofRejectsInvalidInputBeforeEntropy(t *testing.T) {
	var invalid BlindFactor
	for i := range invalid {
		invalid[i] = 0xff
	}
	for _, tc := range []struct {
		name  string
		bits  uint8
		value uint64
		blind BlindFactor
	}{
		{"width", 32, 42, bpTestBlind(1)},
		{"value", 16, 1 << 16, bpTestBlind(1)},
		{"blind16", 16, 42, invalid},
		{"blind64", 64, 42, invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := proveRangeBits(tc.value, tc.blind, tc.bits, func() (BlindFactor, error) {
				t.Fatal("invalid input reached entropy source")
				return BlindFactor{}, nil
			})
			if err == nil || proof != nil {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
}
