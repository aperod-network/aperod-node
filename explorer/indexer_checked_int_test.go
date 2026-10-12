package explorer

import "testing"

func TestCheckedInt64FromUint64(t *testing.T) {
const max = uint64(1<<63 - 1)

got, err := checkedInt64FromUint64("height", max)
if err != nil || got != int64(max) {
t.Fatalf("max signed value: got %d, err %v", got, err)
}

if _, err := checkedInt64FromUint64("height", max+1); err == nil {
t.Fatal("expected values above PostgreSQL BIGINT range to be rejected")
}
}
