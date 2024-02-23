package config

import (
	"encoding/base64"
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

type PlatformConfiguration struct {
	BlockStoragePath string

	*vz.MacPlatformConfiguration
}

func NewPlatformConfiguration(blockStoragePath string, auxiliaryStoragePath string, hardwareModelData string, machineIdentifierData string) (*PlatformConfiguration, error) {
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(auxiliaryStoragePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new mac auxiliary storage: %w", err)
	}

	decodedHardwareModelData, err := base64.StdEncoding.DecodeString(hardwareModelData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 string: %w", err)
	}
	hardwareModel, err := vz.NewMacHardwareModelWithData(decodedHardwareModelData)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new hardware model: %w", err)
	}

	decodedMachineIdentifierData, err := base64.StdEncoding.DecodeString(machineIdentifierData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 string: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifierWithData(decodedMachineIdentifierData)
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
