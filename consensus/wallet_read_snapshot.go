// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import "fmt"

// WithCanonicalRead captures external durable reads against one canonical
// transition. The callback should do only the short capture operation (such as
// LevelDB GetSnapshot); decoding and iteration belong after it returns.
func (e *Engine) WithCanonicalRead(fn func() error) error {
	if fn == nil {
		return fmt.Errorf("canonical read callback is nil")
	}
	e.productionMu.Lock()
	defer e.productionMu.Unlock()
	return fn()
}
