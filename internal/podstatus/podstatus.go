// Package podstatus holds pure predicates for deriving pod/container readiness,
// kept free of cgo so they are unit-testable in the Linux dev container.
package podstatus

import corev1 "k8s.io/api/core/v1"

// HasExecPostStartHook reports whether c has an exec-shaped postStart hook, the only shape
// the provider runs; HTTPGet/TCPSocket/Sleep are ignored and never gate readiness.
// Lockstep with the executor in pkg/client/vz.go, which keys off the same shape.
func HasExecPostStartHook(c corev1.Container) bool {
	return c.Lifecycle != nil && c.Lifecycle.PostStart != nil && c.Lifecycle.PostStart.Exec != nil
}
