//go:build !linux

package api

import "fmt"

func evictDailyBenchmarkFiles(string) (int, int64, error) {
	return 0, 0, fmt.Errorf("per-file benchmark cache eviction is only supported on Linux")
}
