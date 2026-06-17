// Package execerror turns SSH exit errors into a typed exit status vk's exec
// server can surface.
package execerror

import (
	"errors"

	utilexec "k8s.io/utils/exec"
)

// ExitStatusError is an error carrying a remote command exit status. SSH exit
// errors (ssh.ExitError) satisfy it; transport failures and *ssh.ExitMissingError
// do not.
type ExitStatusError interface {
	error
	ExitStatus() int
}

// AsCodeExitError wraps an error carrying a remote exit status in a CodeExitError,
// UNWRAPPED so vk's ServeExec type-assert surfaces a non-zero exit code. Detection
// is by interface (ssh.ExitError has unexported fields). *ssh.ExitMissingError (no
// ExitStatus), nil, and non-exit errors pass through unchanged.
func AsCodeExitError(err error) error {
	if es, ok := errors.AsType[ExitStatusError](err); ok {
		return utilexec.CodeExitError{Err: err, Code: es.ExitStatus()}
	}
	return err
}
