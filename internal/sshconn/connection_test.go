package sshconn_test

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// newDialFunc returns a sshconn.DialFunc that dials the in-process test server.
// It is context-aware (net.DialContext + ssh.NewClientConn) so a caller's
// deadline bounds both the TCP dial and the handshake.
func newDialFunc(t *testing.T, srv *testServer) sshconn.DialFunc {
	t.Helper()
	cfg := &ssh.ClientConfig{
		User:            "tester",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	return func(ctx context.Context) (*ssh.Client, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", srv.Addr())
		if err != nil {
			return nil, err
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, srv.Addr(), cfg)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return ssh.NewClient(c, chans, reqs), nil
	}
}

// runExec opens a session on conn, runs cmd, and returns its combined output.
// The session is always closed before returning.
func runExec(t *testing.T, c *sshconn.Connection, cmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := c.NewSession(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()

	out, err := sess.Output(cmd)
	return string(out), err
}

// (a) Concurrent sessions over one Connection do not cross channels, and share a
// single transport (one dial for both).
func TestConnection_ConcurrentSessions_AreIsolatedAndShareOneTransport(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	const (
		cmdA = "command-alpha"
		cmdB = "command-bravo"
	)

	type result struct {
		out string
		err error
	}
	resA := make(chan result, 1)
	resB := make(chan result, 1)

	var start sync.WaitGroup
	start.Add(1)
	go func() {
		start.Wait()
		out, err := runExec(t, conn, cmdA)
		resA <- result{out, err}
	}()
	go func() {
		start.Wait()
		out, err := runExec(t, conn, cmdB)
		resB <- result{out, err}
	}()
	start.Done()

	gotA := <-resA
	gotB := <-resB

	require.NoError(t, gotA.err)
	require.NoError(t, gotB.err)
	// Each session's output is its OWN command echoed back - channels not crossed.
	assert.Equal(t, cmdA, gotA.out, "session A received its own command's output")
	assert.Equal(t, cmdB, gotB.out, "session B received its own command's output")
	assert.Equal(t, 1, srv.Accepts(), "both sessions must multiplex over a single transport")
}

// (b) Closing a session closes only the channel, not the shared client: a
// subsequent NewSession reuses the same transport.
func TestConnection_SessionClose_DoesNotCloseSharedClient(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	out1, err := runExec(t, conn, "first")
	require.NoError(t, err)
	assert.Equal(t, "first", out1)
	require.Equal(t, 1, srv.Accepts())

	// runExec already closed session #1. A new session must still succeed.
	out2, err := runExec(t, conn, "second")
	require.NoError(t, err)
	assert.Equal(t, "second", out2)
	assert.Equal(t, 1, srv.Accepts(), "session close must not tear down the shared client")
}

// (c) When the transport dies, the next NewSession transparently re-dials once
// and succeeds; the internal first failure is not surfaced to the caller.
func TestConnection_ReconnectsOnTransportDrop(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	out1, err := runExec(t, conn, "before-drop")
	require.NoError(t, err)
	assert.Equal(t, "before-drop", out1)
	require.Equal(t, 1, srv.Accepts())

	// Kill the live server-side transport, then prove the next session re-dials.
	srv.ForceDropActive()

	// Poll briefly so the client notices the dead transport; each NewSession
	// attempt either re-dials and succeeds or transiently errors as the old
	// client tears down.
	var out2 string
	require.Eventually(t, func() bool {
		o, e := runExec(t, conn, "after-drop")
		if e != nil {
			return false
		}
		out2 = o
		return true
	}, 5*time.Second, 20*time.Millisecond, "NewSession must recover after a transport drop")

	assert.Equal(t, "after-drop", out2, "the recovered session must be fully working")
	assert.Equal(t, 2, srv.Accepts(), "exactly one re-dial after the drop")
}

// (d) A live server refusing a channel for MaxSessions is surfaced as an
// *ssh.OpenChannelError with Reason ResourceShortage and must NOT trigger a
// fallback re-dial - the transport is healthy, the server is just at capacity.
func TestConnection_MaxSessions_SurfacesOpenChannelErrorWithoutRedial(t *testing.T) {
	srv := newTestServer(t, 1)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Session A stays open, holding the single allowed slot. Request a "shell"
	// (no exec) so the server keeps the channel alive without auto-closing.
	sessA, err := conn.NewSession(ctx)
	require.NoError(t, err)
	require.NoError(t, sessA.Shell())
	require.Equal(t, 1, srv.Accepts())

	// Session B is refused at the channel layer.
	sessB, err := conn.NewSession(ctx)
	if sessB != nil {
		_ = sessB.Close()
	}
	require.Error(t, err, "NewSession must fail when the server is at MaxSessions")

	var oce *ssh.OpenChannelError
	require.True(t, errors.As(err, &oce),
		"a live server's channel refusal must surface as *ssh.OpenChannelError, got %T: %v", err, err)
	assert.Equal(t, ssh.ResourceShortage, oce.Reason,
		"MaxSessions exhaustion must report ResourceShortage")
	// THE critical assertion: a channel refusal is NOT a dead transport, so the
	// Connection must not discard a healthy client and re-dial.
	assert.Equal(t, 1, srv.Accepts(), "channel refusal must not trigger a fallback re-dial")

	// Free the slot; a new session succeeds over the SAME transport.
	require.NoError(t, sessA.Close())
	require.Eventually(t, func() bool {
		out, e := runExec(t, conn, "after-free")
		return e == nil && out == "after-free"
	}, 5*time.Second, 20*time.Millisecond, "a session must succeed once the slot is freed")
	assert.Equal(t, 1, srv.Accepts(), "freeing a slot must not have caused a re-dial")
}

// (e) Close is idempotent, releases the keepalive goroutine, and a post-close
// NewSession fails.
func TestConnection_Close_IsIdempotentAndStopsKeepalive(t *testing.T) {
	srv := newTestServer(t, 0)

	// Baseline captured AFTER the server's accept loop is running so the only
	// goroutine expected to disappear after Close is the Connection's keepalive.
	baseline := runtime.NumGoroutine()
	conn := sshconn.New(newDialFunc(t, srv))

	out, err := runExec(t, conn, "live")
	require.NoError(t, err)
	assert.Equal(t, "live", out)
	require.Equal(t, 1, srv.Accepts())

	assert.NoError(t, conn.Close(), "first Close must return nil")
	assert.NoError(t, conn.Close(), "second Close must be a nil no-op (idempotent)")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sess, err := conn.NewSession(ctx)
	if sess != nil {
		_ = sess.Close()
	}
	require.Error(t, err, "NewSession after Close must return a non-nil error")
	assert.ErrorIs(t, err, sshconn.ErrClosed,
		"NewSession after Close must report ErrClosed, got %v", err)

	// The keepalive goroutine started on first dial must be gone after Close.
	assertNoGoroutineLeak(t, baseline)
}

// (e, sub-case) New() then Close() without ever dialing returns nil and never
// established a transport.
func TestConnection_CloseWithoutDial_IsNilAndNeverDials(t *testing.T) {
	srv := newTestServer(t, 0)

	baseline := runtime.NumGoroutine()
	conn := sshconn.New(newDialFunc(t, srv))

	assert.NoError(t, conn.Close(), "Close without a prior dial must return nil")
	assert.Equal(t, 0, srv.Accepts(), "Close without NewSession must never have dialed")

	// New must not start a goroutine before the first successful dial.
	assertNoGoroutineLeak(t, baseline)
}

// assertNoGoroutineLeak polls for the goroutine count to return to (at most) the
// supplied baseline. goleak is not a dependency, so this bounded poll stands in
// for it; a stray keepalive goroutine fails the test.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Give finished goroutines a chance to be scheduled out before sampling.
		runtime.Gosched()
		got := runtime.NumGoroutine()
		if got <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: have %d goroutines, baseline was %d", got, baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
