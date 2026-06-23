package api

// WebSocket event hub for Phase 2 (block 2.2).
//
// Clients connect to GET /ws and subscribe to one or more topics:
//   new_block           — emitted when a new block is finalized
//   new_transaction     — emitted when a tx enters the mempool
//   confirmed_transaction — emitted when a tx is included in a block
//
// Wire format (server → client):
//   {"topic":"new_block","data":{...BlockResponse...}}
//
// Client → server messages are ignored (subscribe-only push model).
// Heartbeat: server sends {"topic":"ping"} every 15 s; dead connections
// are pruned when the write fails.

import (
        "encoding/json"
        "fmt"
        "log/slog"
        "net/http"
        "sync"
        "time"

        "github.com/aperod/aperod/core"
        "golang.org/x/net/websocket"
)

// WSEvent is the envelope sent to every subscribed client.
type WSEvent struct {
        Topic string      `json:"topic"`
        Data  interface{} `json:"data,omitempty"`
}

// wsClient wraps a WebSocket connection with a send channel.
type wsClient struct {
        conn   *websocket.Conn
        send   chan WSEvent
        topics map[string]bool // subscribed topics ("" = all)
}

// Hub manages all active WebSocket connections.
type Hub struct {
        mu      sync.RWMutex
        clients map[*wsClient]struct{}
        log     *slog.Logger
}

// NewHub creates a Hub and starts the heartbeat goroutine.
func NewHub(log *slog.Logger) *Hub {
        h := &Hub{
                clients: make(map[*wsClient]struct{}),
                log:     log,
        }
        go h.heartbeat()
        return h
}

// BroadcastBlock sends a new_block event to all connected clients.
func (h *Hub) BroadcastBlock(b *core.Block) {
        h.broadcast(WSEvent{Topic: "new_block", Data: blockToResponse(b)})
}

// BroadcastTx sends a new_transaction event to all clients.
func (h *Hub) BroadcastTx(tx *core.Transaction) {
        hash := tx.Hash()
        h.broadcast(WSEvent{Topic: "new_transaction", Data: map[string]interface{}{
                "hash":    fmt.Sprintf("%x", hash[:]),
                "inputs":  len(tx.Inputs),
                "outputs": len(tx.Outputs),
                "fee":     tx.Fee,
                "size":    tx.Size(),
        }})
}

// BroadcastConfirmed sends a confirmed_transaction event.
func (h *Hub) BroadcastConfirmed(tx *core.Transaction, blockHeight uint64) {
        hash := tx.Hash()
        h.broadcast(WSEvent{Topic: "confirmed_transaction", Data: map[string]interface{}{
                "hash":         fmt.Sprintf("%x", hash[:]),
                "block_height": blockHeight,
        }})
}

func (h *Hub) broadcast(evt WSEvent) {
        h.mu.RLock()
        clients := make([]*wsClient, 0, len(h.clients))
        for c := range h.clients {
                clients = append(clients, c)
        }
        h.mu.RUnlock()

        for _, c := range clients {
                select {
                case c.send <- evt:
                default:
                        // Channel full — drop event for this slow client
                }
        }
}

// heartbeat pings all clients every 15 s to detect dead connections.
func (h *Hub) heartbeat() {
        ticker := time.NewTicker(15 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
                h.broadcast(WSEvent{Topic: "ping"})
        }
}

// ClientCount returns the number of connected WebSocket clients.
func (h *Hub) ClientCount() int {
        h.mu.RLock()
        defer h.mu.RUnlock()
        return len(h.clients)
}

// register adds a client to the hub.
func (h *Hub) register(c *wsClient) {
        h.mu.Lock()
        h.clients[c] = struct{}{}
        h.mu.Unlock()
}

// unregister removes a client and closes its send channel.
func (h *Hub) unregister(c *wsClient) {
        h.mu.Lock()
        delete(h.clients, c)
        h.mu.Unlock()
        close(c.send)
}

// Handler returns an http.Handler that upgrades HTTP to WebSocket.
func (h *Hub) Handler() http.Handler {
        return websocket.Handler(func(conn *websocket.Conn) {
                c := &wsClient{
                        conn:   conn,
                        send:   make(chan WSEvent, 64),
                        topics: map[string]bool{"new_block": true, "new_transaction": true, "confirmed_transaction": true},
                }
                h.register(c)
                defer h.unregister(c)

                h.log.Info("ws client connected", "addr", conn.RemoteAddr())

                // Write loop
                go func() {
                        enc := json.NewEncoder(conn)
                        for evt := range c.send {
                                conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
                                if err := enc.Encode(evt); err != nil {
                                        conn.Close()
                                        return
                                }
                        }
                }()

                // Read loop (drain & discard — subscribe-only model)
                buf := make([]byte, 512)
                for {
                        conn.SetReadDeadline(time.Now().Add(60 * time.Second))
                        if _, err := conn.Read(buf); err != nil {
                                return // client disconnected
                        }
                }
        })
}
