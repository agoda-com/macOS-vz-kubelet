package vm

import (
	"context"

	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
)

// VirtualMachineInfo stores the information about macOS virtual machine
type VirtualMachineInfo struct {
	Ref                string
	Resource           resource.MacOSVirtualMachine
	DownloadCancelFunc context.CancelFunc
	PermitAcquired     bool

	// SSHConn is the per-VM persistent SSH connection: one cached client shared
	// across all exec sessions for this VM, reconnecting on transport death. Nil
	// until CreateVirtualMachine eager-creates it; closed in DeleteVirtualMachine.
	SSHConn *sshconn.Connection
}
