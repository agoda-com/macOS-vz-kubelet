package envcfg

import (
	"os"
	"time"
)

// defaultSSHReadinessTimeout caps the ENTIRE post-start SSH readiness probe loop
// (every attempt and inter-attempt wait together). It exists so a PERMANENT failure
// - rejected creds, a KEX mismatch, an sshd that never starts - surfaces as a pod
// failure (SetError -> Failed, every macOS pod hook or hookless) instead of retrying
// silently until the pod is deleted. Generous on purpose:
// the loop must churn past the transient guest mDNS .local login stall (see
// pkg/resourcemanager/macos_poststart.go waitForVirtualMachineSSHReady), which is
// far shorter than 60s, so the cap only fires on a genuinely stuck guest.
const defaultSSHReadinessTimeout = 60 * time.Second

// SSHReadinessTimeout returns the overall cap on the post-start SSH readiness loop.
// It defaults to 60s and is overridable via the VZ_SSH_READINESS_TIMEOUT environment
// variable (a Go duration string); an empty, blank, unparseable, or non-positive
// value falls back to the default.
func SSHReadinessTimeout() time.Duration {
	d, _ := ResolveDuration(os.Getenv("VZ_SSH_READINESS_TIMEOUT"), defaultSSHReadinessTimeout)
	return d
}
