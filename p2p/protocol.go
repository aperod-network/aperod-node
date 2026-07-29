// Package p2p implements the Aperod peer-to-peer networking layer.
// Uses a simple length-prefixed JSON framing over TCP.
// Replace with libp2p for production once core is stable.
package p2p

import (
        "encoding/binary"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "time"

        "github.com/aperod/aperod/crypto"
)

// MessageType identifies the purpose of a network message.
type MessageType uint8

const (
        MsgPing        MessageType = 0x01
        MsgPong        MessageType = 0x02
        MsgGetHeaders  MessageType = 0x10
        MsgHeaders     MessageType = 0x11
        MsgGetBlock    MessageType = 0x12
        MsgBlock       MessageType = 0x13
        MsgTx          MessageType = 0x20
        MsgVote        MessageType = 0x30
        MsgGetPeers    MessageType = 0x40
        MsgPeers       MessageType = 0x41
)

// MaxMessageSize is the maximum allowed message size (10 MB).
const MaxMessageSize = 10 * 1024 * 1024

// DialTimeout for outbound connections.
const DialTimeout = 5 * time.Second

// ReadTimeout for incoming messages.
// ReadTimeout for incoming messages — tight to prevent Slowloris-style stalls.
const ReadTimeout = 5 * time.Second

// WriteTimeout for outbound writes — prevents a slow peer from blocking the goroutine.
const WriteTimeout = 5 * time.Second

// Envelope wraps every network message with a type tag and length-prefixed body.
type Envelope struct {
        Type    MessageType `json:"t"`
        Payload []byte      `json:"p"`
}

// ─── Message payloads ─────────────────────────────────────────────────────────

// PingMsg is sent on connect and periodically to maintain the connection.
type PingMsg struct {
        NodeID    string `json:"node_id"`
        Height    uint64 `json:"height"`
        UserAgent string `json:"ua"`
        Timestamp int64  `json:"ts"`
}

// PeersMsg is the response to MsgGetPeers.
type PeersMsg struct {
        Addrs []string `json:"addrs"` // "host:port" list
}

// GetHeadersMsg requests headers starting after KnownHashes (up to Limit).
type GetHeadersMsg struct {
        KnownHashes []crypto.Hash32 `json:"known"` // tail of local chain
        Limit       int             `json:"limit"`
}

// HeadersMsg contains a slice of serialized block headers.
type HeadersMsg struct {
        Headers []SerializedHeader `json:"headers"`
}

// SerializedHeader is a JSON-friendly block header.
type SerializedHeader struct {
        Height       uint64   `json:"height"`
        Hash         [32]byte `json:"hash"`
        PrevHash     [32]byte `json:"prev_hash"`
        MerkleRoot   [32]byte `json:"merkle_root"`
        Timestamp    int64    `json:"timestamp"`
        Round        uint32   `json:"round"`
        ValidatorPub []byte   `json:"validator_pub"`
        Signature    []byte   `json:"sig"`
}

// GetBlockMsg requests a specific block by hash.
type GetBlockMsg struct {
        Hash crypto.Hash32 `json:"hash"`
}

// VoteMsg carries a consensus finalization vote.
type VoteMsg struct {
        BlockHash    crypto.Hash32 `json:"block_hash"`
        Height       uint64        `json:"height"`
        ValidatorPub []byte        `json:"validator_pub"`
        Signature    []byte        `json:"sig"`
}

// ─── Framing ──────────────────────────────────────────────────────────────────

// WriteMsg encodes and sends a message over conn.
// Format: [4-byte big-endian length][msgType byte][JSON body]
func WriteMsg(conn net.Conn, msgType MessageType, payload interface{}) error {
        return writeMsg(conn, msgType, payload)
}

// ReadMsg reads the next message from conn (exported alias for tests).
func ReadMsg(conn net.Conn) (MessageType, []byte, error) {
        return readMsg(conn)
}

func writeMsg(conn net.Conn, msgType MessageType, payload interface{}) error {
        conn.SetWriteDeadline(time.Now().Add(WriteTimeout)) //nolint:errcheck
        data, err := json.Marshal(payload)
        if err != nil {
                return fmt.Errorf("marshal: %w", err)
        }

        // [1 byte type][payload]
        body := append([]byte{byte(msgType)}, data...)

        if len(body) > MaxMessageSize {
                return fmt.Errorf("message too large: %d bytes", len(body))
        }

        // 4-byte length prefix
        header := make([]byte, 4)
        binary.BigEndian.PutUint32(header, uint32(len(body)))

        if _, err := conn.Write(header); err != nil {
                return err
        }
        _, err = conn.Write(body)
        return err
}

// readMsg reads the next message from conn.
func readMsg(conn net.Conn) (MessageType, []byte, error) {
        conn.SetReadDeadline(time.Now().Add(ReadTimeout))

        var lenBuf [4]byte
        if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
                return 0, nil, fmt.Errorf("read length: %w", err)
        }
        length := binary.BigEndian.Uint32(lenBuf[:])
        if length == 0 || length > MaxMessageSize {
                return 0, nil, fmt.Errorf("invalid message length: %d", length)
        }

        body := make([]byte, length)
        if _, err := io.ReadFull(conn, body); err != nil {
                return 0, nil, fmt.Errorf("read body: %w", err)
        }

        msgType := MessageType(body[0])
        return msgType, body[1:], nil
}

// unmarshal decodes a JSON payload into v.
func unmarshal(data []byte, v interface{}) error {
        return json.Unmarshal(data, v)
}

// Unmarshal is an exported alias for unmarshal used in tests.
func Unmarshal(data []byte, v interface{}) error { return unmarshal(data, v) }
