package sshconn_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// controllableDialer wraps a real DialFunc with test-driven latency and error
// injection so a dial can be held "in flight" deterministically. It records each
// dial entry (started) and blocks on release before returning, which lets tests
// pin the single-flight + dial-outside-the-mutex contract that the testServer's
// instantaneous dial cannot exercise.
type controllableDialer struct {
	inner sshconn.DialFunc

	// calls counts every dial invocation (incremented at entry).
	calls atomic.Int64

	// started receives once per dial entry (buffered, non-blocking send) so a
	// test can wait until a dial is actually in flight.
	started chan struct{}

	// release, when non-nil, blocks each dial until it is closed (or the dial
	// ctx fires). nil means dial immediately.
	release chan struct{}

	mu       sync.Mutex
	failErr  error    // when set, returned instead of dialing inner
	lastConn ssh.Conn // the most recent successfully dialed client's transport
}

func newControllableDialer(inner sshconn.DialFunc) *controllableDialer {
	return &controllableDialer{
		inner:   inner,
		started: make(chan struct{}, 64),
	}
}

// Calls returns how many times the dial function was invoked.
func (d *controllableDialer) Calls() int { return int(d.calls.Load()) }

// setFail makes subsequent dials return err without contacting the server; nil
// clears the injected failure.
func (d *controllableDialer) setFail(err error) {
	d.mu.Lock()
	d.failErr = err
	d.mu.Unlock()
}

// dialFunc returns the instrumented DialFunc.
func (d *controllableDialer) dialFunc() sshconn.DialFunc {
	return func(ctx context.Context) (*ssh.Client, error) {
		d.calls.Add(1)
		select {
		case d.started <- struct{}{}:
		default:
		}
		if d.release != nil {
			select {
			case <-d.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		d.mu.Lock()
		failErr := d.failErr
		d.mu.Unlock()
		if failErr != nil {
			return nil, failErr
		}
		client, err := d.inner(ctx)
		if err == nil {
			d.mu.Lock()
			d.lastConn = client.Conn
			d.mu.Unlock()
		}
		return client, err
	}
}

// waitClientDone blocks until the most recently dialed client's transport is
// dead (its Conn.Wait returns), giving tests a deterministic signal that the
// client has observed a transport drop - so the next NewSession reliably takes
// the reconnect path without polling.
func (d *controllableDialer) waitClientDone(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	conn := d.lastConn
	d.mu.Unlock()
	require.NotNil(t, conn, "no client has been dialed yet")
	done := make(chan struct{})
	go func() {
		_ = conn.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client transport never observed the drop")
	}
}

// waitStarted blocks until at least one dial has entered, or fails the test.
func (d *controllableDialer) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("no dial was ever started")
	}
}

// 1. Close must NOT block on an in-flight dial. The current impl dials under the
// mutex, so a dial parked on release holds mu and Close blocks until release ->
// this TIMES OUT (RED). After single-flight, the dial runs outside mu and Close
// returns promptly; the parked NewSession then observes the closed connection.
func TestConnection_CloseDoesNotBlockOnInFlightDial(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))
	dialer.release = make(chan struct{})

	conn := sshconn.New(dialer.dialFunc())

	// Release the gate on cleanup too, so a RED run (which t.Fatals before the
	// explicit release) still unwinds the parked dial promptly rather than
	// leaking it under its ctx. Idempotent via Once.
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(dialer.release) }) }
	t.Cleanup(releaseGate)

	// A NewSession parked in the gated dial, ctx-bounded so a regression can't
	// hang the dial indefinitely - mirrors a kubectl exec/attach reconnect that
	// stalls.
	sessErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		sess, err := conn.NewSession(ctx)
		if sess != nil {
			_ = sess.Close()
		}
		sessErr <- err
	}()
	dialer.waitStarted(t)

	// Close must return without waiting for the parked dial.
	closed := make(chan error, 1)
	go func() { closed <- conn.Close() }()
	select {
	case err := <-closed:
		assert.NoError(t, err, "Close during an in-flight dial must return cleanly")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close blocked on an in-flight dial - the dial is holding the mutex")
	}

	// Release the parked dial; the started NewSession must not hang. It either
	// observes the close (ErrClosed) or returns a session that is immediately
	// unusable - either way it must RETURN.
	releaseGate()
	select {
	case <-sessErr:
		// Returned; exact error is impl-detail (ErrClosed or a dead-client error).
	case <-time.After(2 * time.Second):
		t.Fatal("parked NewSession never returned after Close + release")
	}
}

// 2. A short-ctx caller must give up on its own deadline rather than block on the
// mutex behind another caller's in-flight dial. Current impl: caller B blocks on
// mu.Lock() until A's dial releases, well past B's 50ms deadline -> RED.
func TestConnection_ShortCtxCallerGivesUpDuringInFlightDial(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))
	dialer.release = make(chan struct{})

	conn := sshconn.New(dialer.dialFunc())

	// Releasing the gate is idempotent and registered as a cleanup so that even
	// if the test fails early (t.Fatal), the parked dial and any Close unwind
	// promptly instead of waiting on caller A's ctx. Cleanups run LIFO, so this
	// (registered last) runs before the conn.Close cleanup below.
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(dialer.release) }) }
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(releaseGate)

	// Caller A holds an in-flight dial open. Its ctx is bounded so a regression
	// can never hang the dial indefinitely under the lock.
	aErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		sess, err := conn.NewSession(ctx)
		if sess != nil {
			_ = sess.Close()
		}
		aErr <- err
	}()
	dialer.waitStarted(t)

	// Caller B has a short deadline; it must return ~promptly with ctx error, not
	// wait for A's dial.
	ctxB, cancelB := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelB()
	bDone := make(chan error, 1)
	go func() {
		sess, err := conn.NewSession(ctxB)
		if sess != nil {
			_ = sess.Close()
		}
		bDone <- err
	}()

	select {
	case errB := <-bDone:
		require.Error(t, errB, "short-ctx caller B must not succeed while a dial is parked")
		assert.ErrorIs(t, errB, context.DeadlineExceeded,
			"caller B must give up on its own deadline, got %v", errB)
	case <-time.After(2 * time.Second):
		t.Fatal("caller B blocked on the mutex past its deadline - dial is under the lock")
	}

	// Release A; it must complete successfully over the single transport.
	releaseGate()
	select {
	case errA := <-aErr:
		assert.NoError(t, errA, "caller A's session must succeed once released")
	case <-time.After(2 * time.Second):
		t.Fatal("caller A never completed after release")
	}
}

// 3. N concurrent NewSession on a FRESH Connection collapse into exactly ONE dial
// (single-flight). May already be green via the current under-mu serialization -
// this is a characterization lock that also has teeth against a naive lock-free
// refactor that lets each waiter dial.
func TestConnection_ConcurrentNewSession_SingleDial(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))
	dialer.release = make(chan struct{})

	conn := sshconn.New(dialer.dialFunc())
	defer func() { _ = conn.Close() }()

	const n = 8
	var ready sync.WaitGroup
	ready.Add(n)
	errs := make(chan error, n)
	for range n {
		go func() {
			ready.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sess, err := conn.NewSession(ctx)
			if sess != nil {
				_ = sess.Close()
			}
			errs <- err
		}()
	}

	ready.Wait()
	dialer.waitStarted(t) // a dial is in flight; the rest must coalesce onto it
	close(dialer.release)

	for range n {
		require.NoError(t, <-errs, "every concurrent NewSession must succeed")
	}
	assert.Equal(t, 1, dialer.Calls(), "N concurrent first sessions must collapse into a single dial")
	assert.Equal(t, 1, srv.Accepts(), "single-flight must establish exactly one transport")
}

// 4. (concurrent reconnect) After a transport drop, N concurrent NewSession must re-dial
// exactly ONCE, not once per caller. All N must recover over the new transport.
func TestConnection_ConcurrentReconnect_SingleRedial(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))

	conn := sshconn.New(dialer.dialFunc())
	defer func() { _ = conn.Close() }()

	// Establish the first transport.
	out, err := runExec(t, conn, "warmup")
	require.NoError(t, err)
	require.Equal(t, "warmup", out)
	require.Equal(t, 1, srv.Accepts())
	require.Equal(t, 1, dialer.Calls())

	// Kill it, then fire N concurrent sessions that must all reconnect.
	srv.ForceDropActive()
	dialer.waitClientDone(t)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	oks := make(chan bool, n)
	for range n {
		go func() {
			defer wg.Done()
			// Each caller retries briefly so a transient mid-teardown failure
			// doesn't flake; the assertion is on the TOTAL re-dial count.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				o, e := runExec(t, conn, "recovered")
				if e == nil && o == "recovered" {
					oks <- true
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			oks <- false
		}()
	}
	wg.Wait()
	close(oks)

	for ok := range oks {
		require.True(t, ok, "every caller must recover after the drop")
	}
	// Exactly one re-dial total: warmup (1) + a single shared reconnect (1) = 2.
	assert.Equal(t, 2, srv.Accepts(), "concurrent reconnect must re-dial exactly once, not once per caller")
	assert.Equal(t, 2, dialer.Calls(), "the dialer must be invoked exactly twice total")
}

// 5. (dial-error recovery) A dial that errors on the first attempt and succeeds afterwards:
// the first NewSession surfaces the error (no panic/nil-deref); a subsequent
// NewSession dials fresh and succeeds.
func TestConnection_DialError_SurfacesAndRetries(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))
	wantErr := errors.New("dial refused")
	dialer.setFail(wantErr)

	conn := sshconn.New(dialer.dialFunc())
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := conn.NewSession(ctx)
	if sess != nil {
		_ = sess.Close()
	}
	require.Error(t, err, "the first dial error must surface")
	assert.ErrorIs(t, err, wantErr, "the dial error must reach the caller, got %v", err)
	assert.Equal(t, 0, srv.Accepts(), "a failed dial establishes no transport")

	// Recover: a later NewSession dials fresh and works.
	dialer.setFail(nil)
	out, err := runExec(t, conn, "after-dial-error")
	require.NoError(t, err, "a NewSession after a cleared dial failure must succeed")
	assert.Equal(t, "after-dial-error", out)
	assert.Equal(t, 1, srv.Accepts(), "recovery establishes exactly one transport")
}

// 6. (transparent reconnect) A single NewSession after a transport drop must return a working
// session with NO error surfaced - the internal first-failure is swallowed by the
// one-shot reconnect. Deterministic (no Eventually): the drop is observed
// synchronously before the recovery call.
func TestConnection_SingleCallReconnectIsTransparent(t *testing.T) {
	srv := newTestServer(t, 0)
	dialer := newControllableDialer(newDialFunc(t, srv))

	conn := sshconn.New(dialer.dialFunc())
	defer func() { _ = conn.Close() }()

	// Establish, then run one session to completion so the transport is fully up.
	out, err := runExec(t, conn, "first")
	require.NoError(t, err)
	require.Equal(t, "first", out)
	require.Equal(t, 1, srv.Accepts())

	// Drop the transport and synchronously wait for the client to observe it, so
	// the next NewSession deterministically takes the reconnect path.
	srv.ForceDropActive()
	dialer.waitClientDone(t)

	// A SINGLE NewSession call must transparently reconnect and return a working
	// session - no error bubbled up to the caller.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := conn.NewSession(ctx)
	require.NoError(t, err, "the one-shot reconnect must swallow the internal first-failure")
	require.NotNil(t, sess)
	defer func() { _ = sess.Close() }()

	got, err := sess.Output("after-drop")
	require.NoError(t, err, "the reconnected session must be fully working")
	assert.Equal(t, "after-drop", string(got))
	assert.Equal(t, 2, srv.Accepts(), "exactly one re-dial on the single recovery call")
}
