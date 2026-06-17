package envcfg

import (
	"os"
	"time"
)

// PostStartTimeoutEnv overrides the post-start timeout with a Go duration string
// (e.g. "5s"). Single source for the env var name.
const PostStartTimeoutEnv = "VZ_POSTSTART_TIMEOUT"

// defaultPostStartTimeout bounds the single post-start hook exec only, NOT the
// SSH-readiness probe loop (which is capped separately by VZ_SSH_READINESS_TIMEOUT).
// Keeping the hook budget off the readiness wait stops a slow guest login from
// being charged against the hook. k8s has no field for a custom post-start timeout,
// so this is the default (post-start should be lite anyway).
const defaultPostStartTimeout = 10 * time.Second

// PostStartTimeoutChecked resolves the post-start timeout, returning the
// ResolveDuration error (the default is still applied alongside it) so a caller
// can warn once at startup on an invalid/non-positive VZ_POSTSTART_TIMEOUT.
func PostStartTimeoutChecked() (time.Duration, error) {
	return ResolveDuration(os.Getenv(PostStartTimeoutEnv), defaultPostStartTimeout)
}
