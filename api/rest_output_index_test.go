package api

import "testing"

func TestOutputIndexInRange(t *testing.T) {
	tests := []struct {
		name       string
		outIndex   uint32
		outputSize int
		want       bool
	}{
		{name: "first output", outIndex: 0, outputSize: 1, want: true},
		{name: "index at end", outIndex: 1, outputSize: 1, want: false},
		{name: "empty outputs", outIndex: 0, outputSize: 0, want: false},
		{name: "negative output count", outIndex: 0, outputSize: -1, want: false},
		{name: "maximum uint32", outIndex: ^uint32(0), outputSize: 1, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outputIndexInRange(tt.outIndex, tt.outputSize); got != tt.want {
				t.Fatalf("outputIndexInRange(%d, %d) = %t, want %t",
					tt.outIndex, tt.outputSize, got, tt.want)
			}
		})
	}
}
