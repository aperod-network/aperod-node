package consensus

// CheckpointInterval is how many blocks between checkpoint anchors.
const CheckpointInterval = 1000

// IsCheckpoint returns true if height is a checkpoint block.
// Height 0 (genesis) is never a checkpoint; the first checkpoint is at height 1000.
func IsCheckpoint(height uint64) bool {
	return height > 0 && height%CheckpointInterval == 0
}

// NextCheckpoint returns the next checkpoint height at or after h.
func NextCheckpoint(h uint64) uint64 {
	if h == 0 {
		return CheckpointInterval
	}
	rem := h % CheckpointInterval
	if rem == 0 {
		return h
	}
	return h + (CheckpointInterval - rem)
}

// PrevCheckpoint returns the most recent checkpoint height at or before h.
// Returns 0 if h < CheckpointInterval.
func PrevCheckpoint(h uint64) uint64 {
	return (h / CheckpointInterval) * CheckpointInterval
}
