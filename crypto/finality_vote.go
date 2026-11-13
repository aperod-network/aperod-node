package crypto

import "encoding/binary"

const (
	FinalityVoteVersionLegacy      uint8 = 1
	FinalityVoteVersionHeightBound uint8 = 2
)

// FinalityVoteMessage returns the signed digest for a finality vote.
// Version 1 is retained for historical certificates. Version 2 binds the
// height as well as the block hash, preventing a signature from being
// replayed as a vote for another height.
func FinalityVoteMessage(version uint8, height uint64, blockHash Hash32) Hash32 {
	switch version {
	case 0, FinalityVoteVersionLegacy:
		return HashBytes([]byte("aperod/finalize/v1"), blockHash[:])
	case FinalityVoteVersionHeightBound:
		var heightBytes [8]byte
		binary.BigEndian.PutUint64(heightBytes[:], height)
		return HashBytes([]byte("aperod/finalize/v2"), heightBytes[:], blockHash[:])
	default:
		return Hash32{}
	}
}
