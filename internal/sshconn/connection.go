// Package sshconn provides a per-VM persistent SSH connection that caches one
// *ssh.Client, multiplexes sessions over it, transparently reconnects when the
// transport dies, and owns a keepalive goroutine for the cached client.
package sshconn

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"golang.org/x/crypto/ssh"
)

// ErrClosed is returned by NewSession after the Connection has been closed.
var ErrClosed = errors.New("ssh connection closed")

// keepaliveInterval is how often a keepalive request is sent on the cached
// client's transport.
const keepaliveInterval = 30 * time.Second

// DialFunc establishes a new SSH client. The supplied ctx bounds the dial; the
// caller owns its deadline.
type DialFunc func(ctx context.Context) (*ssh.Client, error)

// dialOp is one in-flight dial that concurrent callers coalesce onto
// (single-flight). The dial runs OUTSIDE the Connection mutex; done is closed
// when it finishes, client/err hold its result, and cancel aborts it (Close uses
// cancel so it never waits on a slow dial).
type dialOp struct {
	done   chan struct{}
	cancel context.CancelFunc
	client *ssh.Client
	err    error
}

// Connection is a per-VM persistent SSH connection. A single *ssh.Client is
// cached and shared across sessions; when its transport dies the next
// NewSession re-dials once and retries. It is safe for concurrent use.
type Connection struct {
	dial DialFunc

	mu       sync.Mutex
	client   *ssh.Client
	kaCancel context.CancelFunc // stops the current client's keepalive
	closed   bool
	pending  *dialOp // in-flight dial, or nil
}

// New returns a Connection that dials with the supplied DialFunc. No goroutine
// or transport is established until the first NewSession.
func New(dial DialFunc) *Connection {
	return &Connection{dial: dial}
}

// NewSession opens a new session channel over the cached client, re-dialing once
// if the transport is dead. ctx bounds both any dial performed here AND each
// session-open channel request; the caller MUST give it a deadline.
//
// Reconnect rule: a channel refusal from a live server (*ssh.OpenChannelError,
// e.g. MaxSessions -> ssh.ResourceShortage) is surfaced as-is (transport healthy).
// Any other NewSession error is a dead transport: reconnect and retry once.
func (c *Connection) NewSession(ctx context.Context) (*ssh.Session, error) {
	// Pre-cancelled ctx: bail before the open goroutine spawns, else the
	// openSession select can race to retire a healthy cached client.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	client, err := c.clientForSession(ctx, nil)
	if err != nil {
		return nil, err
	}

	sess, err := c.openSession(ctx, client)
	if err == nil {
		return sess, nil
	}

	// Live server refused the channel: transport is healthy, do not re-dial.
	if _, ok := errors.AsType[*ssh.OpenChannelError](err); ok {
		return nil, err
	}

	// ctx fired during the channel-open: openSession already retired the client.
	// Do not start a doomed re-dial with a spent ctx; surface the cancel error.
	if ctx.Err() != nil {
		return nil, err
	}

	// Transport is dead: reconnect once and retry exactly once.
	client, err = c.clientForSession(ctx, client)
	if err != nil {
		return nil, err
	}
	return c.openSession(ctx, client)
}

// openSession opens one session channel on client, bounded by ctx. x/crypto/ssh
// has no per-channel-open deadline (OpenChannel waits forever for the reply), so
// the open runs in a goroutine selected against ctx. NOT a net.Conn SetDeadline:
// that breaks the multiplexed keepalive and sibling sessions on the shared transport.
//
// ctx-first: retire the client (a wedged transport is dead for every multiplexed
// session anyway; on a healthy one the window is sub-ms and the next NewSession
// re-dials), then drain the late result and close any session it made. The drain
// cannot leak: retire Close()s the client, unparking the pending OpenChannel.
func (c *Connection) openSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	type openResult struct {
		sess *ssh.Session
		err  error
	}
	resCh := make(chan openResult, 1)
	go func() {
		sess, err := client.NewSession()
		resCh <- openResult{sess: sess, err: err}
	}()

	select {
	case r := <-resCh:
		return r.sess, r.err
	case <-ctx.Done():
		c.retire(client)
		go func() {
			r := <-resCh
			if r.sess != nil {
				_ = r.sess.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// retire closes client, drops it from the cache, and stops its keepalive - only
// if it is still the cached client and the conn is not closed. Guards: never touch
// a successor someone else cached, never fight a concurrent Close. Same semantics
// as the stale-retire branch in clientForSession.
func (c *Connection) retire(client *ssh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.client != client {
		return
	}
	_ = c.client.Close()
	c.stopKeepaliveLocked()
	c.client = nil
}

// clientForSession returns a usable cached client, dialing one (single-flight) if
// none is cached or if stale is the dead client to retire. The dial runs outside
// the mutex; this caller waits on it bounded by its OWN ctx, so a short deadline
// gives up promptly while the shared dial keeps running for other waiters.
func (c *Connection) clientForSession(ctx context.Context, stale *ssh.Client) (*ssh.Client, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.client != nil && c.client != stale {
		// Cached client is usable (or someone already reconnected past stale).
		cl := c.client
		c.mu.Unlock()
		return cl, nil
	}
	if stale != nil && c.client == stale {
		// Retire the dead client exactly once: only the caller still seeing stale clears it.
		_ = c.client.Close()
		c.stopKeepaliveLocked()
		c.client = nil
	}
	op := c.pending
	if op == nil {
		// dialCtx inherits the FIRST caller's ctx (not WithoutCancel) on purpose:
		// the caller's deadline (the probe budget) bounds a stalled dial so a retry
		// gets a fresh dial. Trade-off (M3): the shared dial is bound to the first
		// caller; if its deadline fires, the dial is cancelled for coalesced waiters
		// too. They self-heal by re-dialing on their next NewSession. Also
		// cancellable by Close.
		dialCtx, cancel := context.WithCancel(ctx)
		op = &dialOp{done: make(chan struct{}), cancel: cancel}
		c.pending = op
		c.mu.Unlock()
		go c.runDial(dialCtx, op)
	} else {
		c.mu.Unlock()
	}

	select {
	case <-op.done:
		return op.client, op.err
	case <-ctx.Done():
		// Give up on MY deadline. The shared dial keeps running for other waiters
		// only while the first caller's ctx is live (see M3 above); a coalesced
		// waiter self-heals by re-dialing on its next NewSession.
		return nil, ctx.Err()
	}
}

// runDial performs the dial outside the mutex, then publishes the result. On
// success it caches the client and starts the keepalive, unless the Connection
// was closed meanwhile (then it closes the fresh client and reports ErrClosed).
// Even if every current waiter gave up, a successful dial still caches the client
// + keepalive for the NEXT NewSession (the single-flight win).
func (c *Connection) runDial(dialCtx context.Context, op *dialOp) {
	client, err := c.dial(dialCtx)

	c.mu.Lock()
	defer c.mu.Unlock()
	// op.cancel releases dialCtx (govet lostcancel). INVARIANT: each return below
	// sets op.err or op.client before the deferred close, else a waiter wakes to
	// (nil, nil).
	defer close(op.done)
	defer op.cancel()
	c.pending = nil
	if c.closed {
		if client != nil {
			_ = client.Close()
		}
		op.err = ErrClosed
		return
	}
	if err != nil {
		op.err = err
		return
	}
	c.client = client
	// Keepalive lives for the Connection: keep the caller's ctx values but strip
	// its cancellation so it survives past the dial; only Close/reconnect (via
	// kaCancel) or a dead transport stops it.
	kaCtx, kaCancel := context.WithCancel(context.WithoutCancel(dialCtx))
	c.kaCancel = kaCancel
	go keepAlive(kaCtx, client.Conn)
	op.client = client
}

// Close tears down the cached client and stops the keepalive. Idempotent. It
// aborts any in-flight dial via the op cancel and never waits on it: runDial
// sees the closed flag and discards its result.
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.pending != nil {
		c.pending.cancel()
	}
	c.stopKeepaliveLocked()
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// stopKeepaliveLocked cancels the running keepalive, if any. Must hold mu.
func (c *Connection) stopKeepaliveLocked() {
	if c.kaCancel != nil {
		c.kaCancel()
		c.kaCancel = nil
	}
}

// keepAlive sends a periodic keepalive on conn until ctx is cancelled or a send
// fails (transport dead). ctx is the Connection-lifetime keepalive context, not
// a request ctx.
func keepAlive(ctx context.Context, conn ssh.Conn) {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, _, err := conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				// Debug, not Error: graceful shutdown drops the guest network then
				// Close()s, so a send failure here is usually benign teardown, not
				// an incident. Reconnect-on-next-use is the operator-facing signal.
				log.G(ctx).WithError(err).Debug("ssh keepalive failed; stopping")
				return
			}
		}
	}
}
