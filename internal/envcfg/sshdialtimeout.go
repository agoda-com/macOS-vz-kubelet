package envcfg

import (
	"os"
	"time"
)

// defaultSSHDialTimeout bounds TCP connect + SSH handshake so neither a black-hole
// VM IP nor a guest stuck in the mDNS .local login stall can hang the dial. Tight
// by design: the SSH client is cached per VM (internal/sshconn), so this is paid
// once per VM on the initial dial / rare reconnect, never on steady-state reuse.
const defaultSSHDialTimeout = 5 * time.Second

// SSHDialTimeout returns the bounded timeout for SSH dials, covering TCP connect
// + SSH handshake (see internal/ssh.DialContext). It defaults to 5s and is
// overridable via the VZ_SSH_DIAL_TIMEOUT environment variable (a Go duration
// string); an empty, blank, unparseable, or non-positive value falls back to the
// default.
func SSHDialTimeout() time.Duration {
	d, _ := ResolveDuration(os.Getenv("VZ_SSH_DIAL_TIMEOUT"), defaultSSHDialTimeout)
	return d
}
