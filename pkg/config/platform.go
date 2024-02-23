package config

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

type PlatformConfiguration struct {
	BlockStoragePath string

	*vz.MacPlatformConfiguration
}

func NewPlatformConfiguration(blockStoragePath string, auxiliaryStoragePath string, hardwareModelData []byte, machineIdentifierBytes []byte) (*PlatformConfiguration, error) {
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(auxiliaryStoragePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new mac auxiliary storage: %w", err)
	}
	hardwareModel, err := vz.NewMacHardwareModelWithData(hardwareModelData)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new hardware model: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifierWithData(machineIdentifierBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new machine identifier: %w", err)
	}

	c, err := vz.NewMacPlatformConfiguration(
		vz.WithMacAuxiliaryStorage(auxiliaryStorage),
		vz.WithMacHardwareModel(hardwareModel),
		vz.WithMacMachineIdentifier(machineIdentifier),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new mac platform configuration: %w", err)
	}

	return &PlatformConfiguration{
		BlockStoragePath:         blockStoragePath,
		MacPlatformConfiguration: c,
	}, nil
}
