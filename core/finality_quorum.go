package core

import (
	"fmt"
	"math"
)

// RequiredFinalityVotes preserves the strict quorum rule floor(n*ppm/1e6)+1
// without overflowing the intermediate multiplication or narrowing the result.
func RequiredFinalityVotes(members int, thresholdPPM uint32) (int, error) {
	if members < 0 || thresholdPPM == 0 || thresholdPPM > 1_000_000 {
		return 0, fmt.Errorf("invalid finality committee size or threshold")
	}
	n := uint64(members)
	p := uint64(thresholdPPM)
	required := (n/1_000_000)*p + ((n%1_000_000)*p)/1_000_000 + 1
	if required > math.MaxInt {
		return 0, fmt.Errorf("finality quorum exceeds native integer range")
	}
	return int(required), nil
}
