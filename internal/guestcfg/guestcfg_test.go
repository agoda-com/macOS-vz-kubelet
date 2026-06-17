package guestcfg_test

import (
	"strings"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/guestcfg"

	"github.com/stretchr/testify/assert"
)

const hostnameFallback = "macos-vm"

// suffixLen: the per-(namespace,podName) hash suffix is 8 lowercase hex chars.
const suffixLen = 8

// Command is a 3-element shell exec: ["sh","-c",<script>].
func TestBuildPostStartProbeCommand_Shape(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Normal pod name", namespace: "default", podName: "gitlab-runner-macos-builder-5bd4cdbbfc-44znq"},
		{name: "Pod name with dots and uppercase", namespace: "team-a", podName: "My.Pod.Name"},
		{name: "Empty pod name", namespace: "default", podName: ""},
		{name: "Empty namespace", namespace: "", podName: "my-pod"},
		{name: "Empty namespace and pod name", namespace: "", podName: ""},
		{name: "Hostile pod name", namespace: "ns", podName: "a'b\\c d.e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName)
			if !assert.Len(t, cmd, 3, "command must be a 3-element slice") {
				return
			}
			assert.Equal(t, "sh", cmd[0], "cmd[0] must be sh")
			assert.Equal(t, "-c", cmd[1], "cmd[1] must be -c")
			assert.NotEmpty(t, cmd[2], "cmd[2] script must not be empty")
		})
	}
}

// Pins the exact wire script for a known (namespace,podName). The host name is
// "<sanitized name>-<8 hex of sha256(namespace+"/"+podName)>", so the literal
// suffix below pins the derivation: a name-only implementation cannot produce it.
// Suffix computed offline: printf '%s' 'default/My.Pod' | sha256sum | cut -c1-8.
func TestBuildPostStartProbeCommand_GoldenScript(t *testing.T) {
	cmd := guestcfg.BuildPostStartProbeCommand("default", "My.Pod")

	const host = "My-Pod-15479e83"
	assert.Equal(t, []string{
		"sh",
		"-c",
		"sudo -n defaults write /Library/Preferences/com.apple.mDNSResponder.plist NoMulticastAdvertisements -bool YES; " +
			"sudo -n scutil --set LocalHostName " + host + "; " +
			"sudo -n scutil --set ComputerName " + host + "; " +
			"sudo -n scutil --set HostName " + host + "; " +
			"sudo -n killall mDNSResponder 2>/dev/null; " +
			"true",
	}, cmd, "the post-start probe script must match the live-validated commands exactly")
}

// Script carries the mDNS-hygiene steps for any pod name.
func TestBuildPostStartProbeCommand_HygieneSteps(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Normal pod name", namespace: "default", podName: "gitlab-runner-macos-builder-5bd4cdbbfc-44znq"},
		{name: "Pod name with dots and uppercase", namespace: "team-a", podName: "My.Pod.Name"},
		{name: "Empty pod name", namespace: "default", podName: ""},
		{name: "Empty namespace", namespace: "", podName: "my-pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := scriptOf(t, guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName))

			assert.Contains(t, script, "NoMulticastAdvertisements -bool YES",
				"must disable mDNS multicast advertising")
			assert.Contains(t, script, "scutil --set LocalHostName",
				"must set LocalHostName")
			assert.Contains(t, script, "scutil --set ComputerName",
				"must set ComputerName")
			assert.Contains(t, script, "scutil --set HostName",
				"must set HostName")
			assert.Contains(t, script, "killall mDNSResponder",
				"must restart mDNSResponder to apply the flag")
		})
	}
}

// Script ends with a trailing "true" so the probe exits 0 once SSH succeeds.
func TestBuildPostStartProbeCommand_ExitsZero(t *testing.T) {
	script := scriptOf(t, guestcfg.BuildPostStartProbeCommand("default", "any-pod"))
	assert.True(t, strings.HasSuffix(strings.TrimSpace(script), "true"),
		"script must end with 'true' so it exits 0")
}

// Headline fix: two pods with the SAME name in DIFFERENT namespaces must get
// DISTINCT guest host names, or they re-create the Bonjour -NN mDNS conflict.
// Both still carry the same sanitized-name base prefix.
func TestBuildPostStartProbeCommand_SameNameDifferentNamespace_DistinctHosts(t *testing.T) {
	const podName = "gitlab-runner-macos-builder-5bd4cdbbfc-44znq"

	hostA := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand("team-a", podName)))
	hostB := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand("team-b", podName)))

	assert.NotEqual(t, hostA, hostB,
		"same pod name in different namespaces must yield distinct host names")

	for _, h := range []string{hostA, hostB} {
		assertValidHostName(t, h)
		assert.True(t, strings.HasPrefix(h, podName+"-"),
			"host name %q must carry the sanitized-name base prefix", h)
	}
}

// Bonus collision class: two RAW names that sanitize to the SAME base (dots and
// hyphens both map to '-') must still get DISTINCT host names, because the hash
// is computed over the raw, unsanitized name.
func TestBuildPostStartProbeCommand_SanitizeCollision_DistinctHosts(t *testing.T) {
	const ns = "ns1"

	hostDot := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand(ns, "a.b")))
	hostDash := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand(ns, "a-b")))

	// Same sanitized base, so a name-only derivation would collide.
	assert.True(t, strings.HasPrefix(hostDot, "a-b-"), "%q must share the sanitized base a-b", hostDot)
	assert.True(t, strings.HasPrefix(hostDash, "a-b-"), "%q must share the sanitized base a-b", hostDash)
	assert.NotEqual(t, hostDot, hostDash,
		"raw names that sanitize identically must still yield distinct host names")
}

// Same (namespace,podName) is byte-identical across calls: the derivation is pure.
func TestBuildPostStartProbeCommand_Deterministic(t *testing.T) {
	cmd1 := guestcfg.BuildPostStartProbeCommand("team-a", "My.Pod")
	cmd2 := guestcfg.BuildPostStartProbeCommand("team-a", "My.Pod")
	assert.Equal(t, cmd1, cmd2, "same inputs must produce a byte-identical command")
}

// Host name is always "<base>-<8 hex>", <=63 chars, charset [A-Za-z0-9-].
func TestBuildPostStartProbeCommand_HostNameLengthAndShape(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Over-long pod name truncates to <=63", namespace: "long-namespace", podName: strings.Repeat("a", 200)},
		{name: "Over-long with invalid runes truncates to <=63", namespace: "ns", podName: strings.Repeat("a.b", 100)},
		{name: "Exactly 63 stays <=63", namespace: "ns", podName: strings.Repeat("a", 63)},
		{name: "Short name still suffixed", namespace: "default", podName: "my-pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName)))

			assertValidHostName(t, host)
			base, suffix := splitHostName(t, host)
			assert.NotEmpty(t, base, "host name must carry a non-empty base before the suffix")
			assertHexSuffix(t, suffix)
		})
	}
}

// Base is capped at exactly 54 (63 - 1 - 8) so the "-<8 hex>" suffix always fits.
// 54 valid chars -> base 54 (right at the cap); 55 -> base still 54 (one over).
func TestBuildPostStartProbeCommand_BaseTruncationBoundary(t *testing.T) {
	const maxBaseLen = 54

	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Exactly 54 valid chars: base at the cap", namespace: "ns", podName: strings.Repeat("a", 54)},
		{name: "Exactly 55 valid chars: base capped to 54", namespace: "ns", podName: strings.Repeat("a", 55)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName)))

			base, suffix := splitHostName(t, host)
			assert.Len(t, base, maxBaseLen, "base must be capped at exactly %d chars", maxBaseLen)
			assertHexSuffix(t, suffix)
			assert.Len(t, host, 63, "base 54 + '-' + 8 hex must total 63")
		})
	}
}

// Every host name, hostile input included, is "<base>-<8 lowercase hex>".
func TestBuildPostStartProbeCommand_SuffixIsEightHex(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Normal", namespace: "default", podName: "my-pod"},
		{name: "Dots and uppercase", namespace: "team-a", podName: "My.Pod.Name"},
		{name: "Empty namespace", namespace: "", podName: "my-pod"},
		{name: "Empty pod name", namespace: "default", podName: ""},
		{name: "All-invalid pod name", namespace: "somens", podName: "..."},
		{name: "Hostile pod name", namespace: "ns", podName: "a'b\\c d.e"},
		{name: "Hostile namespace", namespace: "n's\\$(x)", podName: "my-pod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName)))
			_, suffix := splitHostName(t, host)
			assertHexSuffix(t, suffix)
		})
	}
}

// All-invalid pod name still produces a usable host name: the fallback base is
// suffixed like any other ("macos-vm-<8 hex>"), so distinct namespaces stay distinct.
func TestBuildPostStartProbeCommand_FallbackBaseStillSuffixed(t *testing.T) {
	host := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand("somens", "...")))

	// Suffix computed offline: printf '%s' 'somens/...' | sha256sum | cut -c1-8.
	assert.Equal(t, hostnameFallback+"-1917a3d0", host,
		"all-invalid name falls back to the suffixed fallback base")

	base, suffix := splitHostName(t, host)
	assert.Equal(t, hostnameFallback, base, "fallback base must be %q", hostnameFallback)
	assertHexSuffix(t, suffix)

	// Two namespaces, both all-invalid name, must NOT collide on the fallback.
	other := localHostNameOf(t, scriptOf(t, guestcfg.BuildPostStartProbeCommand("otherns", "...")))
	assert.NotEqual(t, host, other,
		"fallback host names must differ across namespaces")
}

// Load-bearing invariant: the caller wraps the script in $'...', so it must never
// contain a single-quote or backslash for ANY input, including hostile NAMESPACES
// (the namespace reaches the script only via the hex hash, never verbatim).
func TestBuildPostStartProbeCommand_NoQuoteOrBackslash(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
	}{
		{name: "Hostile pod name with quote backslash space dot", namespace: "default", podName: "a'b\\c d.e"},
		{name: "Single quotes in pod name", namespace: "default", podName: "'''"},
		{name: "Backslashes in pod name", namespace: "default", podName: "\\\\\\"},
		{name: "Mixed shell metacharacters in pod name", namespace: "default", podName: "p'o\"d$(rm)`x`;\\n"},
		{name: "Unicode and symbols in pod name", namespace: "default", podName: "pod-\u540d\u524d-\u2713!@#"},
		{name: "Hostile namespace with quotes", namespace: "n's'\"", podName: "my-pod"},
		{name: "Hostile namespace with backslashes", namespace: "n\\s\\\\", podName: "my-pod"},
		{name: "Hostile namespace with command substitution", namespace: "$(rm -rf /)`x`", podName: "my-pod"},
		{name: "Unicode namespace", namespace: "ns-\u540d\u524d-\u2713", podName: "my-pod"},
		{name: "Empty", namespace: "", podName: ""},
		{name: "Normal", namespace: "default", podName: "gitlab-runner-macos-builder-5bd4cdbbfc-44znq"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := scriptOf(t, guestcfg.BuildPostStartProbeCommand(tt.namespace, tt.podName))

			assert.NotContains(t, script, "'",
				"script must contain no single-quote (breaks the $'...' wrapper)")
			assert.NotContains(t, script, "\\",
				"script must contain no backslash (breaks the $'...' wrapper)")
		})
	}
}

// scriptOf extracts cmd[2], failing the test if the command shape is wrong.
func scriptOf(t *testing.T, cmd []string) string {
	t.Helper()
	if !assert.Len(t, cmd, 3, "command must be a 3-element slice to extract the script") {
		return ""
	}
	return cmd[2]
}

// localHostNameOf extracts the argument passed to "scutil --set LocalHostName".
func localHostNameOf(t *testing.T, script string) string {
	t.Helper()
	const marker = "scutil --set LocalHostName "
	idx := strings.Index(script, marker)
	if !assert.GreaterOrEqual(t, idx, 0, "script must contain %q", marker) {
		return ""
	}
	rest := script[idx+len(marker):]
	if end := strings.IndexByte(rest, ';'); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// splitHostName splits "<base>-<8 hex>" into base and suffix on the last '-'.
func splitHostName(t *testing.T, host string) (base, suffix string) {
	t.Helper()
	i := strings.LastIndexByte(host, '-')
	if !assert.GreaterOrEqual(t, i, 0, "host name %q must contain a '-' before the suffix", host) {
		return "", ""
	}
	return host[:i], host[i+1:]
}

// assertHexSuffix checks the suffix is exactly 8 chars of [0-9a-f].
func assertHexSuffix(t *testing.T, suffix string) {
	t.Helper()
	if !assert.Len(t, suffix, suffixLen, "suffix must be exactly %d chars", suffixLen) {
		return
	}
	for _, r := range suffix {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		assert.True(t, ok, "suffix rune %q must be lowercase hex", string(r))
	}
}

// assertValidHostName checks the LocalHostName invariant: non-empty, <=63 chars, [A-Za-z0-9-].
func assertValidHostName(t *testing.T, host string) {
	t.Helper()
	assert.NotEmpty(t, host, "host name must not be empty")
	assert.LessOrEqual(t, len(host), 63, "host name must be at most 63 characters")
	for _, r := range host {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		assert.True(t, ok, "host name rune %q must be in [A-Za-z0-9-]", string(r))
	}
}
