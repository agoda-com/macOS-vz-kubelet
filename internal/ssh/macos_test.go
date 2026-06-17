package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"
	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
)

// execModeServer is an in-process SSH server (x/crypto/ssh server side) that exercises
// MacOSSession.ExecuteCommand without real infra, recording how the client drove the
// session so the two execution shapes can be told apart:
//
//   - MODE-A (single-exec): one "exec" request carrying the built command string. The
//     server records it and replies with a configurable exit status. Proves Mode-A routing
//     and that the built string (env exports + sh -c $'...') went out verbatim.
//   - MODE-B (stdin login-shell fallback): one "shell" request, then env-export and command
//     lines written to channel stdin and closed (EOF). The server records every line in
//     order and emulates a login shell's "exit == status of the LAST command": trailing
//     line == `false` -> 1, else 0, so the masking contract is testable.
//
// Server goroutines must not call t.Fatal/require (Goexit on the wrong goroutine); errs are
// drained in Cleanup, mirroring the sshconn testserver pattern.
type execModeServer struct {
	t        *testing.T
	listener net.Listener
	config   *ssh.ServerConfig

	// execExitStatus is the exit status returned for a Mode-A "exec" request.
	execExitStatus uint32

	mu sync.Mutex
	// reqType is the channel request the client issued ("exec" or "shell").
	reqType string
	// execCommand is the command string carried by a Mode-A "exec" request.
	execCommand string
	// shellLines are the lines written to channel stdin during a Mode-B
	// "shell" session, in the order received.
	shellLines []string

	errs chan error

	closeOnce sync.Once
	done      chan struct{}
}

// newExecModeServer starts a server on 127.0.0.1:0 with an in-test-generated
// ed25519 host key and NoClientAuth. execExitStatus is the status returned for a
// Mode-A "exec" request (Mode-B derives its status from the last line).
func newExecModeServer(t *testing.T, execExitStatus uint32) *execModeServer {
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

	s := &execModeServer{
		t:              t,
		listener:       ln,
		config:         cfg,
		execExitStatus: execExitStatus,
		errs:           make(chan error, 64),
		done:           make(chan struct{}),
	}

	go s.acceptLoop()
	t.Cleanup(func() {
		_ = s.Close()
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
func (s *execModeServer) Addr() string { return s.listener.Addr().String() }

// ReqType returns the channel request type the client issued ("exec"/"shell").
func (s *execModeServer) ReqType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqType
}

// ExecCommand returns the command string carried by a Mode-A "exec" request.
func (s *execModeServer) ExecCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execCommand
}

// ShellLines returns the ordered lines received over a Mode-B "shell" session.
func (s *execModeServer) ShellLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.shellLines))
	copy(out, s.shellLines)
	return out
}

// Close stops the listener. Idempotent.
func (s *execModeServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.listener.Close()
	})
	return err
}

func (s *execModeServer) recordErr(err error) {
	if err == nil {
		return
	}
	select {
	case s.errs <- err:
	default:
	}
}

func (s *execModeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Listener closed: normal shutdown.
			return
		}
		go s.handleConn(conn)
	}
}

func (s *execModeServer) handleConn(conn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		go s.handleSession(newChan)
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

// handleSession services one session channel. For "exec" it records the command and
// replies execExitStatus. For "shell" it reads stdin until EOF (the client signals it by
// closing its stdin pipe -> SSH CloseWrite), records each line, and returns the emulated
// login-shell status from the last line. See drainShellStdin for the exit oracle.
func (s *execModeServer) handleSession(nc ssh.NewChannel) {
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
			s.mu.Lock()
			s.reqType = "exec"
			s.execCommand = p.Command
			s.mu.Unlock()
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			s.sendExit(ch, s.execExitStatus)
			return
		case "shell":
			s.mu.Lock()
			s.reqType = "shell"
			s.mu.Unlock()
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			// Read all stdin lines until the client closes the pipe (EOF),
			// then derive the login-shell exit status from the last line.
			status := s.drainShellStdin(ch)
			s.sendExit(ch, status)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// drainShellStdin reads stdin to EOF, records each line in order, and returns the emulated
// login-shell exit status.
//
// The exit oracle is deliberately narrow: it models the one load-bearing contract, "exit ==
// status of the LAST command", with a single fixed failure token (trimmed last line ==
// "false" -> 1, else 0). It is not a shell interpreter. Future Mode-B cases must stay within
// this model: e.g. do NOT expect `exit 5` -> 5, which this oracle silently scores 0.
func (s *execModeServer) drainShellStdin(ch ssh.Channel) uint32 {
	data, err := io.ReadAll(ch)
	if err != nil {
		s.recordErr(err)
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		// Each entry is newline-terminated, so the final Split element is empty; drop
		// empties to keep the recorded lines faithful to what was sent.
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	s.mu.Lock()
	s.shellLines = lines
	s.mu.Unlock()

	status := uint32(0)
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "false" {
		status = 1
	}
	return status
}

func (s *execModeServer) sendExit(ch ssh.Channel, status uint32) {
	payload := ssh.Marshal(exitStatusPayload{Status: status})
	if _, err := ch.SendRequest("exit-status", false, payload); err != nil {
		if !errors.Is(err, io.EOF) {
			s.recordErr(err)
		}
	}
}

// newMacOSSession dials the in-process server and wraps a raw *ssh.Session in a
// MacOSSession as production does. attach drives the TTY / stdin-copy decisions inside
// ExecuteCommand; pass a non-TTY, nil-stdin AttachIO for the deterministic "no stdin copy,
// close pipe, Wait" path.
func newMacOSSession(t *testing.T, srv *execModeServer, attach api.AttachIO) *vzssh.MacOSSession {
	t.Helper()

	cfg := &ssh.ClientConfig{
		User:            "tester",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", srv.Addr())
	require.NoError(t, err)

	c, chans, reqs, err := ssh.NewClientConn(conn, srv.Addr(), cfg)
	if err != nil {
		_ = conn.Close()
		require.NoError(t, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })

	session, err := client.NewSession()
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	stdinPipe, err := session.StdinPipe()
	require.NoError(t, err)

	return vzssh.NewMacOSSession(session, attach, stdinPipe)
}

func execCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// (1) Mode-A routing + success: a shell-exec argv routes through one "exec" request
// carrying the verbatim built string, and a zero exit yields nil.
func TestExecuteCommand_ModeA_RoutesExecAndSucceeds(t *testing.T) {
	srv := newExecModeServer(t, 0)
	sess := newMacOSSession(t, srv, node.DiscardingExecIO())

	cmd := []string{"sh", "-c", "echo hi"}
	err := sess.ExecuteCommand(execCtx(t), nil, cmd)
	require.NoError(t, err)

	want, buildErr := utils.BuildExecCommandString(cmd, nil)
	require.NoError(t, buildErr, "precondition: this argv must build a Mode-A string")
	assert.Equal(t, "exec", srv.ReqType(), "a shell-exec argv must route through a single exec request")
	assert.Equal(t, want, srv.ExecCommand(),
		"the exec request must carry the verbatim BuildExecCommandString output")
}

// (2) Mode-A with env: the built exec string carries the env-export prefix ahead of the
// sh -c body.
func TestExecuteCommand_ModeA_PrependsEnvExports(t *testing.T) {
	srv := newExecModeServer(t, 0)
	sess := newMacOSSession(t, srv, node.DiscardingExecIO())

	env := []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	cmd := []string{"sh", "-c", "echo hi"}
	err := sess.ExecuteCommand(execCtx(t), env, cmd)
	require.NoError(t, err)

	assert.Equal(t, "exec", srv.ReqType())
	got := srv.ExecCommand()
	assert.True(t, strings.HasPrefix(got, "export FOO=\"bar\"\n"),
		"exec command must begin with the env-export prefix, got %q", got)
	assert.Contains(t, got, "sh -c $'echo hi'",
		"exec command must still carry the sh -c body after the exports, got %q", got)
}

// (3) Mode-A non-zero exit surfaces as the raw *ssh.ExitError (CodeExitError wrapping
// happens in the caller, not here).
func TestExecuteCommand_ModeA_NonZeroExitSurfacesExitError(t *testing.T) {
	srv := newExecModeServer(t, 3)
	sess := newMacOSSession(t, srv, node.DiscardingExecIO())

	// Exit status is server-driven: the server returns execExitStatus (3) for any exec
	// request and never runs the body. The "exit 3" body is illustrative; real `exit N`
	// semantics are an e2e concern, not this fake's.
	cmd := []string{"sh", "-c", "exit 3"}
	err := sess.ExecuteCommand(execCtx(t), nil, cmd)
	require.Error(t, err, "a non-zero exit status must surface as an error")

	var exitErr *ssh.ExitError
	require.True(t, errors.As(err, &exitErr),
		"ExecuteCommand must surface the raw *ssh.ExitError, got %T: %v", err, err)
	assert.Equal(t, 3, exitErr.ExitStatus(), "the surfaced error must carry the server exit status")
}

// (4) Mode-B routing: a non shell-exec argv (the real gitlab-runner prod hook shape) falls
// back to one "shell" request with the lines in order; a non-`false` last line yields nil.
func TestExecuteCommand_ModeB_RoutesShellAndDeliversLinesInOrder(t *testing.T) {
	srv := newExecModeServer(t, 0)
	sess := newMacOSSession(t, srv, node.DiscardingExecIO())

	cmd := []string{"mkdir -p /tmp/x", "networksetup -setproxyautodiscovery Ethernet on"}
	err := sess.ExecuteCommand(execCtx(t), nil, cmd)
	require.NoError(t, err)

	assert.Equal(t, "shell", srv.ReqType(),
		"a non shell-exec argv must fall back to a shell session, not exec")
	assert.Equal(t, cmd, srv.ShellLines(),
		"both command lines must arrive over stdin in order")
}

// (5) Mode-B masking (the load-bearing contract) plus negative control. A login shell
// reading stdin returns the LAST line's status; earlier failures are masked. So
// ["false","true"] succeeds and ["true","false"] fails.
func TestExecuteCommand_ModeB_OnlyLastLineGates(t *testing.T) {
	t.Run("earlier failure is masked by a passing last line", func(t *testing.T) {
		srv := newExecModeServer(t, 0)
		sess := newMacOSSession(t, srv, node.DiscardingExecIO())

		cmd := []string{"false", "true"}
		err := sess.ExecuteCommand(execCtx(t), nil, cmd)
		require.NoError(t, err,
			"only the last line gates: a failing earlier line must be masked")

		assert.Equal(t, "shell", srv.ReqType())
		assert.Equal(t, cmd, srv.ShellLines())
	})

	t.Run("a failing last line gates", func(t *testing.T) {
		srv := newExecModeServer(t, 0)
		sess := newMacOSSession(t, srv, node.DiscardingExecIO())

		cmd := []string{"true", "false"}
		err := sess.ExecuteCommand(execCtx(t), nil, cmd)
		require.Error(t, err, "a failing last line must gate and surface an error")

		var exitErr *ssh.ExitError
		require.True(t, errors.As(err, &exitErr),
			"the last-line failure must surface as *ssh.ExitError, got %T: %v", err, err)
		assert.Equal(t, 1, exitErr.ExitStatus())
		assert.Equal(t, "shell", srv.ReqType())
		assert.Equal(t, cmd, srv.ShellLines())
	})
}

// (6) Mode-B env export: env vars are written as `export NAME="VALUE"` lines ahead of the
// command lines.
func TestExecuteCommand_ModeB_WritesEnvExportsBeforeCommands(t *testing.T) {
	srv := newExecModeServer(t, 0)
	sess := newMacOSSession(t, srv, node.DiscardingExecIO())

	env := []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	cmd := []string{"mkdir -p /tmp/x", "true"}
	err := sess.ExecuteCommand(execCtx(t), env, cmd)
	require.NoError(t, err)

	assert.Equal(t, "shell", srv.ReqType())
	want := []string{"export FOO=\"bar\"", "mkdir -p /tmp/x", "true"}
	assert.Equal(t, want, srv.ShellLines(),
		"env-export lines must precede the command lines, in order")
}
