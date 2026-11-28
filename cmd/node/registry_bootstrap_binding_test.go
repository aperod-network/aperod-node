package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func TestRegistryBootstrapKeyImageBindingRequiresSignedMatch(t *testing.T) {
	digest := strings.Repeat("a", 64)
	statement := mainnetbootstrap.Statement{
		KeyImageAnchorSHA256:    digest,
		KeyImageAnchorHeight:    7,
		KeyImageAnchorBlockHash: strings.Repeat("b", 64),
	}

	if err := validateRegistryBootstrapKeyImageBinding(nil, statement, &config.RegistryBootstrapConfig{
		Height: 7,
	}); err == nil {
		t.Fatal("partial key-image config accepted")
	}
	if err := validateRegistryBootstrapKeyImageBinding(nil, statement, &config.RegistryBootstrapConfig{
		Height:         8,
		KeyImageFile:   "/tmp/anchor.json",
		KeyImageSHA256: digest,
	}); err == nil {
		t.Fatal("config anchor at a different height accepted")
	}
	if err := validateRegistryBootstrapKeyImageBinding(nil, statement, &config.RegistryBootstrapConfig{
		Height:         7,
		KeyImageFile:   "/tmp/anchor.json",
		KeyImageSHA256: strings.Repeat("c", 64),
	}); err == nil {
		t.Fatal("config anchor with a different digest accepted")
	}
}

func TestRegistryBootstrapKeyImageBindingChecksCanonicalBlockHash(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close key-image binding database: %v", err)
		}
	}()

	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	block := makeSignedBlk(t, 7, crypto.Hash32{}, priv, pub)
	putRawBlk(t, db, block)

	digest := strings.Repeat("a", 64)
	statement := mainnetbootstrap.Statement{
		KeyImageAnchorSHA256:    digest,
		KeyImageAnchorHeight:    7,
		KeyImageAnchorBlockHash: fmt.Sprintf("%x", block.Hash()),
	}
	cfg := &config.RegistryBootstrapConfig{
		Height:         7,
		KeyImageFile:   "/tmp/anchor.json",
		KeyImageSHA256: digest,
	}
	if err := validateRegistryBootstrapKeyImageBinding(db, statement, cfg); err != nil {
		t.Fatalf("valid signed key-image anchor rejected: %v", err)
	}

	statement.KeyImageAnchorBlockHash = strings.Repeat("b", 64)
	if err := validateRegistryBootstrapKeyImageBinding(db, statement, cfg); err == nil {
		t.Fatal("key-image anchor whose canonical block hash differs was accepted")
	}
}
