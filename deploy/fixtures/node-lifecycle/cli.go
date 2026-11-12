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

	address := "apro" + strings.Repeat("A", 99)
	for i, arg := range os.Args {
		if arg == "--output" && i+1 < len(os.Args) {
			keystore := fmt.Sprintf(
				`{"address":%q,"crypto":{"ciphertext":"synthetic-only"}}`, address)
			if err := os.WriteFile(os.Args[i+1], []byte(keystore), 0600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(address)
			return
		}
	}
	fmt.Println(address)
}
