// Synthetic deterministic wallet output; never used with production identities.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	key := strings.Repeat("33", 32)
	pub := strings.Repeat("22", 32)
	args := strings.Join(os.Args[1:], " ")
	if strings.Contains(args, "keygen") {
		fmt.Printf("Private: %s\nPublic: %s\n", key, pub)
		return
	}
	if strings.Contains(args, "pubkey") {
		fmt.Println(pub)
		return
	}
	for i, arg := range os.Args {
		if arg == "--out" && i+1 < len(os.Args) {
			_ = os.WriteFile(os.Args[i+1], []byte(`{"address":"apro`+strings.Repeat("A", 99)+`"}`), 0600)
			return
		}
	}
	fmt.Println("apro" + strings.Repeat("A", 99))
}
