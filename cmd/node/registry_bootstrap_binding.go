package main

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func validateRegistryBootstrapKeyImageBinding(
	db *store.DB,
	statement mainnetbootstrap.Statement,
	bootstrap *config.RegistryBootstrapConfig,
) error {
	if bootstrap == nil {
		return nil
	}

	hasFile := bootstrap.KeyImageFile != ""
	hasDigest := bootstrap.KeyImageSHA256 != ""
	if hasFile != hasDigest {
		return errors.New("registry bootstrap key-image file and digest must be configured together")
	}

	hasSignedAnchor := statement.KeyImageAnchorSHA256 != "" ||
		statement.KeyImageAnchorHeight != 0 ||
		statement.KeyImageAnchorBlockHash != ""
	if !hasFile {
		if hasSignedAnchor {
			return errors.New("signed key-image anchor is not configured for registry bootstrap")
		}
		return nil
	}
	if !hasSignedAnchor ||
		bootstrap.KeyImageSHA256 != statement.KeyImageAnchorSHA256 ||
		bootstrap.Height != statement.KeyImageAnchorHeight {
		return errors.New("registry bootstrap key-image anchor does not match the signed Mainnet statement")
	}
	if db == nil {
		return errors.New("registry bootstrap key-image anchor requires the opened chain database")
	}
	hash, found, err := db.GetCanonicalHash(bootstrap.Height)
	if err != nil {
		return fmt.Errorf("read registry key-image anchor height %d: %w", bootstrap.Height, err)
	}
	if !found || hex.EncodeToString(hash[:]) != statement.KeyImageAnchorBlockHash {
		return errors.New("registry bootstrap key-image anchor block hash does not match the signed Mainnet statement")
	}
	return nil
}
