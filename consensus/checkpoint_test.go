package consensus

import "testing"

func TestIsCheckpoint(t *testing.T) {
	cases := []struct {
		height uint64
		want   bool
	}{
		{0, false},
		{1, false},
		{999, false},
		{1000, true},
		{1001, false},
		{2000, true},
		{9999, false},
		{10000, true},
	}
	for _, c := range cases {
		if got := IsCheckpoint(c.height); got != c.want {
			t.Errorf("IsCheckpoint(%d) = %v, want %v", c.height, got, c.want)
		}
	}
}

func TestNextCheckpoint(t *testing.T) {
	cases := []struct {
		h    uint64
		want uint64
	}{
		{0, 1000},
		{1, 1000},
		{999, 1000},
		{1000, 1000},
		{1001, 2000},
		{1999, 2000},
		{2000, 2000},
	}
	for _, c := range cases {
		if got := NextCheckpoint(c.h); got != c.want {
			t.Errorf("NextCheckpoint(%d) = %d, want %d", c.h, got, c.want)
		}
	}
}

func TestPrevCheckpoint(t *testing.T) {
	cases := []struct {
		h    uint64
		want uint64
	}{
		{0, 0},
		{500, 0},
		{999, 0},
		{1000, 1000},
		{1500, 1000},
		{2000, 2000},
		{2999, 2000},
	}
	for _, c := range cases {
		if got := PrevCheckpoint(c.h); got != c.want {
			t.Errorf("PrevCheckpoint(%d) = %d, want %d", c.h, got, c.want)
		}
	}
}
