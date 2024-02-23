package client

import (
	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"k8s.io/apimachinery/pkg/types"
)

type VirtualMachineClient interface {
	CreateVirtualMachine(namespace string, name string, cpu uint, memorySize uint64) error
	GetVirtualMachine(namespace string, name string) (*vz.VirtualMachine, error)
	DeleteVirtualMachine(namespace string, name string) error
}

type MacOSClient struct {
	VirtualMachineClient

	instances map[types.NamespacedName]*vz.VirtualMachine
}

func NewMacOSClient() *MacOSClient {
	return &MacOSClient{}
}

func (m *MacOSClient) CreateVirtualMachine(namespace string, name string, cpu uint, memorySize uint64) error {
	platformConfig, err := config.NewPlatformConfiguration(
		"/Users/vgorbachov/Development/macosvm/disk.img",
		"/Users/vgorbachov/Development/macosvm/aux.img",
		[]byte("YnBsaXN0MDDTAQIDBAUGXxAZRGF0YVJlcHJlc2VudGF0aW9uVmVyc2lvbl8QD1BsYXRmb3JtVmVyc2lvbl8QEk1pbmltdW1TdXBwb3J0ZWRPUxQAAAAAAAAAAAAAAAAAAAABEAKjBwgIEA0QAAgPKz1SY2VpawAAAAAAAAEBAAAAAAAAAAkAAAAAAAAAAAAAAAAAAABt"),
		[]byte("YnBsaXN0MDDRAQJURUNJRBMcS7NwwIaUuwgLEAAAAAAAAAEBAAAAAAAAAAMAAAAAAAAAAAAAAAAAAAAZ"),
	)
	if err != nil {
		return err
	}

	config, err := config.NewVirtualMachineConfiguration(platformConfig, cpu, memorySize, "en0", "")
	if err != nil {
		return err
	}

	vm, err := vz.NewVirtualMachine(config.VirtualMachineConfiguration)
	if err != nil {
		return err
	}

	m.instances[types.NamespacedName{Namespace: namespace, Name: name}] = vm

	return vm.Start()
}

func (c *MacOSClient) GetVirtualMachine(namespace string, name string) (*vz.VirtualMachine, error) {
	vm, ok := c.instances[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil, errdefs.NotFound("virtual machine not found")
	}

	return vm, nil
}

func (c *MacOSClient) DeleteVirtualMachine(namespace string, name string) error {
	vm, ok := c.instances[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil
	}

	delete(c.instances, types.NamespacedName{Namespace: namespace, Name: name})

	return vm.Stop()
}
