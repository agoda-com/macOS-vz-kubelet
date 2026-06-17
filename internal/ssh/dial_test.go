package ssh_test

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/testdata"
)

func TestDialContext(t *testing.T) {
	addr := "127.0.0.1:2222"
	listener := startMockSSHServer(t, addr)
	defer func() {
		assert.NoError(t, listener.Close())
	}()

	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	t.Run("successful connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", addr, config)
		assert.NoError(t, err)
		assert.NotNilf(t, client, "Expected client, got nil")
		require.NoError(t, client.Close())
	})

	t.Run("context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", addr, config)
		assert.Errorf(t, err, "Expected error due to context timeout, got nil")
		assert.Nilf(t, client, "Expected nil client, got %v", client)
		assert.EqualErrorf(t, err, context.DeadlineExceeded.Error(), "Expected context.DeadlineExceeded, got %v", err)
	})

	t.Run("connection error", func(t *testing.T) {
		invalidAddr := "127.0.0.1:9999"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", invalidAddr, config)
		assert.Errorf(t, err, "Expected error due to invalid address, got nil")
		assert.Nilf(t, client, "Expected nil client, got %v", client)
	})
}

// Handshake stalls (server never sends a banner) and ctx expires: DialContext
// must close the raw conn, not leak it - the server-side conn then sees EOF.
func TestDialContextClosesConnOnTimeout(t *testing.T) {
	listenConfig := &net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { assert.NoError(t, listener.Close()) }()

	// Accept one conn, never send the banner, so the client's NewClientConn
	// blocks reading it until ctx expires.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	const timeout = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	client, err := vzssh.DialContext(ctx, "tcp", listener.Addr().String(), config)
	elapsed := time.Since(start)

	assert.Error(t, err, "Expected error due to stalled handshake")
	assert.Nil(t, client, "Expected nil client, got %v", client)
	// No hang: the dial returns close to the timeout, not far beyond it.
	assert.Less(t, elapsed, 2*timeout, "DialContext did not return promptly after ctx expiry")

	// NewClientConn writes the client banner before blocking on the server's, so
	// the first read drains that banner (nil err); the next blocks until close.
	// Read until a non-nil error; a fired deadline = leak, EOF/closed = fixed.
	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(timeout):
		t.Fatal("server never accepted the connection")
	}
	defer func() { _ = serverConn.Close() }()

	require.NoError(t, serverConn.SetReadDeadline(time.Now().Add(2*timeout)))
	buf := make([]byte, 256)
	var readErr error
	for readErr == nil {
		_, readErr = serverConn.Read(buf)
	}
	if errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Fatalf("server conn never closed within %v - DialContext leaked the conn on ctx cancel", 2*timeout)
	}
}

// config.Timeout must bound the SSH handshake, not just the TCP connect: a guest
// stuck in the mDNS .local login stall completes the TCP connect but never sends
// a banner. The caller ctx is given a far longer deadline so only config.Timeout
// can cut a stalled handshake; if it does not, the dial runs until the caller ctx
// expires and the elapsed-time oracle below fires.
func TestDialContextBoundsHandshakeByConfigTimeout(t *testing.T) {
	listenConfig := &net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { assert.NoError(t, listener.Close()) }()

	// Accept one conn, never send the banner, so the client's NewClientConn
	// blocks reading it until a timeout cuts the dial. Hand the conn back to the
	// test goroutine and keep it open until the test ends; closing it early would
	// let the handshake fail on EOF and mask whether config.Timeout did the bound.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	defer func() {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	}()

	const timeout = 150 * time.Millisecond
	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}

	// Caller ctx far outlives config.Timeout, so only config.Timeout can bound a
	// stalled handshake; a return near the caller deadline = config.Timeout ignored.
	ctx, cancel := context.WithTimeout(context.Background(), 30*timeout)
	defer cancel()

	start := time.Now()
	client, err := vzssh.DialContext(ctx, "tcp", listener.Addr().String(), config)
	elapsed := time.Since(start)

	assert.Error(t, err, "Expected error due to handshake bounded by config.Timeout")
	assert.Nil(t, client, "Expected nil client, got %v", client)
	// Deliberately narrow: the bound is config.Timeout, not the caller ctx. 4x
	// absorbs scheduler jitter while staying well under the 30x caller deadline,
	// so a regression that drops the config.Timeout bound trips this.
	assert.Less(t, elapsed, 4*timeout, "DialContext did not bound the handshake by config.Timeout")
}

// The config.Timeout ctx that bounds dial+handshake must NOT outlive into the
// returned client: sshconn caches this client and reuses it for every exec/attach
// on the VM, so closing it when the dial ctx expires would silently break reuse.
// The handshake goroutine only closes the client in its ctx.Done arm, which loses
// the rendezvous to the result send on success (see DialContext's handshake select),
// so a successful dial returns a live client and the late defer cancel is a no-op on it.
// This guards that: a regression closing the client unconditionally on dial-ctx expiry
// would make NewSession fail with "use of closed network connection".
func TestDialContextClientUsableAfterTimeoutElapses(t *testing.T) {
	listener := startMockSSHServer(t, "127.0.0.1:0")
	defer func() { assert.NoError(t, listener.Close()) }()

	const dialTimeout = 100 * time.Millisecond
	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout, // short, so the derived dial ctx expires during the sleep below
	}

	// Long caller ctx so config.Timeout is the bound under test, not the caller deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client, err := vzssh.DialContext(ctx, "tcp", listener.Addr().String(), config)
	require.NoError(t, err)
	require.NotNil(t, client)
	defer func() { _ = client.Close() }()

	// Sleep well past config.Timeout so the dial-bounding ctx has certainly expired.
	time.Sleep(3 * dialTimeout)

	// A client whose transport was closed on dial-ctx expiry would fail here; a live
	// reused client opens the session against the still-open conn.
	sess, err := client.NewSession()
	require.NoError(t, err, "cached client must stay usable after the dial timeout elapses")
	// Best-effort: the mock accepts then immediately closes the channel server-side, so
	// Close may race that; the load-bearing assertion is that NewSession succeeded.
	_ = sess.Close()
}

// Mock SSH server to simulate ssh.ClientConn
func startMockSSHServer(t *testing.T, addr string) net.Listener {
	t.Helper()

	privateBytes := testdata.PEMBytes["rsa"]
	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		t.Fatalf("Failed to parse private key: %v", err)
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(private)

	listenConfig := &net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go handleChannels(t, chans)
			}()
		}
	}()
	return listener
}

// Handle channels for mock SSH server
func handleChannels(t *testing.T, chans <-chan ssh.NewChannel) {
	t.Helper()

	for newChannel := range chans {
		channel, _, err := newChannel.Accept()
		require.NoError(t, err)
		require.NoError(t, channel.Close())
	}
}
