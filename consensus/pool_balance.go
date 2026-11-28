package consensus

import (
	"fmt"
	"math"
)

// checkedPoolBalance prevents an unsigned balance from becoming the legacy
// engine's negative "pool disabled" sentinel. An invalid configuration or
// persisted monetary balance must stop startup/processing, not enable minting.
func checkedPoolBalance(value uint64) int64 {
	if value > math.MaxInt64 {
		panic(fmt.Sprintf("consensus: pool balance %d exceeds signed engine range", value))
	}
	return int64(value)
}
