package ssh

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

type MacOSSession struct {
	attach    api.AttachIO
	stdinPipe io.WriteCloser

	*ssh.Session
}

func NewMacOSSession(session *ssh.Session, attach api.AttachIO, stdinPipe io.WriteCloser) *MacOSSession {
	session.Stdout = attach.Stdout()
	session.Stderr = attach.Stderr()

	return &MacOSSession{
		attach:    attach,
		stdinPipe: stdinPipe,
		Session:   session,
	}
}

// SetupSessionIO sets up IO for the SSH session.
func (s *MacOSSession) SetupSessionIO(ctx context.Context) error {
	consoleSizeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	consoleSize := node.GetConsoleSize(consoleSizeCtx, s.attach)
	if s.attach.TTY() && consoleSize != nil {
		return setupTTYSession(ctx, s.Session, s.stdinPipe, s.attach, consoleSize)
	}

	return nil
}

// ExecuteCommand executes the provided command in the SSH session.
func (s *MacOSSession) ExecuteCommand(ctx context.Context, env []corev1.EnvVar, cmd []string) error {
	// Kubernetes defines cmd as an executable followed by its arguments. Do not
	// treat individual elements as shell input lines; callers that need shell
	// syntax must request it explicitly with an argv such as ["sh", "-c", script].
	cmdStr, err := utils.BuildExecCommandString(cmd, env)
	if err != nil {
		return err
	}
	if err := s.Start(cmdStr); err != nil {
		return err
	}

	if s.attach.TTY() {
		return s.Wait()
	}

	// If TTY is not enabled, copy stdin to stdinPipe in a synchronous manner
	if s.attach.Stdin() != nil {
		if _, err := io.Copy(s.stdinPipe, s.attach.Stdin()); err != nil {
			return err
		}
	}

	// Close stdinPipe to signal end of synchronous input
	if err := s.stdinPipe.Close(); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to close stdin pipe")
	}

	return s.Wait()
}

// setupTTYSession sets up TTY for the SSH session.
func setupTTYSession(ctx context.Context, session *ssh.Session, stdinPipe io.WriteCloser, attach api.AttachIO, consoleSize *[2]uint) error {
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,     // Enable echoing
		ssh.TTY_OP_ISPEED: 14400, // Input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // Output speed = 14.4kbaud
	}

	if err := session.RequestPty(node.GetTerminalType(), int(consoleSize[0]), int(consoleSize[1]), modes); err != nil {
		return fmt.Errorf("request for pseudo terminal failed: %v", err)
	}

	go node.HandleTerminalResizing(ctx, attach, func(size api.TermSize) error {
		return session.WindowChange(int(size.Height), int(size.Width))
	})

	if in := attach.Stdin(); in != nil {
		go func() {
			if _, err := io.Copy(stdinPipe, in); err != nil && err != io.EOF {
				log.G(ctx).WithError(err).Error("Failed to copy stdin to stdinPipe")
			}

			_ = session.Close()
		}()
	}

	return nil
}
