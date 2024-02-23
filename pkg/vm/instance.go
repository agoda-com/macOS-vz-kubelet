package vm

import (
	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/config"
)

type VirtualMachineInstance struct {
	Namespace string
	Name      string

	*vz.VirtualMachine
}

func NewVirtualMachineInstance(namespace, name string, config *config.VirtualMachineConfiguration) (*VirtualMachineInstance, error) {
	vm, err := vz.NewVirtualMachine(config.VirtualMachineConfiguration)
	if err != nil {
		return nil, err
	}

	return &VirtualMachineInstance{
		Namespace: namespace,
		Name:      name,

		VirtualMachine: vm,
	}, nil
}
