package api

import (
	"encoding/json"
	"math/big"
)

// estimatedUnbondTime retains the existing numeric JSON field while keeping
// uint64 height differences from wrapping into a past (or negative) time.
// These estimates are display-only; withdrawal eligibility remains height-based.
func estimatedUnbondTime(nowMS int64, blocksLeft uint64) json.Number {
	delta := new(big.Int).Mul(new(big.Int).SetUint64(blocksLeft), big.NewInt(1000))
	return json.Number(delta.Add(delta, big.NewInt(nowMS)).String())
}
