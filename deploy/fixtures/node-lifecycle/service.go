// This is a synthetic service for host lifecycle tests, not an Aperod node.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const version = "fixture-old"
const unhealthy = false

func main() {
	for i, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println(version)
			return
		}
		if arg == "--validate-config" {
			for j, value := range os.Args {
				if value == "--config" && j+1 < len(os.Args) {
					data, err := os.ReadFile(os.Args[j+1])
					if err != nil || !strings.Contains(string(data), "data_dir:") {
						os.Exit(3)
					}
					fmt.Println("fixture config accepted")
					return
				}
			}
			fmt.Fprintln(os.Stderr, "missing fixture config", i)
			os.Exit(3)
		}
	}
	var height atomic.Int64
	height.Store(100)
	go func() {
		for range time.Tick(100 * time.Millisecond) {
			height.Add(1)
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if unhealthy {
			http.Error(w, "synthetic unhealthy candidate", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"height": height.Load(), "tip_height": height.Load(), "syncing": false,
			"version": version, "peer_count": 1, "peers": map[string]int{"connected": 1},
		})
	})
	if err := http.ListenAndServe("127.0.0.1:8545", nil); err != nil {
		panic(err)
	}
}
