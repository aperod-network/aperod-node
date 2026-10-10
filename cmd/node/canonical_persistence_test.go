package main

import (
	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
)

func noopCanonicalPersistence(*core.Block, *avm.PreparedBlock) error {
	return nil
}
