package resourcemanager

import (
	"context"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/agoda-com/macOS-vz-kubelet/internal/execerror"
	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
)

// ExecInVirtualMachine executes a command inside a specified virtual machine.
func (c *MacOSClient) ExecInVirtualMachine(ctx context.Context, namespace, name string, cmd []string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.ExecInVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, err := c.getVirtualMachineInfo(ctx, namespace, name)
	if err != nil {
		return err
	}
	if info.SSHConn == nil {
		return errdefs.InvalidInputf("ssh connection not initialized for %s/%s", namespace, name)
	}

	_, sessionSpan := trace.StartSpan(ctx, "MacOSClient.SSHNewSession")
	session, err := info.SSHConn.NewSession(ctx)
	if err != nil {
		// NewSession err returned UNWRAPPED: pollSSHReady fail-fast type-asserts via Cause(),
		// not errors.Unwrap; a %w wrap degrades a permanent config error to infinite retry
		// (see macos_poststart.go pollSSHReady). SetupSessionIO below DOES %w-wrap - fine, it
		// is not a config error and does not gate the loop.
		sessionSpan.SetStatus(err)
		sessionSpan.End()
		return err
	}
	sessionSpan.SetStatus(nil)
	sessionSpan.End()
	// Redundant Close() safe on the shared client: this defer + AfterFunc reap below + TTY
	// stdin-EOF path (NewMacOSSession) can each close this session, but x/crypto's sentClose
	// latch caps it at one CHANNEL_CLOSE on the wire, so a duplicate can't tear down the
	// per-VM client or sibling sessions. Relies on single-use sessions (never pooled).
	defer func() { _ = session.Close() }()

	// On caller cancellation, signal the remote process (best-effort) THEN close the
	// session: closing the channel alone does not reap a non-PTY remote process, and
	// the shared client stays up for other sessions. macOS sshd delivers the SIGTERM to
	// the session's whole process group. Do NOT drop the signal; the exec/attach
	// orphan-reap depends on it. Fires for the bounded-ctx callers (probe,
	// graceful-shutdown) and for kubectl exec/attach via the pinned vk fork that cancels
	// the request ctx on client disconnect. On normal completion stopReap() deregisters
	// this before the deferred session.Close() above tears down.
	stopReap := context.AfterFunc(ctx, func() {
		if err := session.Signal(ssh.SIGTERM); err != nil {
			log.G(ctx).WithError(err).Debug("ssh signal-on-cancel failed; remote process may be orphaned")
		}
		_ = session.Close()
	})
	defer stopReap()

	// We establish stdinPipe here instead of directly assigning attach.Stdin() to the session
	// because we need to monitor any interruptions to stdin in order to properly close the session.
	// For example, if the interactive terminal is closed without exiting the session, the session
	// would be left hanging.
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer func() {
		_ = stdinPipe.Close()
	}()

	macOSSession := vzssh.NewMacOSSession(session, attach, stdinPipe)
	setupCtx, setupSpan := trace.StartSpan(ctx, "MacOSClient.SSHSetupSessionIO")
	err = macOSSession.SetupSessionIO(setupCtx)
	if err != nil {
		setupSpan.SetStatus(err)
		setupSpan.End()
		return fmt.Errorf("failed to setup session IO: %w", err)
	}
	setupSpan.SetStatus(nil)
	setupSpan.End()

	execCtx, execSpan := trace.StartSpan(ctx, "MacOSClient.SSHExecuteCommand")
	// Normalize the SSH exit error so vk's exec server surfaces a non-zero exit
	// status instead of a generic Internal error. Returned unwrapped (vk type-asserts).
	err = execerror.AsCodeExitError(macOSSession.ExecuteCommand(execCtx, info.Resource.Env(), cmd))
	execSpan.SetStatus(err)
	execSpan.End()
	return err
}
