package provider

import (
	"context"
	"fmt"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	v1 "k8s.io/api/core/v1"
)

func extractCPURequest(ctx context.Context, rl v1.ResourceList) (uint, error) {
	cpuSpec := rl[v1.ResourceCPU]
	cpu, ok := cpuSpec.AsInt64()
	if !ok {
		log.G(ctx).Info("Failed to parse CPU request")
		return 0, fmt.Errorf("failed to parse CPU request")
	}

	return vm.ValidateCPUCount(uint(cpu))
}

func extractMemoryRequest(ctx context.Context, rl v1.ResourceList) (uint64, error) {
	memorySpec := rl[v1.ResourceMemory]
	memory, ok := memorySpec.AsInt64()
	if !ok {
		log.G(ctx).Info("Failed to parse memory request")
		return 0, fmt.Errorf("failed to parse memory request")
	}

	return vm.ValidateMemorySize(uint64(memory))
}
