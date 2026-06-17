package resource

import (
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	corev1 "k8s.io/api/core/v1"
)

// VirtualMachineState represents the state of a macOS virtual machine.
type VirtualMachineState int

const (
	// VM is preparing.
	VirtualMachineStatePreparing VirtualMachineState = iota

	// VM is starting.
	VirtualMachineStateStarting

	// VM is running.
	VirtualMachineStateRunning

	// VM is terminating.
	VirtualMachineStateTerminating

	// VM has terminated.
	VirtualMachineStateTerminated

	// VM has failed.
	VirtualMachineStateFailed
)

type VirtualMachine interface {
	// Env returns the environment variables for the virtual machine.
	Env() []corev1.EnvVar

	// State returns the current state of the virtual machine.
	State() VirtualMachineState

	// Error returns the error state of the virtual machine.
	Error() error

	// SetError sets the error state of the virtual machine.
	SetError(err error)

	// IPAddress returns the IP address of the virtual machine.
	IPAddress() string

	// StartedAt returns the start time of the virtual machine.
	StartedAt() *time.Time

	// FinishedAt returns the finish time of the virtual machine.
	FinishedAt() *time.Time

	// PostStartFinishedAt returns the postStart hook success time, or nil if not finished
	// (no hook, still running, or failed).
	PostStartFinishedAt() *time.Time
}

// MacOSVirtualMachine represents a macOS virtual machine instance along with its error state.
type MacOSVirtualMachine struct {
	env                 []corev1.EnvVar            // Environment variables for the virtual machine.
	instance            *vm.VirtualMachineInstance // The underlying virtual machine instance.
	err                 error                      // Error state of the virtual machine.
	postStartFinishedAt time.Time                  // set once postStart hook succeeds; zero = not finished
}

// NewMacOSVirtualMachine creates a new instance of MacOSVirtualMachine.
func NewMacOSVirtualMachine(env []corev1.EnvVar) MacOSVirtualMachine {
	return MacOSVirtualMachine{
		env: env,
	}
}

// Environment returns the environment variables for the macOS virtual machine.
func (m *MacOSVirtualMachine) Env() []corev1.EnvVar {
	return m.env
}

// Instance returns the internal VirtualMachineInstance.
func (m *MacOSVirtualMachine) Instance() *vm.VirtualMachineInstance {
	return m.instance
}

// SetInstance sets the internal VirtualMachineInstance.
func (m *MacOSVirtualMachine) SetInstance(instance *vm.VirtualMachineInstance) {
	m.instance = instance
}

// State returns the current state of the macOS virtual machine.
func (m *MacOSVirtualMachine) State() VirtualMachineState {
	if m.err != nil {
		return VirtualMachineStateFailed
	}

	instance := m.instance
	if instance == nil {
		return VirtualMachineStatePreparing
	}

	switch instance.State() {
	case vz.VirtualMachineStateStarting:
		return VirtualMachineStateStarting
	case vz.VirtualMachineStateRunning:
		return VirtualMachineStateRunning
	case vz.VirtualMachineStateStopping:
		return VirtualMachineStateTerminating
	case vz.VirtualMachineStateStopped:
		return VirtualMachineStateTerminated
	default: // consider all other states as failed
	}

	return VirtualMachineStateFailed
}

// Error returns the error state of the macOS virtual machine.
func (m *MacOSVirtualMachine) Error() error {
	return m.err
}

// SetError sets the error state of the macOS virtual machine.
func (m *MacOSVirtualMachine) SetError(err error) {
	m.err = err
}

// IPAddress returns the IP address of the macOS virtual machine.
func (m *MacOSVirtualMachine) IPAddress() string {
	if m.instance == nil {
		return ""
	}

	return m.instance.IPAddress
}

// StartedAt returns the start time of the macOS virtual machine.
func (m *MacOSVirtualMachine) StartedAt() *time.Time {
	if m.instance == nil {
		return nil
	}

	return m.instance.StartedAt
}

// FinishedAt returns the finish time of the macOS virtual machine.
func (m *MacOSVirtualMachine) FinishedAt() *time.Time {
	if m.instance == nil {
		return nil
	}

	return m.instance.FinishedAt
}

// PostStartFinishedAt returns the post-start success time (the SSH readiness probe,
// plus the exec hook when one exists), or nil if not finished (still running or failed).
func (m *MacOSVirtualMachine) PostStartFinishedAt() *time.Time {
	if m.postStartFinishedAt.IsZero() {
		return nil
	}
	return &m.postStartFinishedAt
}

// SetPostStartFinishedAt records successful post-start completion (the SSH readiness
// probe, plus the exec hook when one exists).
func (m *MacOSVirtualMachine) SetPostStartFinishedAt(t time.Time) {
	m.postStartFinishedAt = t
}
