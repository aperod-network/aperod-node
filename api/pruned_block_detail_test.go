// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"testing"

	"github.com/aperod/aperod/store"
)

func TestPrunedBlockDetailBurnTotalsUnavailable(t *testing.T) {
	response := prunedBlockDetailResponse(&store.StoredBlock{TxCount: 2})
	for _, field := range []string{
		"fees_burned_napro",
		"protocol_fee_burned_napro",
		"intentional_burn_napro",
		"avm_gas_burned_napro",
	} {
		if value, exists := response[field]; !exists || value != nil {
			t.Errorf("%s = %#v (present=%v), want explicit unavailable null", field, value, exists)
		}
	}
}
