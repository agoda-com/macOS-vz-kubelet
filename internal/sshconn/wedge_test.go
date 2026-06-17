package sshconn_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionResult carries the outcome of an async NewSession back to the test.
type sessionResult struct {
	sess interface{ Close() error }
	err  error
}

// newSessionAsync runs conn.NewSession(ctx) in a goroutine, returning a size-1
// channel with the result. Buffered so a late sender never leaks if the test gives
// up first. Caller closes the session, if any, via the returned result.
func newSessionAsync(ctx context.Context, c *sshconn.Connection) <-chan sessionResult {
	ch := make(chan sessionResult, 1)
	go func() {
		sess, err := c.NewSession(ctx)
		// *ssh.Session satisfies the minimal Close()-error shape; nil stays nil.
		if sess != nil {
			ch <- sessionResult{sess: sess, err: err}
			return
		}
		ch <- sessionResult{sess: nil, err: err}
	}()
	return ch
}

// awaitSession blocks for the async NewSession result, failing the test if it does
// not return within margin past ctx firing. On pre-fix code the session-open
// ignores ctx and hangs, so RED manifests as a clear failure, not a hung run.
func awaitSession(t *testing.T, res <-chan sessionResult, ctxDeadline time.Duration) sessionResult {
	t.Helper()
	const margin = 3 * time.Second
	select {
	case r := <-res:
		return r
	case <-time.After(ctxDeadline + margin):
		t.Fatalf("NewSession did not return by ctx deadline (%s) + margin (%s); session-open ignored ctx",
			ctxDeadline, margin)
		return sessionResult{} // unreachable
	}
}

// (wedge-a) On a cached client whose channel-open never completes (half-open
// transport), NewSession must honor the context DEADLINE: return at ~deadline with
// errors.Is(err, context.DeadlineExceeded), not block on the never-answered open.
func TestConnection_NewSessionOnWedgedCachedClient_HonorsContextDeadline(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	// Warm the cache: one successful session establishes and caches the client.
	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())

	// Wedge the transport: handshake stays up, but the next channel-open hangs.
	srv.SetWedgeChannelOpens(true)

	const deadline = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	r := awaitSession(t, newSessionAsync(ctx, conn), deadline)
	elapsed := time.Since(start)
	if r.sess != nil {
		_ = r.sess.Close()
	}

	require.Error(t, r.err, "NewSession on a wedged cached client must not succeed")
	assert.ErrorIs(t, r.err, context.DeadlineExceeded,
		"NewSession must surface the ctx deadline, got %v", r.err)
	// Returned at ~deadline, not after a long hang. Generous upper bound so the
	// assertion proves "bounded by ctx" without being flaky on a slow runner.
	assert.Less(t, elapsed, deadline+2*time.Second,
		"NewSession must return at ~ctx deadline, took %s", elapsed)
}

// (wedge-b) On a cached client whose channel-open never completes, NewSession
// must honor an explicit CANCEL issued while it is in flight: it returns with an
// error satisfying errors.Is(err, context.Canceled). The cancel fires only after
// the server has parked the channel-open (deterministic, no sleep).
func TestConnection_NewSessionOnWedgedCachedClient_HonorsContextCancel(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())

	srv.SetWedgeChannelOpens(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := newSessionAsync(ctx, conn)
	// Wait until the channel-open is genuinely parked server-side, THEN cancel.
	srv.WaitWedgedOpen(t)
	cancel()

	// ctx cancel has no deadline of its own; pass 0 so awaitSession's margin is
	// the only bound - a cancel-honoring NewSession returns well within it.
	r := awaitSession(t, res, 0)
	if r.sess != nil {
		_ = r.sess.Close()
	}

	require.Error(t, r.err, "NewSession on a wedged cached client must not succeed after cancel")
	assert.ErrorIs(t, r.err, context.Canceled,
		"NewSession must surface the ctx cancel, got %v", r.err)
}

// (wedge-c) After a ctx-expired NewSession on a wedged cached client, the dead
// client must be RETIRED (closed + uncached). Once the wedge is lifted, the next
// NewSession with a fresh ctx must re-dial (srv.Accepts increments to 2) and work
// end-to-end - proving the wedged client was not left cached to wedge again.
func TestConnection_NewSessionOnWedgedCachedClient_RetiresClientAndRedials(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())

	// Wedge, then drive one NewSession into the ctx-expiry path.
	srv.SetWedgeChannelOpens(true)
	const deadline = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	r := awaitSession(t, newSessionAsync(ctx, conn), deadline)
	if r.sess != nil {
		_ = r.sess.Close()
	}
	require.ErrorIs(t, r.err, context.DeadlineExceeded,
		"precondition: the wedged NewSession must give up on its ctx deadline, got %v", r.err)

	// Lift the wedge. The retired client must be gone, so the next NewSession
	// re-dials (Accepts -> 2) and runs end-to-end over the fresh transport.
	srv.SetWedgeChannelOpens(false)

	var out2 string
	require.Eventually(t, func() bool {
		o, e := runExec(t, conn, "after-wedge")
		if e != nil {
			return false
		}
		out2 = o
		return true
	}, 5*time.Second, 20*time.Millisecond,
		"after a ctx-expired session-open the client must be retired so the next NewSession re-dials")

	assert.Equal(t, "after-wedge", out2, "the re-dialed session must be fully working")
	assert.Equal(t, 2, srv.Accepts(),
		"a ctx-expired wedged session must retire the client; the next NewSession re-dials exactly once")
}

// (wedge-d) After a wedged ctx-expired NewSession and then Close(), no goroutine
// leaks: the wedged channel-open goroutine and any helper started for the cached
// client must exit once the client is closed.
func TestConnection_NewSessionOnWedgedCachedClient_NoGoroutineLeakAfterClose(t *testing.T) {
	srv := newTestServer(t, 0)

	// Baseline AFTER the server accept loop is running so only the Connection's
	// own goroutines are expected to disappear post-Close.
	baseline := runtime.NumGoroutine()
	conn := sshconn.New(newDialFunc(t, srv))

	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())

	srv.SetWedgeChannelOpens(true)
	const deadline = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	r := awaitSession(t, newSessionAsync(ctx, conn), deadline)
	if r.sess != nil {
		_ = r.sess.Close()
	}
	require.ErrorIs(t, r.err, context.DeadlineExceeded,
		"precondition: the wedged NewSession must give up on its ctx deadline, got %v", r.err)

	require.NoError(t, conn.Close(), "Close after a wedged session must return cleanly")

	// The keepalive and any session-open helper goroutine for the wedged client
	// must be gone after Close.
	assertNoGoroutineLeak(t, baseline)
}

// (wedge-e) An ALREADY-cancelled ctx must short-circuit at the top of NewSession:
// it returns context.Canceled immediately without opening a session, AND it must
// NOT retire the healthy cached client (no race into the openSession retire path).
// A subsequent NewSession on a live ctx reuses the SAME transport - no re-dial.
func TestConnection_NewSessionWithPreCancelledCtx_ReturnsCanceledAndKeepsCachedClient(t *testing.T) {
	srv := newTestServer(t, 0)
	conn := sshconn.New(newDialFunc(t, srv))
	defer func() { _ = conn.Close() }()

	// Warm the cache with a healthy client. No wedge here - the transport is fine.
	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())

	// Pre-cancelled ctx: NewSession must give up before touching the client.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	r := awaitSession(t, newSessionAsync(ctx, conn), 0)
	elapsed := time.Since(start)
	if r.sess != nil {
		_ = r.sess.Close()
	}

	require.Error(t, r.err, "NewSession with a pre-cancelled ctx must not succeed")
	assert.ErrorIs(t, r.err, context.Canceled,
		"a pre-cancelled ctx must surface context.Canceled, got %v", r.err)
	assert.Less(t, elapsed, time.Second,
		"a pre-cancelled ctx must short-circuit immediately, took %s", elapsed)

	// The healthy cached client must NOT have been retired: the next session over a
	// live ctx reuses the SAME transport (Accepts stays 1, no re-dial).
	out2, err := runExec(t, conn, "after-precancel")
	require.NoError(t, err, "a live-ctx session after a pre-cancelled one must succeed over the cached client")
	assert.Equal(t, "after-precancel", out2)
	assert.Equal(t, 1, srv.Accepts(),
		"a pre-cancelled ctx must not retire the healthy cached client; no re-dial")
}
