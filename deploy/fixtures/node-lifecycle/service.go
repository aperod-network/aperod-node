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
	args := os.Args[1:]
	hasArg := func(want string) bool {
		for _, arg := range args {
			if arg == want {
				return true
			}
		}
		return false
	}
	validateConfig := func() bool {
		for i, arg := range args {
			if arg == "--config" && i+1 < len(args) {
				data, err := os.ReadFile(args[i+1])
				return err == nil && strings.Contains(string(data), "data_dir:")
			}
		}
		return false
	}
	// Match the production node's read-only maintenance contract so lifecycle
	// tests exercise the installer's pre-stop guard instead of rejecting the
	// synthetic candidate as an unsupported legacy binary.
	if hasArg("--sale-upgrade-preflight-capability") {
		if !hasArg("--validate-config") || !validateConfig() {
			os.Exit(3)
		}
		fmt.Fprintln(os.Stdout, `{"schema":1,"command":"sale-upgrade-preflight"}`)
		return
	}
	if hasArg("--sale-upgrade-preflight") {
		if !validateConfig() {
			os.Exit(3)
		}
		fmt.Fprintf(os.Stdout,
			`{"schema":1,"status":"ok","mode":"inactive","tip_height":100,"tip_hash":%q}`+"\n",
			strings.Repeat("a", 64))
		return
	}
	for i, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println(version)
			return
		}
		if arg == "--validate-config" {
			if validateConfig() {
				fmt.Println("fixture config accepted")
				return
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
