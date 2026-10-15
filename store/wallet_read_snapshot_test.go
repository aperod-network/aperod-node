package store

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestWalletReadSnapshotPinsWalletReadsAndReleases(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	txHash := crypto.Hash32{1}
	ki := crypto.KeyImage{}
	address := crypto.Address("snapshot-address")
	output := &StoredUTXO{TxHash: txHash, OutputIndex: 2, BlockHeight: 7}
	if err := db.PutUTXO(txHash, output.OutputIndex, output); err != nil {
		t.Fatal(err)
	}
	walletKey := lpodWalletPrefix(address)
	var suffix [12]byte
	binary.BigEndian.PutUint64(suffix[:8], output.BlockHeight)
	binary.BigEndian.PutUint32(suffix[8:], output.OutputIndex)
	walletKey = append(walletKey, suffix[:]...)
	walletKey = append(walletKey, make([]byte, 32+32)...)
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(walletKey, encodedOutput); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMeta("tip/hash", txHash[:]); err != nil {
		t.Fatal(err)
	}
	var tipHeight [8]byte
	binary.LittleEndian.PutUint64(tipHeight[:], 7)
	if err := db.PutMeta("tip/height", tipHeight[:]); err != nil {
		t.Fatal(err)
	}

	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()

	if err := db.DeleteUTXO(txHash, output.OutputIndex); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkUTXOSpent(txHash, output.OutputIndex); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkKeyImageSpent(ki); err != nil {
		t.Fatal(err)
	}
	newTxHash := crypto.Hash32{2}
	newOutput := &StoredUTXO{TxHash: newTxHash, OutputIndex: 3, BlockHeight: 8}
	if err := db.PutUTXO(newTxHash, newOutput.OutputIndex, newOutput); err != nil {
		t.Fatal(err)
	}
	newWalletKey := lpodWalletPrefix(address)
	binary.BigEndian.PutUint64(suffix[:8], newOutput.BlockHeight)
	binary.BigEndian.PutUint32(suffix[8:], newOutput.OutputIndex)
	newWalletKey = append(newWalletKey, suffix[:]...)
	newWalletKey = append(newWalletKey, make([]byte, 32)...)
	newWalletKey = append(newWalletKey, newTxHash[:]...)
	newEncodedOutput, err := json.Marshal(newOutput)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(newWalletKey, newEncodedOutput); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMeta("tip/hash", []byte("new canonical tip")); err != nil {
		t.Fatal(err)
	}

	hash, height, err := read.GetTip()
	if err != nil || hash != txHash || height != 7 {
		t.Fatalf("snapshot tip=(%x,%d), err=%v; want (%x,7)", hash, height, err, txHash)
	}
	got, err := read.GetUTXO(txHash, output.OutputIndex)
	if err != nil || got == nil || got.BlockHeight != output.BlockHeight {
		t.Fatalf("snapshot GetUTXO=%+v err=%v; want original output", got, err)
	}
	spent, err := read.IsUTXOSpentChecked(txHash, output.OutputIndex)
	if err != nil || spent {
		t.Fatalf("snapshot spent marker=(%v,%v), want false", spent, err)
	}
	kiSpent, err := read.IsKeyImageSpent(ki)
	if err != nil || kiSpent {
		t.Fatalf("snapshot key image marker=(%v,%v), want false", kiSpent, err)
	}
	rows, _, err := read.LPoDWalletOutputs(address, "", 128)
	if err != nil || len(rows) != 1 {
		t.Fatalf("snapshot wallet outputs=%d err=%v; want one unspent output", len(rows), err)
	}
	rows, _, err = db.LPoDWalletOutputs(address, "", 128)
	if err != nil || len(rows) != 1 || rows[0].TxHash != newTxHash {
		t.Fatalf("live wallet outputs=%+v err=%v; want only newly-added output", rows, err)
	}

	read.Release()
	read.Release()
}

func TestWalletReadSnapshotPinsLPoDEarningsPagesAndReadiness(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	address := crypto.Address("earnings-snapshot-address")
	funding := crypto.Hash32{1}
	firstBlock := crypto.Hash32{2}
	secondBlock := crypto.Hash32{3}
	oldTip := secondBlock
	putEarning := func(blockHash, txHash crypto.Hash32, height uint64, amount uint64) {
		t.Helper()
		output := StoredUTXO{
			TxHash: txHash, OutputIndex: 0, BlockHeight: height, AmountNAPRO: amount,
		}
		raw, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		key := append([]byte{}, lpodEarningsPrefix(address)...)
		var suffix [76]byte
		binary.BigEndian.PutUint64(suffix[:8], height)
		copy(suffix[12:44], blockHash[:])
		copy(suffix[44:], txHash[:])
		key = append(key, suffix[:]...)
		if err := db.put(key, raw); err != nil {
			t.Fatal(err)
		}
	}
	putEarning(firstBlock, crypto.Hash32{4}, 10, 100)
	putEarning(secondBlock, crypto.Hash32{5}, 11, 200)
	if err := db.put(lpodEarningsReadyKey, earningsReadyValue(funding, oldTip, 11)); err != nil {
		t.Fatal(err)
	}

	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()

	newTip := crypto.Hash32{6}
	putEarning(newTip, crypto.Hash32{7}, 12, 300)
	if err := db.put(lpodEarningsReadyKey, earningsReadyValue(funding, newTip, 12)); err != nil {
		t.Fatal(err)
	}

	ready, err := read.LPoDEarningsIndexReady(funding, oldTip, 11)
	if err != nil || !ready {
		t.Fatalf("pinned earnings readiness=(%v,%v), want ready at captured tip", ready, err)
	}
	ready, err = read.LPoDEarningsIndexReady(funding, newTip, 12)
	if err != nil || ready {
		t.Fatalf("pinned earnings readiness=(%v,%v), want false for live tip", ready, err)
	}

	firstPage, cursor, err := read.LPoDEarningsOutputs(address, "", 1)
	if err != nil || len(firstPage) != 1 || firstPage[0].TxHash != (crypto.Hash32{4}) || cursor == "" {
		t.Fatalf("first pinned earnings page=%+v cursor=%q err=%v", firstPage, cursor, err)
	}
	secondPage, next, err := read.LPoDEarningsOutputs(address, cursor, 1)
	if err != nil || len(secondPage) != 1 || secondPage[0].TxHash != (crypto.Hash32{5}) || next != "" {
		t.Fatalf("second pinned earnings page=%+v next=%q err=%v", secondPage, next, err)
	}
	liveRows, _, err := db.LPoDEarningsOutputs(address, "", 128)
	if err != nil || len(liveRows) != 3 {
		t.Fatalf("live earnings index rows=%d err=%v, want newly-added third row", len(liveRows), err)
	}
}

func TestWalletReadSnapshotLPoDEarningsJoinUsesOnlyExactUnspentOutputs(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	address := crypto.Address("earnings-join-address")
	blockHash := crypto.Hash32{9}
	reward := StoredUTXO{
		TxHash: crypto.Hash32{1}, OutputIndex: 0, BlockHeight: 7,
		OneTimePub: crypto.Point32{1}, TxPubKey: crypto.Point32{2},
		AmountCommit: crypto.Commitment{3}, EncAmount: [8]byte{4},
	}
	spentReward := StoredUTXO{
		TxHash: crypto.Hash32{5}, OutputIndex: 1, BlockHeight: 7,
		OneTimePub: crypto.Point32{6}, TxPubKey: crypto.Point32{7},
		AmountCommit: crypto.Commitment{8}, EncAmount: [8]byte{9},
	}
	ordinary := StoredUTXO{
		TxHash: crypto.Hash32{10}, OutputIndex: 2, BlockHeight: 7,
		OneTimePub: crypto.Point32{11}, TxPubKey: crypto.Point32{12},
		AmountCommit: crypto.Commitment{13}, EncAmount: [8]byte{14},
	}
	batch := new(leveldb.Batch)
	addWalletRow := func(output StoredUTXO, earningsAmount uint64, spent bool) {
		t.Helper()
		key := append([]byte{}, lpodWalletPrefix(address)...)
		var suffix [76]byte
		binary.BigEndian.PutUint64(suffix[:8], output.BlockHeight)
		binary.BigEndian.PutUint32(suffix[8:12], output.OutputIndex)
		copy(suffix[12:44], blockHash[:])
		copy(suffix[44:], output.TxHash[:])
		key = append(key, suffix[:]...)
		raw, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		batch.Put(key, raw)
		batch.Put(heightKey(output.BlockHeight), blockHash[:])
		if earningsAmount > 0 {
			earning := output
			earning.AmountNAPRO = earningsAmount
			earningKey, err := lpodEarningsKeyForWalletIndexKey(key)
			if err != nil {
				t.Fatal(err)
			}
			earningRaw, err := json.Marshal(earning)
			if err != nil {
				t.Fatal(err)
			}
			batch.Put(earningKey, earningRaw)
		}
		if spent {
			batch.Put(spentUTXOKey(output.TxHash, output.OutputIndex), []byte{1})
		}
	}
	addWalletRow(reward, 45000000, false)
	addWalletRow(spentReward, 36000000, true)
	addWalletRow(ordinary, 0, false)
	if err := db.db.Write(batch, nil); err != nil {
		t.Fatal(err)
	}

	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()

	rows, next, _, err := read.LPoDWalletOutputsAfterHeightWithEarnings(
		address, 0, false, "", 7, 128,
	)
	if err != nil || next != "" || len(rows) != 2 {
		t.Fatalf("snapshot earnings join rows=%d next=%q err=%v; want two unspent outputs", len(rows), next, err)
	}
	got := make(map[crypto.Hash32]uint64, len(rows))
	for _, row := range rows {
		got[row.TxHash] = row.AmountNAPRO
	}
	if got[reward.TxHash] != 45000000 {
		t.Fatalf("exact reward amount=%d, want 45000000", got[reward.TxHash])
	}
	if got[ordinary.TxHash] != 0 {
		t.Fatalf("ordinary wallet output was classified as reward: %d", got[ordinary.TxHash])
	}
	if _, found := got[spentReward.TxHash]; found {
		t.Fatal("spent reward was returned or enriched")
	}
}

func TestWalletReadSnapshotLPoDEarningsJoinRejectsConflictingIndexRow(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	address := crypto.Address("earnings-join-conflict-address")
	blockHash := crypto.Hash32{3}
	output := StoredUTXO{
		TxHash: crypto.Hash32{4}, OutputIndex: 0, BlockHeight: 2,
		OneTimePub: crypto.Point32{5}, TxPubKey: crypto.Point32{6},
		AmountCommit: crypto.Commitment{7}, EncAmount: [8]byte{8},
	}
	key := append([]byte{}, lpodWalletPrefix(address)...)
	var suffix [76]byte
	binary.BigEndian.PutUint64(suffix[:8], output.BlockHeight)
	copy(suffix[12:44], blockHash[:])
	copy(suffix[44:], output.TxHash[:])
	key = append(key, suffix[:]...)
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(key, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.put(heightKey(output.BlockHeight), blockHash[:]); err != nil {
		t.Fatal(err)
	}
	conflicting := output
	conflicting.OneTimePub = crypto.Point32{99}
	conflicting.AmountNAPRO = 45000000
	earningKey, err := lpodEarningsKeyForWalletIndexKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(earningKey, raw); err != nil {
		t.Fatal(err)
	}
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()

	if _, _, _, err := read.LPoDWalletOutputsAfterHeightWithEarnings(
		address, 0, false, "", 2, 128,
	); err == nil {
		t.Fatal("conflicting reward-index output was accepted")
	}
}

func BenchmarkLPoDWalletOutputRewardJoin30kSpentHistory(b *testing.B) {
	const total = 30000
	const active = 10
	const pageSize = 128

	db, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	address := crypto.Address("benchmark-large-reward-wallet")
	walletPrefix := lpodWalletPrefix(address)
	earningsPrefix := lpodEarningsPrefix(address)
	batch := new(leveldb.Batch)
	for i := 1; i <= total; i++ {
		height := uint64(i)
		var blockHash, txHash crypto.Hash32
		binary.BigEndian.PutUint64(blockHash[:8], height)
		binary.BigEndian.PutUint64(txHash[:8], height)
		txHash[31] = 1
		output := StoredUTXO{
			TxHash: txHash, OutputIndex: 0, BlockHeight: height,
			OneTimePub: crypto.Point32{1}, TxPubKey: crypto.Point32{2},
			AmountCommit: crypto.Commitment{3}, EncAmount: [8]byte{4},
		}
		var suffix [76]byte
		binary.BigEndian.PutUint64(suffix[:8], height)
		copy(suffix[12:44], blockHash[:])
		copy(suffix[44:], txHash[:])
		walletKey := append(append([]byte{}, walletPrefix...), suffix[:]...)
		walletRaw, err := json.Marshal(output)
		if err != nil {
			b.Fatal(err)
		}
		batch.Put(walletKey, walletRaw)
		earning := output
		earning.AmountNAPRO = 1
		earningKey := append(append([]byte{}, earningsPrefix...), suffix[:]...)
		earningRaw, err := json.Marshal(earning)
		if err != nil {
			b.Fatal(err)
		}
		batch.Put(earningKey, earningRaw)
		batch.Put(heightKey(height), blockHash[:])
		if i <= total-active {
			batch.Put(spentUTXOKey(txHash, 0), []byte{1})
		}
	}
	if err := db.db.Write(batch, nil); err != nil {
		b.Fatal(err)
	}

	walletPages := (total + pageSize - 1) / pageSize
	earningsPages := (total + pageSize - 1) / pageSize
	b.Run("legacy-sequential-index-scan", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(float64(walletPages), "wallet_pages/op")
		b.ReportMetric(float64(earningsPages), "earnings_pages/op")
		for i := 0; i < b.N; i++ {
			read, err := db.NewWalletReadSnapshot()
			if err != nil {
				b.Fatal(err)
			}
			cursor := ""
			for {
				_, next, _, err := read.LPoDWalletOutputsAfterHeight(address, 0, false, cursor, total, pageSize)
				if err != nil {
					b.Fatal(err)
				}
				if next == "" {
					break
				}
				cursor = next
			}
			cursor = ""
			for {
				_, next, err := read.LPoDEarningsOutputs(address, cursor, pageSize)
				if err != nil {
					b.Fatal(err)
				}
				if next == "" {
					break
				}
				cursor = next
			}
			read.Release()
		}
	})
	b.Run("inline-active-output-join", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(float64(walletPages), "wallet_pages/op")
		b.ReportMetric(float64(active), "reward_gets/op")
		b.ReportMetric(0, "earnings_pages/op")
		for i := 0; i < b.N; i++ {
			read, err := db.NewWalletReadSnapshot()
			if err != nil {
				b.Fatal(err)
			}
			cursor := ""
			var returned int
			for {
				rows, next, _, err := read.LPoDWalletOutputsAfterHeightWithEarnings(
					address, 0, false, cursor, total, pageSize,
				)
				if err != nil {
					b.Fatal(err)
				}
				returned += len(rows)
				for _, row := range rows {
					if row.AmountNAPRO != 1 {
						b.Fatalf("inline reward amount=%d, want 1", row.AmountNAPRO)
					}
				}
				if next == "" {
					break
				}
				cursor = next
			}
			if returned != active {
				b.Fatalf("inline wallet output count=%d, want %d", returned, active)
			}
			read.Release()
		}
	})
}

func TestReadCanonicalBlockBoundedRejectsMissingAndMalformedBodies(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	block := &core.Block{Header: core.BlockHeader{
		Height: 0, MerkleRoot: core.MerkleRoot(nil), Timestamp: 1,
	}}
	hash := block.Hash()
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(append(append([]byte{}, prefixBlock...), hash[:]...), raw); err != nil {
		t.Fatal(err)
	}
	if err := db.put(heightKey(0), hash[:]); err != nil {
		t.Fatal(err)
	}
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	got, err := read.ReadCanonicalBlockBounded(0, 1024)
	if err != nil || got.Header.Height != 0 {
		t.Fatalf("read valid empty canonical block: block=%+v err=%v", got, err)
	}
	if _, err := read.ReadCanonicalBlockBounded(0, 1); err == nil {
		t.Fatal("accepted a canonical body above the decode-byte limit")
	}
	read.Release()

	missingHash := crypto.Hash32{1}
	if err := db.put(heightKey(1), missingHash[:]); err != nil {
		t.Fatal(err)
	}
	read, err = db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read.ReadCanonicalBlockBounded(1, 1024); err == nil {
		t.Fatal("accepted a canonical height with no stored block body")
	}
	read.Release()

	bad := *block
	bad.Txs = []core.Transaction{{Version: core.TxVersionBase}}
	badRaw, err := json.Marshal(&bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.put(append(append([]byte{}, prefixBlock...), hash[:]...), badRaw); err != nil {
		t.Fatal(err)
	}
	read, err = db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read.ReadCanonicalBlockBounded(0, 1024); err == nil {
		t.Fatal("accepted block body that does not match its committed Merkle root")
	}
	read.Release()
}

func TestLPoDWalletOutputsResumeCursorSurvivesSnapshotRenewal(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	address := crypto.Address("resume-cursor-test")
	var parent crypto.Hash32
	blockHashes := make([]crypto.Hash32, 3)
	for height := uint64(0); height < 3; height++ {
		block := &core.Block{Header: core.BlockHeader{
			Height: height, PrevHash: parent, MerkleRoot: core.MerkleRoot(nil), Timestamp: int64(height + 1),
		}}
		hash := block.Hash()
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(hash, height, raw, nil, crypto.Hash32{}); err != nil {
			t.Fatal(err)
		}
		parent, blockHashes[height] = hash, hash
	}
	for height, indexes := range map[uint64][]uint32{1: {0, 1}, 2: {0}} {
		for _, index := range indexes {
			txHash := crypto.HashBytes([]byte{byte(height), byte(index), 91})
			output := StoredUTXO{TxHash: txHash, OutputIndex: index, BlockHeight: height}
			key := lpodWalletPrefix(address)
			var suffix [12]byte
			binary.BigEndian.PutUint64(suffix[:8], height)
			binary.BigEndian.PutUint32(suffix[8:], index)
			key = append(key, suffix[:]...)
			key = append(key, blockHashes[height][:]...)
			key = append(key, txHash[:]...)
			raw, err := json.Marshal(output)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.put(key, raw); err != nil {
				t.Fatal(err)
			}
			if height == 1 && index == 0 {
				if err := db.MarkUTXOSpent(txHash, index); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rows, next, resume, err := read.LPoDWalletOutputsAfterHeight(address, 0, false, "", 2, 1)
	read.Release()
	if err != nil || len(rows) != 0 || next == "" || resume != next {
		t.Fatalf("first page rows=%d next=%q resume=%q err=%v", len(rows), next, resume, err)
	}

	// A new read snapshot models a renewed short lease. The native address key
	// remains a valid continuation position independently of its old session.
	read, err = db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rows, next, resume, err = read.LPoDWalletOutputsAfterHeight(address, 0, false, resume, 2, 1)
	if err != nil || len(rows) != 1 || rows[0].BlockHeight != 1 || rows[0].OutputIndex != 1 || next == "" {
		read.Release()
		t.Fatalf("renewed page rows=%+v next=%q resume=%q err=%v", rows, next, resume, err)
	}
	rows, _, _, err = read.LPoDWalletOutputsAfterHeight(address, 0, false, resume, 1, 10)
	read.Release()
	if err != nil || len(rows) != 0 {
		t.Fatalf("through-height bound leaked later rows: rows=%+v err=%v", rows, err)
	}

	otherHash := crypto.Hash32{99}
	if err := db.db.Put(heightKey(1), otherHash[:], nil); err != nil {
		t.Fatal(err)
	}
	read, err = db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = read.LPoDWalletOutputsAfterHeight(address, 0, false, resume, 2, 1)
	read.Release()
	if _, ok := err.(*WalletCursorReorgError); !ok {
		t.Fatalf("noncanonical resume cursor error=%v, want WalletCursorReorgError", err)
	}

	if err := db.db.Delete(heightKey(1), nil); err != nil {
		t.Fatal(err)
	}
	read, err = db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = read.LPoDWalletOutputsAfterHeight(address, 0, false, resume, 2, 1)
	read.Release()
	if _, ok := err.(*WalletReconciliationError); !ok {
		t.Fatalf("missing canonical cursor height error=%v, want WalletReconciliationError", err)
	}
}

func TestWalletReadSnapshotCheckpointBudgetBeforeDecode(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash := crypto.Hash32{9}
	if err := db.put(lpodKey(hash), []byte("not json and larger than the limit")); err != nil {
		t.Fatal(err)
	}
	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()
	if _, err := read.LoadLPoDCheckpointAtBounded(hash, 1); err == nil ||
		err.Error() != "store: checkpoint exceeds auxiliary read budget" {
		t.Fatalf("bounded checkpoint read error=%v, want budget error before decode", err)
	}
}

func TestWalletReadSnapshotCheckpointBudgetIncludesFundingCheckpoint(t *testing.T) {
	db, _, migration, priv, address := trustedLPoDV3FundingFixture(t)
	defer db.Close()

	funding, settlement, raw := lpodActivationBlock(t, db, migration, priv, address)
	if err := db.CommitRawBlockWithAVM(
		funding.Hash(), funding.Header.Height, raw, nil, crypto.Hash32{}, settlement,
	); err != nil {
		t.Fatalf("commit funding block: %v", err)
	}
	child, _, _, _ := commitLPoDStoreTestBlock(t, db, migration, priv, address)

	fundingData, err := db.get(lpodKey(funding.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	childData, err := db.get(lpodKey(child.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	budget := len(childData) + len(fundingData) - 1
	if len(childData) == 0 || len(fundingData) == 0 ||
		len(childData) > budget || len(fundingData) <= budget-len(childData) {
		t.Fatalf("test checkpoints do not exceed the cumulative budget: child=%d funding=%d budget=%d",
			len(childData), len(fundingData), budget)
	}

	read, err := db.NewWalletReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()
	if checkpoint, err := read.LoadLPoDCheckpointAt(child.Hash()); err != nil || checkpoint == nil {
		t.Fatalf("unbounded recursive checkpoint read failed: checkpoint=%v err=%v", checkpoint, err)
	}
	if _, err := read.LoadLPoDCheckpointAtBounded(child.Hash(), budget); err == nil ||
		err.Error() != errLPoDCheckpointReadBudget.Error() {
		t.Fatalf("bounded recursive checkpoint read error=%v, want cumulative budget error", err)
	}
}

func TestCommitRawBlockWithAVMCommitsKeyImagesAtomically(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ki := crypto.KeyImage{}
	block := &core.Block{
		Header: core.BlockHeader{Height: 7},
		Txs: []core.Transaction{{
			Inputs: []core.RingInput{{KeyImage: ki}},
		}},
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	hash := block.Hash()
	if err := db.CommitRawBlockWithAVM(hash, block.Header.Height, raw,
		[]AVMWrite{{Value: []byte("invalid empty-key write")}}, crypto.Hash32{}); err == nil {
		t.Fatal("expected invalid AVM write to abort the complete canonical batch")
	}
	if got, found, err := db.GetCanonicalHash(block.Header.Height); err != nil || found || got != (crypto.Hash32{}) {
		t.Fatalf("failed commit left canonical height index: hash=%x found=%v err=%v", got, found, err)
	}
	if spent, err := db.IsKeyImageSpent(ki); err != nil || spent {
		t.Fatalf("failed commit left key-image marker: spent=%v err=%v", spent, err)
	}
	if err := db.CommitRawBlockWithAVM(hash, block.Header.Height, raw,
		[]AVMWrite{{Key: []byte("atomic-test"), Value: []byte("committed")}}, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	if got, found, err := db.GetCanonicalHash(block.Header.Height); err != nil || !found || got != hash {
		t.Fatalf("canonical height index=(%x,%v), err=%v; want %x", got, found, err, hash)
	}
	if spent, err := db.IsKeyImageSpent(ki); err != nil || !spent {
		t.Fatalf("canonical key-image marker=(%v,%v), want spent", spent, err)
	}
	if value, found, err := db.GetAVMState([]byte("atomic-test")); err != nil || !found || string(value) != "committed" {
		t.Fatalf("AVM state=(%q,%v), err=%v; want committed", value, found, err)
	}
}
