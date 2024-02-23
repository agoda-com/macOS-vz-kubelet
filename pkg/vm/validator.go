package vm

import (
	"fmt"

	vz "github.com/Code-Hex/vz/v3"
)

func ValidateCPUCount(cpuCount uint) (uint, error) {
	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	if cpuCount > maxAllowed {
		return maxAllowed, fmt.Errorf("cpu count %d is greater than the maximum allowed cpu count %d", cpuCount, maxAllowed)
	}

	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	if cpuCount < minAllowed {
		return minAllowed, fmt.Errorf("cpu count %d is less than the minimum allowed cpu count %d", cpuCount, minAllowed)
	}

	return cpuCount, nil
}

func ValidateMemorySize(memorySize uint64) (uint64, error) {
	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	if memorySize > maxAllowed {
		return maxAllowed, fmt.Errorf("memory size %d is greater than the maximum allowed memory size %d", memorySize, maxAllowed)
	}

	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	if memorySize < minAllowed {
		return minAllowed, fmt.Errorf("memory size %d is less than the minimum allowed memory size %d", memorySize, minAllowed)
	}

	return memorySize, nil
}
