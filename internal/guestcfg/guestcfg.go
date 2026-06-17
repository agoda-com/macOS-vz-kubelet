// Package guestcfg builds the macOS-VM post-start guest-config commands.
// Pure Go (no cgo, no macOS syscalls); the Darwin provider wires it into the SSH probe.
package guestcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// hostNameFallback: used when the sanitized pod name is empty. Valid LocalHostName, shell-safe.
const hostNameFallback = "macos-vm"

// suffixHexLen: the host name carries a per-(namespace,podName) hash suffix of 8 hex chars.
const suffixHexLen = 8

// maxBaseLen caps the sanitized base so base + "-" + 8-hex suffix stays within the
// 63-char LocalHostName limit.
const maxBaseLen = 63 - 1 - suffixHexLen

// BuildPostStartProbeCommand returns the post-start readiness-probe command
// ["sh","-c",<script>], which also does one-time best-effort guest hygiene against
// shared-VLAN mDNS .local pollution: disable mDNS advertising, set the host name to
// the sanitized pod name plus a per-(namespace,podName) hash suffix (so VMs stop
// colliding on one .local name), restart mDNSResponder. Always exits 0 once SSH
// connects, so the probe gates on sshd, not hygiene.
//
// Host name carries a per-(namespace,podName) hash suffix (see hostName) so VMs
// with colliding sanitized bases stay distinct.
//
// Quote-safety: the caller $'...'-wraps cmd[2], so the script must contain NO
// single-quote ' and NO backslash \, but DOUBLE-quotes " ARE allowed. Sanitized
// host names and hex suffixes are shell-safe.
func BuildPostStartProbeCommand(namespace, podName string) []string {
	h := hostName(namespace, podName)
	steps := []string{
		"sudo -n defaults write /Library/Preferences/com.apple.mDNSResponder.plist NoMulticastAdvertisements -bool YES",
		"sudo -n scutil --set LocalHostName " + h,
		"sudo -n scutil --set ComputerName " + h,
		"sudo -n scutil --set HostName " + h,
		"sudo -n killall mDNSResponder 2>/dev/null",
		// Best-effort exit 0: probe gates on sshd, not hygiene.
		"true",
	}
	return []string{"sh", "-c", strings.Join(steps, "; ")}
}

// hostName builds "<base>-<8 hex>": base is the sanitized pod name, suffix is the
// first 8 hex of sha256(namespace+"/"+podName) over the RAW strings. Total <=63.
// Only the hex digest reaches the script, never namespace/podName verbatim - so
// quote-safety stays structural and the suffix keys uniqueness even when bases
// collide (dots and hyphens both sanitize to '-').
func hostName(namespace, podName string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + podName))
	suffix := hex.EncodeToString(sum[:])[:suffixHexLen]
	return sanitizeBase(podName) + "-" + suffix
}

// sanitizeBase maps a pod name to a valid LocalHostName base, safe unquoted in the
// caller's $'...' wrapper: keep [A-Za-z0-9-] (else '-'), cap at maxBaseLen, trim
// leading/trailing '-' (a leading '-' looks like a scutil flag; truncation can
// expose a trailing one), fall back to hostNameFallback if nothing survives. No
// dots, spaces, quotes, or backslashes.
func sanitizeBase(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= maxBaseLen {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return hostNameFallback
	}
	return out
}
