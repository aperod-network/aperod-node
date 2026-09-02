package p2p

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

type admissionTestHandler struct{}

func (admissionTestHandler) OnBlock(*core.Block)                   {}
func (admissionTestHandler) OnTransaction(*core.Transaction)       {}
func (admissionTestHandler) OnVote(VoteMsg)                        {}
func (admissionTestHandler) CurrentHeight() uint64                 { return 0 }
func (admissionTestHandler) CurrentTailHashes(int) []crypto.Hash32 { return nil }
func (admissionTestHandler) GetBlock(crypto.Hash32) *core.Block    { return nil }

type admissionRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c *admissionRemoteConn) RemoteAddr() net.Addr { return c.remote }

func newAdmissionHost(cfg Config) *Host {
	return NewHost(cfg, admissionTestHandler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitAdmissionSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestOutboundAdmissionReservationThroughRegistration verifies both outbound
// caps while all successful handshakes are paused immediately before final
// registration. The dial goroutines have already returned at that point, so
// only a reservation retained by handleConn can prevent later dials from
// entering the handshake.
func TestOutboundAdmissionReservationThroughRegistration(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		targetIP func(int) net.IP
	}{
		{
			name: "MaxPeers",
			cfg:  Config{MaxPeers: 2},
			targetIP: func(i int) net.IP {
				return net.IPv4(10, 0, 0, byte(i+1))
			},
		},
		{
			name:     "MaxPeersPerIP",
			cfg:      Config{MaxPeers: 8, MaxPeersPerIP: 2},
			targetIP: func(int) net.IP { return net.IPv4(10, 1, 0, 1) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const attempts = 8
			tt.cfg.NodeID = "reservation-host"
			tt.cfg.UserAgent = "aperod/test"
			h := newAdmissionHost(tt.cfg)
			t.Cleanup(h.Stop)

			registerRelease := make(chan struct{})
			atRegister := make(chan struct{}, attempts)
			h.preRegisterHook = func(outbound bool) {
				if outbound {
					atRegister <- struct{}{}
					<-registerRelease
				}
			}

			registered := make(chan struct{}, attempts)
			var dialCalls atomic.Int32
			h.dialContextFunc = func(_ context.Context, _, addr string) (net.Conn, error) {
				dialCalls.Add(1)
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				remote := &net.TCPAddr{IP: net.ParseIP(host)}
				if _, err := fmt.Sscanf(port, "%d", &remote.Port); err != nil {
					return nil, err
				}
				local, peer := net.Pipe()
				go func() {
					defer peer.Close()
					msgType, _, err := readMsg(peer)
					if err != nil || msgType != MsgPing {
						return
					}
					if err := writeMsg(peer, MsgPong, PingMsg{NodeID: addr, UserAgent: "aperod/test"}); err != nil {
						return
					}
					msgType, _, err = readMsg(peer)
					if err == nil && msgType == MsgGetHeaders {
						registered <- struct{}{}
					}
					for err == nil {
						_, _, err = readMsg(peer)
					}
				}()
				return &admissionRemoteConn{Conn: local, remote: remote}, nil
			}

			var wg sync.WaitGroup
			for i := 0; i < attempts; i++ {
				ip := tt.targetIP(i)
				addr := net.JoinHostPort(ip.String(), strconv.Itoa(31000+i))
				wg.Add(1)
				go func() {
					defer wg.Done()
					h.dialPeer(addr)
				}()
			}
			wg.Wait()

			if got := dialCalls.Load(); got != 2 {
				t.Fatalf("dial calls while registrations blocked = %d, want 2", got)
			}
			waitAdmissionSignal(t, atRegister, "first outbound pre-registration")
			waitAdmissionSignal(t, atRegister, "second outbound pre-registration")
			select {
			case <-atRegister:
				t.Fatal("more than two outbound handshakes passed admission")
			default:
			}

			close(registerRelease)
			waitAdmissionSignal(t, registered, "first outbound registration")
			waitAdmissionSignal(t, registered, "second outbound registration")
			if got := h.PeerCount(); got != 2 {
				t.Fatalf("PeerCount = %d, want 2", got)
			}
		})
	}
}

// TestInboundAdmissionCountsOutboundReservation covers the mixed-direction
// handoff: an inbound handshake cannot consume the last slot while an outbound
// handshake owns its reservation but has not yet registered.
func TestInboundAdmissionCountsOutboundReservation(t *testing.T) {
	h := newAdmissionHost(Config{MaxPeers: 1, NodeID: "mixed-host", UserAgent: "aperod/test"})
	t.Cleanup(h.Stop)

	outboundAtRegister := make(chan struct{})
	releaseOutbound := make(chan struct{})
	h.preRegisterHook = func(outbound bool) {
		if outbound {
			close(outboundAtRegister)
			<-releaseOutbound
		}
	}

	outboundRegistered := make(chan struct{})
	h.dialContextFunc = func(context.Context, string, string) (net.Conn, error) {
		local, peer := net.Pipe()
		go func() {
			defer peer.Close()
			if msgType, _, err := readMsg(peer); err != nil || msgType != MsgPing {
				return
			}
			if err := writeMsg(peer, MsgPong, PingMsg{NodeID: "outbound"}); err != nil {
				return
			}
			if msgType, _, err := readMsg(peer); err == nil && msgType == MsgGetHeaders {
				close(outboundRegistered)
			}
			for {
				if _, _, err := readMsg(peer); err != nil {
					return
				}
			}
		}()
		return &admissionRemoteConn{Conn: local, remote: &net.TCPAddr{IP: net.IPv4(10, 2, 0, 1), Port: 32001}}, nil
	}

	h.dialPeer("10.2.0.1:32001")
	waitAdmissionSignal(t, outboundAtRegister, "outbound reservation handoff")

	hostSide, clientSide := net.Pipe()
	hostConn := &admissionRemoteConn{Conn: hostSide, remote: &net.TCPAddr{IP: net.IPv4(10, 3, 0, 1), Port: 33001}}
	go h.handleConn(hostConn, false, 0, 0, "")
	if err := writeMsg(clientSide, MsgPing, PingMsg{NodeID: "inbound"}); err != nil {
		t.Fatalf("write inbound Ping: %v", err)
	}
	if msgType, _, err := readMsg(clientSide); err != nil || msgType != MsgPong {
		t.Fatalf("read inbound Pong: type=%v err=%v", msgType, err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readMsg(clientSide); err == nil {
		t.Fatal("inbound connection remained open despite outbound reservation")
	}
	_ = clientSide.Close()

	close(releaseOutbound)
	waitAdmissionSignal(t, outboundRegistered, "outbound final registration")
	if got := h.PeerCount(); got != 1 {
		t.Fatalf("PeerCount = %d, want 1", got)
	}
}

// TestBanDuringInboundHandshakeCannotRegister pauses an inbound connection
// after its Pong is sent but before final admission, commits a ban, and then
// verifies the final under-gate ban check rejects it.
func TestBanDuringInboundHandshakeCannotRegister(t *testing.T) {
	h := newAdmissionHost(Config{MaxPeers: 4, NodeID: "ban-race-host", UserAgent: "aperod/test"})
	t.Cleanup(h.Stop)

	atRegister := make(chan struct{})
	releaseRegister := make(chan struct{})
	h.preRegisterHook = func(bool) {
		close(atRegister)
		<-releaseRegister
	}

	const remoteAddr = "10.4.0.1:34001"
	hostSide, clientSide := net.Pipe()
	hostConn := &admissionRemoteConn{Conn: hostSide, remote: &net.TCPAddr{IP: net.IPv4(10, 4, 0, 1), Port: 34001}}
	go h.handleConn(hostConn, false, 0, 0, "")

	if err := writeMsg(clientSide, MsgPing, PingMsg{NodeID: "soon-banned"}); err != nil {
		t.Fatalf("write Ping: %v", err)
	}
	if msgType, _, err := readMsg(clientSide); err != nil || msgType != MsgPong {
		t.Fatalf("read Pong: type=%v err=%v", msgType, err)
	}
	waitAdmissionSignal(t, atRegister, "inbound pre-registration")

	h.BanPeer(remoteAddr, "admission race test", time.Hour)
	close(releaseRegister)
	_ = clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readMsg(clientSide); err == nil {
		t.Fatal("banned inbound connection remained open")
	}
	_ = clientSide.Close()
	if got := h.PeerCount(); got != 0 {
		t.Fatalf("PeerCount = %d, want 0 after concurrent ban", got)
	}
}
