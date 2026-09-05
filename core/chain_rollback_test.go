package core

import "testing"

func TestChainRollbackLastBlockRestoresEvictedWindowEntry(t *testing.T) {
	chain := NewChain(1)
	genesis := &Block{Header: BlockHeader{Height: 0}}
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}

	block := &Block{Header: BlockHeader{
		Height:   1,
		PrevHash: genesis.Hash(),
		Round:    1,
	}}
	if err := chain.AddBlock(block); err != nil {
		t.Fatal(err)
	}
	if chain.GetByHeight(0) != nil {
		t.Fatal("genesis was not evicted from one-block window")
	}

	if err := chain.RollbackLastBlock(block); err != nil {
		t.Fatal(err)
	}
	if chain.Tip() != genesis || chain.GetByHeight(0) != genesis ||
		chain.GetByHash(genesis.Hash()) != genesis {
		t.Fatal("rollback did not restore the previous tip and evicted indexes")
	}
	if chain.GetByHeight(1) != nil || chain.HasBlock(block.Hash()) {
		t.Fatal("rollback left the rejected block indexed")
	}
}
