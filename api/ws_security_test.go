package api

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

func newSecurityTestHub() *Hub {
	return &Hub{
		clients:   make(map[*wsClient]struct{}),
		connPerIP: make(map[string]int),
		log:       slog.Default(),
	}
}

func TestHubRegisterAtomicallyEnforcesConnectionCaps(t *testing.T) {
	hub := newSecurityTestHub()
	var accepted atomic.Int32
	var clientsMu sync.Mutex
	var clients []*wsClient
	var wg sync.WaitGroup

	for i := 0; i < MaxWSClientsPerIP*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &wsClient{send: make(chan WSEvent, 1), ip: "203.0.113.10"}
			if hub.register(client) {
				accepted.Add(1)
				clientsMu.Lock()
				clients = append(clients, client)
				clientsMu.Unlock()
			}
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != MaxWSClientsPerIP {
		t.Fatalf("concurrent registrations accepted = %d, want %d", got, MaxWSClientsPerIP)
	}
	if got := hub.ClientCount(); got != MaxWSClientsPerIP {
		t.Fatalf("client count = %d, want %d", got, MaxWSClientsPerIP)
	}

	for _, client := range clients {
		hub.unregister(client)
	}

	globalHub := newSecurityTestHub()
	for i := 0; i < MaxWSClients; i++ {
		client := &wsClient{send: make(chan WSEvent, 1), ip: fmt.Sprintf("198.51.100.%d", i)}
		if !globalHub.register(client) {
			t.Fatalf("registration %d unexpectedly rejected before global cap", i)
		}
	}
	if globalHub.register(&wsClient{send: make(chan WSEvent, 1), ip: "192.0.2.1"}) {
		t.Fatal("registration beyond global cap was accepted")
	}
}

func TestHubBroadcastDoesNotSendToClosedClientChannel(t *testing.T) {
	hub := newSecurityTestHub()
	client := &wsClient{send: make(chan WSEvent, 64), ip: "203.0.113.11"}
	if !hub.register(client) {
		t.Fatal("failed to register test client")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 10000; i++ {
			hub.broadcast(WSEvent{Topic: "test"})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		hub.unregister(client)
	}()
	close(start)
	wg.Wait()

	if got := hub.ClientCount(); got != 0 {
		t.Fatalf("client count after unregister = %d, want 0", got)
	}
}
