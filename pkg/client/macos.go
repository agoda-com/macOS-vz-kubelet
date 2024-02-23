package client

import (
	"context"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/config"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/types"
)

type VirtualMachineClient interface {
	CreateVirtualMachine(ctx context.Context, namespace string, name string, cpu uint, memorySize uint64) error
	GetVirtualMachine(ctx context.Context, namespace string, name string) (*vm.VirtualMachineInstance, error)
	GetVirtualMachineListResult(ctx context.Context) ([]*vm.VirtualMachineInstance, error)
	DeleteVirtualMachine(ctx context.Context, namespace string, name string) error
}

type MacOSClient struct {
	VirtualMachineClient

	instances map[types.NamespacedName]*vm.VirtualMachineInstance
}

func NewMacOSClient() *MacOSClient {
	return &MacOSClient{
		instances: make(map[types.NamespacedName]*vm.VirtualMachineInstance),
	}
}

func (m *MacOSClient) CreateVirtualMachine(ctx context.Context, namespace string, name string, cpu uint, memorySize uint64) error {
	platformConfig, err := config.NewPlatformConfiguration(
		"/Users/vgorbachov/Development/macosvm/disk.img",
		"/Users/vgorbachov/Development/macosvm/aux.img",
		"YnBsaXN0MDDTAQIDBAUGXxAZRGF0YVJlcHJlc2VudGF0aW9uVmVyc2lvbl8QD1BsYXRmb3JtVmVyc2lvbl8QEk1pbmltdW1TdXBwb3J0ZWRPUxQAAAAAAAAAAAAAAAAAAAABEAKjBwgIEA0QAAgPKz1SY2VpawAAAAAAAAEBAAAAAAAAAAkAAAAAAAAAAAAAAAAAAABt",
		"YnBsaXN0MDDRAQJURUNJRBMcS7NwwIaUuwgLEAAAAAAAAAEBAAAAAAAAAAMAAAAAAAAAAAAAAAAAAAAZ",
	)
	if err != nil {
		return err
	}

	config, err := config.NewVirtualMachineConfiguration(platformConfig, cpu, memorySize, "en0", "aa:bb:cc:dd:ee:ff")
	if err != nil {
		return err
	}

	vm, err := vm.NewVirtualMachineInstance(namespace, name, config)
	if err != nil {
		return err
	}

	m.instances[types.NamespacedName{Namespace: namespace, Name: name}] = vm

	return vm.Start()
}

func (c *MacOSClient) GetVirtualMachine(ctx context.Context, namespace string, name string) (*vm.VirtualMachineInstance, error) {
	vm, ok := c.instances[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil, errdefs.NotFound("virtual machine not found")
	}

	return vm, nil
}

func (c *MacOSClient) GetVirtualMachineListResult(ctx context.Context) ([]*vm.VirtualMachineInstance, error) {
	return maps.Values(c.instances), nil
}

func (c *MacOSClient) DeleteVirtualMachine(ctx context.Context, namespace string, name string) error {
	vm, ok := c.instances[types.NamespacedName{Namespace: namespace, Name: name}]
	if !ok {
		return nil
	}

	delete(c.instances, types.NamespacedName{Namespace: namespace, Name: name})

	return vm.Stop()
}
