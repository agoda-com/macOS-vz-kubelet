package envcfg_test

import (
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
)

// defaultDialTimeout is the bounded dial timeout used when the env var is unset,
// empty, or invalid. Mirrors the documented default for SSHDialTimeout.
const defaultDialTimeout = 5 * time.Second

// SSHDialTimeout: unset env -> 5s default; a valid Go duration -> that value;
// empty/whitespace -> default; an unparseable value -> default. This bounds TCP
// connect + SSH handshake on the deadline-less exec/attach path so a black-hole
// VM IP, or a guest stuck in the mDNS .local login stall, can no longer hang the
// dial. Safe to keep tight because the SSH client is cached per VM.
func TestSSHDialTimeout(t *testing.T) {
	tests := []struct {
		name string
		// set reports whether VZ_SSH_DIAL_TIMEOUT is present at all; when false
		// the env var is left unset so the default path is exercised.
		set   bool
		value string
		want  time.Duration
	}{
		{name: "Unset falls back to 5s default", set: false, want: defaultDialTimeout},
		{name: "Valid 30s override", set: true, value: "30s", want: 30 * time.Second},
		{name: "Valid sub-second override", set: true, value: "500ms", want: 500 * time.Millisecond},
		{name: "Valid minutes override", set: true, value: "2m", want: 2 * time.Minute},
		{name: "Empty falls back to default", set: true, value: "", want: defaultDialTimeout},
		{name: "Whitespace-only falls back to default", set: true, value: "   ", want: defaultDialTimeout},
		{name: "Unparseable garbage falls back to default", set: true, value: "abc", want: defaultDialTimeout},
		{name: "Bare number without unit falls back to default", set: true, value: "30", want: defaultDialTimeout},
		{name: "Zero falls back to default", set: true, value: "0s", want: defaultDialTimeout},
		{name: "Negative falls back to default", set: true, value: "-5s", want: defaultDialTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("VZ_SSH_DIAL_TIMEOUT", tt.value)
			} else {
				// Ensure no ambient value leaks in from the host environment.
				// t.Setenv then unset within the subtest keeps the default path honest.
				t.Setenv("VZ_SSH_DIAL_TIMEOUT", "")
			}
			got := envcfg.SSHDialTimeout()
			assert.Equal(t, tt.want, got, "resolved SSH dial timeout")
		})
	}
}

// The default is exactly 5s (not merely non-zero): the bounded dial must be a
// few-second cap, and 5s is the documented contract.
func TestSSHDialTimeout_DefaultIs5s(t *testing.T) {
	t.Setenv("VZ_SSH_DIAL_TIMEOUT", "")
	assert.Equal(t, 5*time.Second, envcfg.SSHDialTimeout(),
		"the unset/invalid default must be exactly 5s")
}
