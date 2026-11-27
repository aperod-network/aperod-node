package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func scrapeMetrics(t *testing.T, server *api.Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", recorder.Code)
	}
	return recorder.Body.String()
}

func lockedTestValidatorKey(t *testing.T, private crypto.ValidatorPrivKey) *crypto.LockedValidatorKey {
	t.Helper()
	keyBytes := append([]byte(nil), private.Bytes()...)
	key, err := crypto.NewLockedValidatorKey(keyBytes, nil)
	crypto.ZeroBytes(keyBytes)
	if err != nil {
		t.Fatalf("NewLockedValidatorKey: %v", err)
	}
	t.Cleanup(key.Destroy)
	return key
}

func appendTestBlocks(t *testing.T, chain *core.Chain, count uint64, validator crypto.ValidatorPubKey) {
	t.Helper()
	tip := chain.Tip()
	if tip == nil {
		t.Fatal("test chain has no genesis")
	}
	prev := tip.Hash()
	for height := uint64(1); height <= count; height++ {
		block := &core.Block{Header: core.BlockHeader{
			Height:       height,
			PrevHash:     prev,
			ValidatorPub: validator,
		}}
		if err := chain.AddBlock(block); err != nil {
			t.Fatalf("append test block at height %d: %v", height, err)
		}
		prev = block.Hash()
	}
}

func TestMetrics_LocalValidatorOnlineRequiresKeyRegistryAndP2P(t *testing.T) {
	tests := []struct {
		name             string
		loadKey          bool
		registerLocalKey bool
		peerCount        int
		peerHeight       uint64
		wantKey          string
		wantActive       string
		wantP2P          string
		wantOnline       string
		wantSyncHealthy  string
		wantMaxLag       string
	}{
		{
			name:             "missing local key",
			registerLocalKey: true,
			peerCount:        1,
			peerHeight:       150,
			wantKey:          "aperod_validator_local_key_loaded 0",
			wantActive:       "aperod_validator_local_active 0",
			wantP2P:          "aperod_validator_p2p_connected 1",
			wantOnline:       "aperod_validator_online 0",
			wantSyncHealthy:  "aperod_validator_sync_healthy 0",
		},
		{
			name:            "local key not active in registry",
			loadKey:         true,
			peerCount:       1,
			peerHeight:      150,
			wantKey:         "aperod_validator_local_key_loaded 1",
			wantActive:      "aperod_validator_local_active 0",
			wantP2P:         "aperod_validator_p2p_connected 1",
			wantOnline:      "aperod_validator_online 0",
			wantSyncHealthy: "aperod_validator_sync_healthy 0",
		},
		{
			name:             "no P2P peer",
			loadKey:          true,
			registerLocalKey: true,
			wantKey:          "aperod_validator_local_key_loaded 1",
			wantActive:       "aperod_validator_local_active 1",
			wantP2P:          "aperod_validator_p2p_connected 0",
			wantOnline:       "aperod_validator_online 0",
			wantSyncHealthy:  "aperod_validator_sync_healthy 0",
		},
		{
			name:             "connected to a synced peer",
			loadKey:          true,
			registerLocalKey: true,
			peerCount:        1,
			peerHeight:       150,
			wantKey:          "aperod_validator_local_key_loaded 1",
			wantActive:       "aperod_validator_local_active 1",
			wantP2P:          "aperod_validator_p2p_connected 1",
			wantOnline:       "aperod_validator_online 1",
			wantSyncHealthy:  "aperod_validator_sync_healthy 1",
			wantMaxLag:       "aperod_peer_sync_lag_blocks_max 0",
		},
		{
			name:             "connected but peer is far behind",
			loadKey:          true,
			registerLocalKey: true,
			peerCount:        1,
			peerHeight:       20,
			wantKey:          "aperod_validator_local_key_loaded 1",
			wantActive:       "aperod_validator_local_active 1",
			wantP2P:          "aperod_validator_p2p_connected 1",
			wantOnline:       "aperod_validator_online 1",
			wantSyncHealthy:  "aperod_validator_sync_healthy 0",
			wantMaxLag:       "aperod_peer_sync_lag_blocks_max 130",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, chain := newTestServer(t)
			localPrivate, localPublic, err := crypto.GenerateValidatorKey()
			if err != nil {
				t.Fatalf("GenerateValidatorKey: %v", err)
			}
			registryPublic := localPublic
			if !test.registerLocalKey {
				_, registryPublic, err = crypto.GenerateValidatorKey()
				if err != nil {
					t.Fatalf("GenerateValidatorKey for registry: %v", err)
				}
			}
			registry := core.NewValidatorRegistry()
			registry.InitFromGenesis([]crypto.ValidatorPubKey{registryPublic}, core.MinStakeNAPR)
			server.SetRegistry(registry)
			if test.loadKey {
				server.SetValidatorKey(lockedTestValidatorKey(t, localPrivate))
			}

			appendTestBlocks(t, chain, 150, localPublic)
			server.SetPeerCounter(func() int { return test.peerCount })
			server.SetPeerListFunc(func() []api.PeerListEntry {
				if test.peerCount == 0 {
					return nil
				}
				return []api.PeerListEntry{{Addr: "test-peer", Height: test.peerHeight}}
			})

			metrics := scrapeMetrics(t, server)
			for _, want := range []string{
				test.wantKey,
				test.wantActive,
				test.wantP2P,
				test.wantOnline,
				test.wantSyncHealthy,
			} {
				if !strings.Contains(metrics, want) {
					t.Errorf("metrics missing %q:\n%s", want, metrics)
				}
			}
			if test.wantMaxLag != "" && !strings.Contains(metrics, test.wantMaxLag) {
				t.Errorf("metrics missing %q:\n%s", test.wantMaxLag, metrics)
			}
		})
	}
}

func TestMyValidatorEndpointReportsLiveNetworkStatus(t *testing.T) {
	server, chain := newTestServer(t)
	private, public, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{public}, core.MinStakeNAPR)
	server.SetRegistry(registry)
	server.SetValidatorKey(lockedTestValidatorKey(t, private))
	appendTestBlocks(t, chain, 2, public)
	server.SetPeerCounter(func() int { return 1 })
	server.SetPeerListFunc(func() []api.PeerListEntry {
		return []api.PeerListEntry{{Addr: "test-peer", Height: 2}}
	})

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/my-validator", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/my-validator status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		PubKey           string `json:"pub_key"`
		RegisteredActive bool   `json:"registered_active"`
		P2PConnected     bool   `json:"p2p_connected"`
		Online           bool   `json:"online"`
		SyncHealthy      bool   `json:"sync_healthy"`
		PeerCount        int    `json:"peer_count"`
		MaxPeerLagBlocks uint64 `json:"max_peer_lag_blocks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if response.PubKey != public.Hex() || !response.RegisteredActive || !response.P2PConnected ||
		!response.Online || !response.SyncHealthy || response.PeerCount != 1 || response.MaxPeerLagBlocks != 0 {
		t.Fatalf("unexpected live validator status: %+v", response)
	}
}
