package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func syntheticAddressHistoryBlock(height uint64, spendPub crypto.Point32, match bool) *core.Block {
	var txs []core.Transaction
	if match {
		txs = []core.Transaction{core.CoinbaseTx(spendPub, 1)}
	}
	return &core.Block{
		Header: core.BlockHeader{Height: height, MerkleRoot: core.MerkleRoot(txs)},
		Txs:    txs,
	}
}

func syntheticAddressHistorySpendPub(t *testing.T) crypto.Point32 {
	t.Helper()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	return keys.Spend.Public
}

func TestParseAddressHistoryQueryEnforcesMaximumPageLimit(t *testing.T) {
	for _, limit := range []string{"1", "200"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/address/test/transactions?limit="+limit, nil)
		query, err := parseAddressHistoryQuery(request)
		if err != nil {
			t.Errorf("limit %s rejected: %v", limit, err)
			continue
		}
		if got := query.limit; got < 1 || got > 200 {
			t.Errorf("limit %s parsed as %d, outside the allowed range", limit, got)
		}
	}

	for _, limit := range []string{"0", "201"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/address/test/transactions?limit="+limit, nil)
		if _, err := parseAddressHistoryQuery(request); err == nil {
			t.Errorf("limit %s accepted; want an error", limit)
		}
	}
}

func TestScanAddressHistoryRejectsOversizedBlock(t *testing.T) {
	spendPub := syntheticAddressHistorySpendPub(t)
	reads := 0
	page := scanAddressHistoryBlocks(4, spendPub, 10,
		func(height uint64) (*core.Block, int, error) {
			reads++
			return syntheticAddressHistoryBlock(height, spendPub, false), maxAddressHistoryBlockBytes + 1, nil
		},
		func(uint64) (bool, error) { return true, nil },
		0,
	)

	if page.err == nil {
		t.Fatal("oversized block was accepted")
	}
	if reads != 1 {
		t.Fatalf("readBlock called %d times, want fail-fast after one block", reads)
	}
	if page.hasMore || page.nextStartHeight != 0 {
		t.Fatalf("oversized page exposed continuation: %+v", page)
	}
}

func TestScanAddressHistoryPageBudgetExactLimitAndOverflow(t *testing.T) {
	spendPub := syntheticAddressHistorySpendPub(t)
	const largestAcceptedPageBlocks = maxAddressHistoryPageBytes / maxAddressHistoryBlockBytes

	t.Run("exact budget is accepted", func(t *testing.T) {
		reads := 0
		page := scanAddressHistoryBlocks(largestAcceptedPageBlocks-1, spendPub, 200,
			func(height uint64) (*core.Block, int, error) {
				reads++
				return syntheticAddressHistoryBlock(height, spendPub, false), maxAddressHistoryBlockBytes, nil
			},
			func(uint64) (bool, error) { return true, nil },
			0,
		)
		if page.err != nil {
			t.Fatalf("exact page budget rejected: %v", page.err)
		}
		if reads != largestAcceptedPageBlocks {
			t.Fatalf("read %d blocks, want %d", reads, largestAcceptedPageBlocks)
		}
		if page.hasMore {
			t.Fatalf("genesis page unexpectedly has continuation: %+v", page)
		}
	})

	t.Run("one byte over budget is rejected", func(t *testing.T) {
		reads := 0
		page := scanAddressHistoryBlocks(largestAcceptedPageBlocks, spendPub, 200,
			func(height uint64) (*core.Block, int, error) {
				reads++
				return syntheticAddressHistoryBlock(height, spendPub, false), maxAddressHistoryBlockBytes, nil
			},
			func(uint64) (bool, error) { return true, nil },
			0,
		)
		if page.err == nil {
			t.Fatal("page exceeding the aggregate byte budget was accepted")
		}
		if reads != largestAcceptedPageBlocks+1 {
			t.Fatalf("read %d blocks, want rejection on block %d", reads, largestAcceptedPageBlocks+1)
		}
	})
}

func TestScanAddressHistoryHardCapsHeightAttemptsAndAdvancesCursor(t *testing.T) {
	spendPub := syntheticAddressHistorySpendPub(t)
	reads := 0
	page := scanAddressHistoryBlocks(2_000, spendPub, 200,
		func(height uint64) (*core.Block, int, error) {
			reads++
			return syntheticAddressHistoryBlock(height, spendPub, false), 1, nil
		},
		func(uint64) (bool, error) { return true, nil },
		0, // Deliberately wider than production's normal pageEnd calculation.
	)

	if page.err != nil {
		t.Fatalf("bounded scan failed: %v", page.err)
	}
	if reads != maxAddressHistoryScanBlocks {
		t.Fatalf("read %d heights, want the hard cap of %d", reads, maxAddressHistoryScanBlocks)
	}
	if !page.hasMore || !page.scanLimited || page.nextStartHeight != 1_000 {
		t.Fatalf("bounded scan continuation = %+v, want next height 1000", page)
	}
}

func TestScanAddressHistoryFollowingCursorsDoesNotRepeatRanges(t *testing.T) {
	spendPub := syntheticAddressHistorySpendPub(t)
	matchingHeights := map[uint64]bool{10: true, 6: true}
	seen := make(map[uint64]int)
	read := func(height uint64) (*core.Block, int, error) {
		seen[height]++
		return syntheticAddressHistoryBlock(height, spendPub, matchingHeights[height]), 1, nil
	}
	exists := func(uint64) (bool, error) { return true, nil }

	start := uint64(10)
	for _, wantNext := range []uint64{9, 5} {
		page := scanAddressHistoryBlocks(start, spendPub, 1, read, exists, 0)
		if page.err != nil {
			t.Fatalf("scan from %d failed: %v", start, page.err)
		}
		if !page.hasMore || page.nextStartHeight != wantNext || page.nextStartHeight >= start {
			t.Fatalf("cursor from %d = %+v, want strict backward cursor %d", start, page, wantNext)
		}
		start = page.nextStartHeight
	}

	last := scanAddressHistoryBlocks(start, spendPub, 1, read, exists, 0)
	if last.err != nil {
		t.Fatalf("terminal scan from %d failed: %v", start, last.err)
	}
	if last.hasMore || last.scanLimited {
		t.Fatalf("terminal scan unexpectedly continues: %+v", last)
	}
	for height, count := range seen {
		if count != 1 {
			t.Errorf("following returned cursors scanned height %d %d times", height, count)
		}
	}
}

func TestAddressHistoryConcurrentScanLimitFailsFast(t *testing.T) {
	if got := len(addressHistoryScanSlots); got != 0 {
		t.Fatalf("history scan slots not empty before test: %d", got)
	}

	const contenders = 32
	start := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var attempted sync.WaitGroup
	var finished sync.WaitGroup
	var admitted atomic.Int32
	var active atomic.Int32
	var peak atomic.Int32
	attempted.Add(contenders)
	finished.Add(contenders)

	for range contenders {
		go func() {
			defer finished.Done()
			<-start
			if tryAcquireAddressHistoryScanSlot() {
				admitted.Add(1)
				now := active.Add(1)
				for prior := peak.Load(); now > prior && !peak.CompareAndSwap(prior, now); prior = peak.Load() {
				}
				attempted.Done()
				<-release
				active.Add(-1)
				releaseAddressHistoryScanSlot()
				return
			}
			attempted.Done()
		}()
	}
	cleanup := func() {
		releaseOnce.Do(func() { close(release) })
		finished.Wait()
	}
	defer cleanup()

	close(start)
	attempted.Wait()
	if got := admitted.Load(); got != maxConcurrentAddressHistoryScans {
		t.Fatalf("admitted %d scans, want exactly %d", got, maxConcurrentAddressHistoryScans)
	}
	if got := peak.Load(); got > maxConcurrentAddressHistoryScans {
		t.Fatalf("peak concurrent scans = %d, want at most %d", got, maxConcurrentAddressHistoryScans)
	}
	if got := len(addressHistoryScanSlots); got != maxConcurrentAddressHistoryScans {
		t.Fatalf("occupied scan slots = %d, want %d", got, maxConcurrentAddressHistoryScans)
	}

	server := NewServer(":0", core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()), core.NewUTXOSet(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/address/test/transactions?limit=50", nil)
	response := httptest.NewRecorder()
	server.restAddressTransactionsPage(response, request, "test", crypto.Point32{})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-capacity status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.Header().Get("Retry-After"))
	}
}
