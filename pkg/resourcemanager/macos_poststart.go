package resourcemanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
	"github.com/agoda-com/macOS-vz-kubelet/internal/guestcfg"
	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"k8s.io/apimachinery/pkg/util/wait"
)

// postStartRetryInterval is the wait between post-start SSH readiness probes
// while the guest sshd is not yet accepting connections.
const postStartRetryInterval = 1 * time.Second

// sshProbeExecBudget bounds only the readiness-probe exec phase (a trivial no-op
// exec). The per-attempt budget is this PLUS a full dial budget: the dial is
// independently capped at envcfg.SSHDialTimeout (config.Timeout in macos_ssh.go
// sshDialFunc), so summing guarantees the exec phase always gets its whole budget
// regardless of how long the dial takes, never clipped by a slow dial.
const sshProbeExecBudget = 10 * time.Second

// execPostStartAction executes the post-start action inside the virtual machine.
func (c *MacOSClient) execPostStartAction(ctx context.Context, namespace, name string, action resource.ExecAction) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.execPostStart")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace": namespace,
		"name":      name,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Debugf("Executing post-start action: %+v", action)
	logger.Info("Virtual machine is running, executing post-start command")

	// Mirror the docker backend: the VM reports an ARP IP before sshd accepts and
	// early handshakes are slow. Probe until sshd is reachable (bounded by ctx),
	// then run the hook exactly once: only the probe retries, never the hook.
	err = c.waitForVirtualMachineSSHReady(ctx, namespace, name)
	if err != nil {
		return err
	}
	log.G(ctx).Debug("post-start SSH readiness probe succeeded; executing hook")

	execCtx, cancel := context.WithTimeout(ctx, action.TimeoutDuration)
	defer cancel()
	err = c.ExecInVirtualMachine(execCtx, namespace, name, action.Command, node.DiscardingExecIO())
	if execCtx.Err() != nil {
		// Prefer the timeout cause over the transport error it surfaces as.
		return execCtx.Err()
	}
	return err
}

// waitForVirtualMachineSSHReady retries the readiness probe until sshd accepts a
// session, a permanent SSH-config error surfaces, the overall cap fires, or the pod
// is removed (ctx cancel), mirroring the docker backend's wait-for-running. The whole
// loop is capped by envcfg.SSHReadinessTimeout so a permanent failure surfaces rather
// than retrying until pod deletion; a transient guest login stall is churned past
// within that cap, NOT charged against the hook timeout.
//
// The probe (guestcfg.BuildPostStartProbeCommand, per pod) doubles as one-time
// best-effort guest hygiene: VMs share a base hostname advertised on mDNS,
// polluting the shared VLAN's .local namespace. It disables mDNS advertising,
// sets a per-(namespace,pod) host name, restarts mDNSResponder. Always exits 0,
// so it gates only on sshd and is safe to repeat; the real hook is unaffected
// (runs once).
//
// Called by both the hook path (probe then hook) and the hookless path (probe
// alone), so every VM gets the hygiene fix-up, not just pods with a postStart hook.
func (c *MacOSClient) waitForVirtualMachineSSHReady(ctx context.Context, namespace, name string) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.waitForVirtualMachineSSHReady")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace": namespace,
		"name":      name,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	probeCmd := guestcfg.BuildPostStartProbeCommand(namespace, name)
	// Per attempt = a full dial budget PLUS a full probe-exec budget (see
	// sshProbeExecBudget for why they are summed).
	attemptTimeout := envcfg.SSHDialTimeout() + sshProbeExecBudget
	err = pollSSHReady(ctx, postStartRetryInterval, attemptTimeout, envcfg.SSHReadinessTimeout(),
		func(attemptCtx context.Context) error {
			return c.ExecInVirtualMachine(attemptCtx, namespace, name, probeCmd, node.DiscardingExecIO())
		})
	return err
}

// isSSHAuthFailure reports whether err is a terminal SSH client authentication
// rejection (wrong user/key/password). golang.org/x/crypto/ssh returns this as a
// plain unwrapped string ("ssh: unable to authenticate, attempted methods [...],
// no supported methods remain") with no typed sentinel, so we match the stable
// substring. KEX failures use a distinct typed error
// (*ssh.AlgorithmNegotiationError, "no common algorithm") and net errors never reach
// the auth phase, so this does not catch transient connect/handshake failures. If
// x/crypto ever changes the text this returns false and the probe simply falls back
// to retry-to-cap (today's behavior), a safe degradation, never a hang.
func isSSHAuthFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unable to authenticate")
}

// pollSSHReady runs probe on retryInterval until it succeeds, returns a permanent
// SSH-config error, the overall cap fires, or ctx is cancelled. Each attempt is
// bounded by attemptTimeout so a stalled handshake or slow exec cannot wedge a single
// attempt; the whole loop is capped by overallTimeout so a permanent failure (rejected
// creds, sshd never up) surfaces rather than retrying forever. On cap expiry or cancel
// it joins the last probe error with the ctx cause (context.DeadlineExceeded for the
// cap, context.Canceled for pod removal) so the caller learns both that the wait ended
// AND why sshd never came up. Extracted from waitForVirtualMachineSSHReady so the loop
// is unit-testable with a fake probe (no VM); the wrapper supplies the real
// ExecInVirtualMachine closure.
func pollSSHReady(ctx context.Context, retryInterval, attemptTimeout, overallTimeout time.Duration, probe func(context.Context) error) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, retryInterval, overallTimeout, true,
		func(ctx context.Context) (bool, error) {
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			defer cancel()
			lastErr = probe(attemptCtx)
			if lastErr == nil {
				return true, nil
			}
			if errdefs.IsInvalidInput(lastErr) {
				// Terminal: errdefs.IsInvalidInput type-asserts and walks Cause(), it does
				// NOT follow errors.Unwrap/%w. So a permanent SSH-config error (missing
				// user, bad key) must reach here un-%w-wrapped or fail-fast silently
				// degrades to infinite retry. Returned UNWRAPPED to preserve that.
				return false, lastErr
			}
			if isSSHAuthFailure(lastErr) {
				// Terminal: the handshake reached auth and the server rejected our credentials.
				// A wrong VZ_SSH_USER/key is permanent across retries, so fail now instead of
				// burning the whole readiness budget. A rare transient early-boot rejection is
				// handled by the pod-level Failed->recreate (the outer retry), not by retrying
				// inside this probe. Returned unwrapped, mirroring the IsInvalidInput fast-fail.
				return false, lastErr
			}
			log.G(ctx).WithError(lastErr).Debug("post-start SSH readiness probe failed, retrying")
			return false, nil
		})
	if errors.Is(err, context.DeadlineExceeded) {
		// Cap fired: name the readiness-wait cap so the caller does not read this as a
		// single-attempt timeout. %w preserves errors.Is(DeadlineExceeded). The cancel
		// (pod-removal) path is deliberately left unprefixed: it is normal teardown.
		err = fmt.Errorf("SSH readiness wait expired after %s: %w", overallTimeout, err)
	}
	if err != nil && lastErr != nil && !errors.Is(err, lastErr) {
		// Loop ended without sshd accepting - ctx cancelled (pod removed) or the
		// overall cap fired (DeadlineExceeded): name why alongside the last probe error.
		return errors.Join(lastErr, err)
	}
	return err
}
