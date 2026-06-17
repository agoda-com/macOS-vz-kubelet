package resourcemanager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// pollSSHReady is the extracted readiness loop; these cover it with a fake probe
// (no VM) so the retry / fail-fast / cancel-join / overall-cap contract is
// exercised without cgo. Tiny interval/timeout keep the test fast; the package is
// cgo-only so it runs on the macOS host, not in the dev container.
//
// pollSSHReady takes (retryInterval, attemptTimeout, overallTimeout): each attempt
// is bounded by attemptTimeout, and the WHOLE loop is bounded by overallTimeout so
// a permanently-unreachable sshd cannot retry forever.
const (
	testRetryInterval  = 1 * time.Millisecond
	testAttemptTimeout = 50 * time.Millisecond
	// Generous so the overall cap never fires for the success / fail-fast /
	// cancel-join / pre-cancelled contracts below: those terminate on their own
	// before the cap is relevant.
	testOverallTimeout = 10 * time.Second
)

// Probe succeeds on the first attempt -> nil, no retry.
func TestPollSSHReadySucceedsImmediately(t *testing.T) {
	calls := 0
	err := pollSSHReady(t.Context(), testRetryInterval, testAttemptTimeout, testOverallTimeout,
		func(context.Context) error {
			calls++
			return nil
		})

	require.NoError(t, err)
	assert.Equal(t, 1, calls, "a first-attempt success must not retry")
}

// A permanent SSH-config error fails fast: returned UNWRAPPED (errdefs.IsInvalidInput
// type-asserts + walks Cause, not errors.Unwrap), no retry.
func TestPollSSHReadyFailsFastOnInvalidInput(t *testing.T) {
	calls := 0
	cfgErr := errdefs.InvalidInputf("missing SSH user")
	err := pollSSHReady(t.Context(), testRetryInterval, testAttemptTimeout, testOverallTimeout,
		func(context.Context) error {
			calls++
			return cfgErr
		})

	require.Error(t, err)
	assert.True(t, errdefs.IsInvalidInput(err), "terminal config error must surface as InvalidInput")
	assert.Equal(t, 1, calls, "a permanent config error must not retry")
}

// Transient errors retry until the pod is removed (ctx cancel): the result joins the
// last probe error with the ctx cause so the caller learns why sshd never came up.
func TestPollSSHReadyJoinsLastErrOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	transient := errors.New("connection refused")
	calls := 0
	err := pollSSHReady(ctx, testRetryInterval, testAttemptTimeout, testOverallTimeout,
		func(context.Context) error {
			calls++
			if calls >= 3 {
				cancel() // pod removed mid-wait after a few transient failures
			}
			return transient
		})

	require.Error(t, err)
	assert.True(t, errors.Is(err, transient), "joined error must carry the last probe error")
	assert.True(t, errors.Is(err, context.Canceled), "joined error must carry the ctx cause")
	assert.GreaterOrEqual(t, calls, 3, "transient errors retry until cancel")
}

// ctx already cancelled before any attempt: the immediate=true poll still runs the
// condition once, so the probe fires against an already-cancelled attemptCtx and
// returns its ctx error; the loop then observes Done and returns a bare
// context.Canceled. The join is skipped (lastErr == ctx cause), and there is no panic.
func TestPollSSHReadyContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pollSSHReady(ctx, testRetryInterval, testAttemptTimeout, testOverallTimeout,
		func(attemptCtx context.Context) error {
			// Mirror a real exec against a dead ctx: surface the ctx error, not a fake
			// transient one, so the discriminator collapses to a bare cancellation.
			return attemptCtx.Err()
		})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "a pre-cancelled ctx returns its own error")
}

// REGRESSION GUARD: a permanently-unreachable sshd (probe ALWAYS returns a transient,
// non-InvalidInput error) must NOT retry forever. The overall cap fires and the loop
// RETURNS; the returned error joins the cap expiry (context.DeadlineExceeded) with the
// last probe error, the same errors.Join contract as the cancel case, so the caller
// learns both that the wait timed out AND why sshd never came up.
func TestPollSSHReadyCapsOverallWait(t *testing.T) {
	transient := errors.New("connection refused")
	const (
		overallTimeout = 30 * time.Millisecond
		attemptTimeout = 10 * time.Millisecond
	)

	// Run in a goroutine and assert it returns: if pollSSHReady regressed to an
	// uncapped loop this would hang, and the test fails on the deadline rather than
	// wedging the whole package.
	done := make(chan error, 1)
	go func() {
		done <- pollSSHReady(context.Background(), testRetryInterval, attemptTimeout, overallTimeout,
			func(context.Context) error {
				return transient
			})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollSSHReady did not return: a permanent failure must be capped, not retried forever")
	}

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the overall cap expiry must surface as DeadlineExceeded")
	assert.True(t, errors.Is(err, transient),
		"the capped error must still carry the last probe error so the caller learns why sshd never came up")
}

// Each attempt is bounded by attemptTimeout independently of the overall cap: the
// probe blocks on its attemptCtx and surfaces attemptCtx.Err() when the per-attempt
// deadline fires. With attemptTimeout < overallTimeout, at least one attempt must
// observe a FIRED attemptCtx, and the loop must still terminate by the overall cap.
// Timing tolerances stay loose for CI jitter; the oracle is "an attempt was bounded
// AND the loop returned", not exact counts or durations.
func TestPollSSHReadyPerAttemptTimeout(t *testing.T) {
	const (
		attemptTimeout = 10 * time.Millisecond
		overallTimeout = 50 * time.Millisecond
	)
	var sawFiredAttemptCtx atomic.Bool

	done := make(chan error, 1)
	go func() {
		done <- pollSSHReady(context.Background(), testRetryInterval, attemptTimeout, overallTimeout,
			func(attemptCtx context.Context) error {
				// Block until this attempt's deadline fires, then surface its error -
				// proving the attempt is bounded by attemptTimeout, not left to run
				// until the overall cap.
				<-attemptCtx.Done()
				if attemptCtx.Err() != nil {
					sawFiredAttemptCtx.Store(true)
				}
				return attemptCtx.Err()
			})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollSSHReady did not return: per-attempt and overall bounds must both terminate the loop")
	}

	assert.True(t, sawFiredAttemptCtx.Load(), "each attempt must be bounded by attemptTimeout")
	require.Error(t, err, "the loop must terminate by the overall cap, not run forever")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the overall cap expiry must surface as DeadlineExceeded")
}

// sshAuthFailureErr is the verbatim x/crypto handshake rejection string the guest
// sshd returns when it refuses authentication. A PLAIN string error, NOT an
// errdefs.InvalidInput, so the loop cannot fast-fail it by type; isSSHAuthFailure
// matches the substring strings.Contains(err.Error(), "unable to authenticate").
const sshAuthFailureErr = "ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"

// shortCapPollArgs returns a short overall cap + tiny interval so a wrong (retry-to-cap)
// impl caps fast instead of hanging; cases that must NOT fast-fail still observe the cap.
func shortCapPollArgs(t *testing.T) (retryInterval, attemptTimeout, overallTimeout time.Duration) {
	t.Helper()
	return 1 * time.Millisecond, 10 * time.Millisecond, 30 * time.Millisecond
}

// runPollWithWatchdog runs pollSSHReady in a goroutine and fails the test if it does
// not return within 5s, so a regressed (uncapped / never-fast-failing) impl surfaces as
// a clear failure instead of wedging the whole package. Mirrors the watchdog used by
// TestPollSSHReadyCapsOverallWait.
func runPollWithWatchdog(t *testing.T, ctx context.Context, retryInterval, attemptTimeout, overallTimeout time.Duration, probe func(context.Context) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- pollSSHReady(ctx, retryInterval, attemptTimeout, overallTimeout, probe)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("pollSSHReady did not return within 5s: it must fast-fail or cap, not retry forever")
		return nil // unreachable; t.Fatal stops the goroutine's test
	}
}

// An SSH auth rejection (the plain x/crypto string, NOT InvalidInput) must fast-fail
// TERMINALLY on the FIRST occurrence - reaching the auth phase means the
// handshake already succeeded (sshd is up) and it rejected our fixed VZ_SSH_USER/key, which
// is terminal. A rare early-boot rejection is handled by the pod-level Failed->recreate
// (the outer retry), not by retrying inside the probe. So fast-fail like the InvalidInput
// path, NOT retried to the overall cap.
func TestPollSSHReadyFastFailsOnAuthFailure(t *testing.T) {
	calls := 0
	retryInterval, attemptTimeout, overallTimeout := shortCapPollArgs(t)
	err := runPollWithWatchdog(t, context.Background(), retryInterval, attemptTimeout, overallTimeout,
		func(context.Context) error {
			calls++
			return errors.New(sshAuthFailureErr)
		})

	require.Error(t, err)
	assert.Equal(t, 1, calls,
		"an auth rejection must fast-fail on the first occurrence, not retry")
	assert.Contains(t, err.Error(), "unable to authenticate",
		"the terminal error must be the auth rejection")
	assert.False(t, errors.Is(err, context.DeadlineExceeded),
		"a fast-fail must NOT surface as the overall-cap DeadlineExceeded")
}

// The probe retries transient net errors while sshd boots, then fast-fails on the
// FIRST auth rejection once sshd accepts and refuses our creds.
func TestPollSSHReadyFastFailsOnAuthAfterTransient(t *testing.T) {
	calls := 0
	transient := errors.New("connection refused")
	retryInterval, attemptTimeout, overallTimeout := shortCapPollArgs(t)
	err := runPollWithWatchdog(t, context.Background(), retryInterval, attemptTimeout, overallTimeout,
		func(context.Context) error {
			calls++
			if calls < 3 {
				return transient // sshd still booting -> retry
			}
			return errors.New(sshAuthFailureErr) // sshd up, creds rejected -> fast-fail
		})

	require.Error(t, err)
	assert.Equal(t, 3, calls, "must retry transient refused while sshd boots, then fast-fail on the first auth rejection")
	assert.Contains(t, err.Error(), "unable to authenticate")
	assert.False(t, errors.Is(err, context.DeadlineExceeded), "auth rejection is a fast-fail, not a cap expiry")
}

// A non-auth, non-InvalidInput transient must STILL retry to the overall cap: the
// auth fast-fail must not change non-auth behavior. Mirror of
// TestPollSSHReadyCapsOverallWait, pinning the auth guard to NOT touch the
// plain-transient path.
func TestPollSSHReadyNonAuthTransientStillRetriesToCap(t *testing.T) {
	transient := errors.New("connection refused")
	retryInterval, attemptTimeout, overallTimeout := shortCapPollArgs(t)
	err := runPollWithWatchdog(t, context.Background(), retryInterval, attemptTimeout, overallTimeout,
		func(context.Context) error {
			return transient
		})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a non-auth transient must retry until the overall cap fires (DeadlineExceeded)")
	assert.True(t, errors.Is(err, transient),
		"the capped error must still carry the last probe error")
}

// The cap-fire terminal error must self-identify as the readiness-wait cap: the probe
// always returns a transient, the short cap fires, and the returned error string must
// CONTAIN "readiness wait expired" AND still satisfy the join contract
// (DeadlineExceeded + the transient).
func TestPollSSHReadyCapErrorMentionsReadinessWait(t *testing.T) {
	transient := errors.New("connection refused")
	retryInterval, attemptTimeout, overallTimeout := shortCapPollArgs(t)
	err := runPollWithWatchdog(t, context.Background(), retryInterval, attemptTimeout, overallTimeout,
		func(context.Context) error {
			return transient
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "readiness wait expired",
		"the cap-fire error must name the readiness-wait cap, not read as a single-attempt timeout")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the cap-fire error must still satisfy the DeadlineExceeded join")
	assert.True(t, errors.Is(err, transient),
		"the cap-fire error must still carry the last probe error (join preserved)")
}

// The cancel (pod-removal) path must NOT get the readiness-wait prefix: cancellation
// is normal teardown, not a cap expiry. The probe returns a transient and the ctx is
// cancelled on the 3rd call. The error must carry context.Canceled and its string
// must NOT contain "readiness wait expired". Regression guard that the prefix is
// applied only to the cap path.
func TestPollSSHReadyCancelErrorHasNoReadinessWaitPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transient := errors.New("connection refused")
	calls := 0
	err := pollSSHReady(ctx, testRetryInterval, testAttemptTimeout, testOverallTimeout,
		func(context.Context) error {
			calls++
			if calls >= 3 {
				cancel() // pod removed mid-wait
			}
			return transient
		})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"the cancel path must carry context.Canceled")
	assert.NotContains(t, err.Error(), "readiness wait expired",
		"cancellation is teardown, not a cap expiry: it must NOT get the readiness-wait prefix")
}
