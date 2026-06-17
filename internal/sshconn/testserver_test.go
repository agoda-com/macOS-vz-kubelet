package sshconn_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is an in-process SSH server (x/crypto/ssh server side) used to
// exercise Connection without real infrastructure. It tracks the number of
// successful handshakes (= dials), can drop the live transport mid-flight, and
// enforces a per-connection MaxSessions limit so the channel-refusal path can
// be exercised deterministically.
type testServer struct {
	t        *testing.T
	listener net.Listener
	config   *ssh.ServerConfig

	// accepts counts successful handshakes (one per established transport).
	accepts atomic.Int64

	// maxSessions caps concurrent "session" channels per transport; 0 means
	// unlimited. Exceeding it rejects the channel with ssh.ResourceShortage.
	maxSessions int

	// wedge, when true, models a half-open transport: handshake up (keepalive
	// still answered), but every incoming "session" channel-open is held with no
	// reply, so client.NewSession waits forever. Runtime-toggleable: dial+exec
	// normally, flip on to hang NewSession, let ctx fire, flip off to prove re-dial.
	wedge atomic.Bool

	// wedgedOpens receives once per channel-open held by wedge mode (buffered,
	// non-blocking send) so a test waits until an open is genuinely parked
	// server-side before cancelling - deterministic, no sleeps.
	wedgedOpens chan struct{}

	mu         sync.Mutex
	activeConn net.Conn // the most recently handshaked server-side conn

	// errs collects failures observed in server goroutines. Server goroutines
	// must NOT call t.Fatal/require (that runs runtime.Goexit on the wrong
	// goroutine); failures are surfaced here and drained by the test.
	errs chan error

	closeOnce sync.Once
	done      chan struct{}
}

// newTestServer starts a server on 127.0.0.1:0 with an in-test-generated ed25519
// host key and NoClientAuth. maxSessions of 0 means unlimited concurrent sessions.
func newTestServer(t *testing.T, maxSessions int) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer from key: %v", err)
	}

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	lc := &net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &testServer{
		t:           t,
		listener:    ln,
		config:      cfg,
		maxSessions: maxSessions,
		errs:        make(chan error, 64),
		done:        make(chan struct{}),
		wedgedOpens: make(chan struct{}, 64),
	}

	go s.acceptLoop()
	t.Cleanup(func() {
		_ = s.Close()
		// Surface any server-side error after the test body finished.
		select {
		case err := <-s.errs:
			if err != nil {
				t.Errorf("server goroutine error: %v", err)
			}
		default:
		}
	})
	return s
}

// Addr returns the server's listen address.
func (s *testServer) Addr() string { return s.listener.Addr().String() }

// Accepts returns the number of successful handshakes (established transports).
func (s *testServer) Accepts() int { return int(s.accepts.Load()) }

// SetWedgeChannelOpens toggles wedge mode (see the wedge field): on -> hold
// channel-opens with no reply so NewSession blocks on ctx; off -> service normally.
func (s *testServer) SetWedgeChannelOpens(on bool) { s.wedge.Store(on) }

// WaitWedgedOpen blocks until a "session" channel-open is parked by wedge mode, or
// fails the test. Lets a test cancel only once NewSession is genuinely in flight
// server-side, no sleep.
func (s *testServer) WaitWedgedOpen(t *testing.T) {
	t.Helper()
	select {
	case <-s.wedgedOpens:
	case <-time.After(2 * time.Second):
		t.Fatal("no wedged channel-open was ever observed")
	}
}

// ForceDropActive abruptly closes the most recently handshaked server-side conn
// to simulate transport death (RST), without a clean SSH disconnect.
func (s *testServer) ForceDropActive() {
	s.mu.Lock()
	conn := s.activeConn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Close stops the listener. Idempotent.
func (s *testServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.listener.Close()
	})
	return err
}

func (s *testServer) recordErr(err error) {
	if err == nil {
		return
	}
	select {
	case s.errs <- err:
	default:
	}
}

func (s *testServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Listener closed: normal shutdown.
			return
		}
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(conn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		// Handshake failed (e.g. transport dropped before completion); not an
		// established connection, so do not count it.
		_ = conn.Close()
		return
	}
	s.accepts.Add(1)

	s.mu.Lock()
	s.activeConn = conn
	s.mu.Unlock()

	go ssh.DiscardRequests(reqs)

	// wedged keeps held channel-opens referenced (no GC); the range loop keeps
	// iterating so the transport keepalive stays healthy.
	var wedged []ssh.NewChannel

	var sessions atomic.Int64
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		// Half-open transport: hold the channel-open with no reply.
		if s.wedge.Load() {
			wedged = append(wedged, newChan)
			select {
			case s.wedgedOpens <- struct{}{}:
			default:
			}
			continue
		}
		// Enforce MaxSessions per transport. The client surfaces this as an
		// *ssh.OpenChannelError with Reason ssh.ResourceShortage.
		if s.maxSessions > 0 && sessions.Load() >= int64(s.maxSessions) {
			_ = newChan.Reject(ssh.ResourceShortage, "max sessions")
			continue
		}
		sessions.Add(1)
		go func(nc ssh.NewChannel) {
			defer sessions.Add(-1)
			s.handleSession(nc)
		}(newChan)
	}
	// Teardown: reject any still-held opens so the client side of a never-answered
	// open does not block on Close.
	for _, nc := range wedged {
		_ = nc.Reject(ssh.ConnectionFailed, "server shutting down")
	}
	_ = sshConn.Close()
}

// execPayload mirrors the wire format of an "exec" request body.
type execPayload struct {
	Command string
}

// exitStatusPayload mirrors the wire format of an "exit-status" request body.
type exitStatusPayload struct {
	Status uint32
}

// handleSession accepts a session channel and services its requests. An "exec"
// request echoes the command back deterministically (so a test can prove
// channels are not crossed), sends exit-status 0, and closes the channel. A
// "shell" request is held OPEN - it replies true but does NOT send exit-status
// or return, so the channel (and thus the per-transport MaxSessions slot) stays
// occupied until the CLIENT closes it; when the client closes, reqs drains, the
// loop ends, and the deferred ch.Close releases the slot. This lets a test hold
// a session to exercise the MaxSessions channel-refusal path.
func (s *testServer) handleSession(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		s.recordErr(err)
		return
	}
	defer func() { _ = ch.Close() }()

	for req := range reqs {
		switch req.Type {
		case "exec":
			var p execPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				s.recordErr(err)
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				return
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			// Deterministic response: echo the command back so the caller can
			// prove the channel carried its own command's output.
			if _, err := io.WriteString(ch, p.Command); err != nil {
				s.recordErr(err)
			}
			s.sendExit(ch, 0)
			return
		case "shell":
			// Accept the shell but keep the channel open: no write, no
			// exit-status, no return. The slot stays held until the client
			// closes the channel and the outer range drains.
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (s *testServer) sendExit(ch ssh.Channel, status uint32) {
	payload := ssh.Marshal(exitStatusPayload{Status: status})
	if _, err := ch.SendRequest("exit-status", false, payload); err != nil {
		// A closed channel during teardown is benign.
		if !errors.Is(err, io.EOF) {
			s.recordErr(err)
		}
	}
}
