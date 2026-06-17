package envcfg_test

import (
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
)

// defaultReadinessTimeout is the overall SSH-readiness cap used when the env var
// is unset, empty, or invalid. Mirrors the documented default for
// SSHReadinessTimeout. Unlike the per-attempt probe budget, this caps the WHOLE
// readiness loop so a permanently-unreachable sshd does not retry forever.
const defaultReadinessTimeout = 60 * time.Second

// SSHReadinessTimeout: unset env -> 60s default; a valid Go duration -> that
// value; empty/whitespace/unparseable/bare-number/non-positive -> default. This
// is the overall cap on the readiness loop (every attempt and inter-attempt wait
// together), distinct from the per-attempt budget.
func TestSSHReadinessTimeout(t *testing.T) {
	tests := []struct {
		name string
		// set reports whether VZ_SSH_READINESS_TIMEOUT is present at all; when
		// false the env var is left unset so the default path is exercised.
		set   bool
		value string
		want  time.Duration
	}{
		{name: "Unset falls back to 60s default", set: false, want: defaultReadinessTimeout},
		{name: "Valid 30s override", set: true, value: "30s", want: 30 * time.Second},
		{name: "Valid sub-second override", set: true, value: "500ms", want: 500 * time.Millisecond},
		{name: "Valid minutes override", set: true, value: "2m", want: 2 * time.Minute},
		{name: "Empty falls back to default", set: true, value: "", want: defaultReadinessTimeout},
		{name: "Whitespace-only falls back to default", set: true, value: "   ", want: defaultReadinessTimeout},
		{name: "Unparseable garbage falls back to default", set: true, value: "abc", want: defaultReadinessTimeout},
		{name: "Bare number without unit falls back to default", set: true, value: "30", want: defaultReadinessTimeout},
		{name: "Zero falls back to default", set: true, value: "0s", want: defaultReadinessTimeout},
		{name: "Negative falls back to default", set: true, value: "-5s", want: defaultReadinessTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("VZ_SSH_READINESS_TIMEOUT", tt.value)
			} else {
				// Ensure no ambient value leaks in from the host environment.
				// t.Setenv then unset within the subtest keeps the default path honest.
				t.Setenv("VZ_SSH_READINESS_TIMEOUT", "")
			}
			got := envcfg.SSHReadinessTimeout()
			assert.Equal(t, tt.want, got, "resolved SSH readiness timeout")
		})
	}
}

// The default is exactly 60s (not merely non-zero): the overall readiness cap is
// the contract that prevents an unreachable sshd from retrying forever, and 60s
// is that contract.
func TestSSHReadinessTimeout_DefaultIs60s(t *testing.T) {
	t.Setenv("VZ_SSH_READINESS_TIMEOUT", "")
	assert.Equal(t, 60*time.Second, envcfg.SSHReadinessTimeout(),
		"the unset/invalid default must be exactly 60s")
}
