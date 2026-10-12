package sale

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// NetAllocationBasis is an explicitly authority-attested forward accounting
// basis, not a proof of pre-checkpoint history. WalletNet accounts for issued
// holdings OUTSIDE the separately reserved LPoD principal and reward balance,
// including validator self-stake. Burned and WalletNet must use one baseline.
type NetAllocationBasis struct {
	Burned               uint64        `json:"burned_napro,string"`
	WalletNet            uint64        `json:"outside_wallet_net_napro,string"`
	WalletAccountingRoot crypto.Hash32 `json:"wallet_accounting_root"`
}

func (n NetAllocationBasis) Consumed() (uint64, error) {
	if n.WalletAccountingRoot == zeroHash || n.Burned > CapNAPRO || n.WalletNet > CapNAPRO-n.Burned {
		return 0, fmt.Errorf("sale: attested post-burn wallet accounting required")
	}
	return n.Burned + n.WalletNet, nil
}

func (n NetAllocationBasis) Digest() crypto.Hash32 {
	return crypto.HashBytes([]byte("aperod/sale/post-burn-wallet-basis/v2"), u64(n.Burned), u64(n.WalletNet), n.WalletAccountingRoot[:])
}
