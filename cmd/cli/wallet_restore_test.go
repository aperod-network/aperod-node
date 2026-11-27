package main

import (
	"strings"
	"testing"
)

func TestReadMnemonicLinePreservesAllWords(t *testing.T) {
	const phrase = "one two three four five six seven eight nine ten eleven twelve"
	got, err := readMnemonicLine(strings.NewReader(phrase + "\n"))
	if err != nil {
		t.Fatalf("readMnemonicLine: %v", err)
	}
	if got != phrase {
		t.Fatalf("mnemonic = %q, want %q", got, phrase)
	}
}
