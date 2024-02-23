package utils

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"
)

// GetVMBundlePath gets macOS VM bundle path.
func GetVMBundlePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err) //
	}
	return filepath.Join(home, "/VM.bundle/")
}

// GetAuxiliaryStoragePath gets a path for auxiliary storage.
func GetAuxiliaryStoragePath() string {
	return filepath.Join(GetVMBundlePath(), "AuxiliaryStorage")
}

// GetDiskImagePath gets a path for disk image.
func GetDiskImagePath() string {
	return filepath.Join(GetVMBundlePath(), "Disk.img")
}

// GetHardwareModelPath gets a path for hardware model.
func GetHardwareModelPath() string {
	return filepath.Join(GetVMBundlePath(), "HardwareModel")
}

// GetMachineIdentifierPath gets a path for machine identifier.
func GetMachineIdentifierPath() string {
	return filepath.Join(GetVMBundlePath(), "MachineIdentifier")
}

// WIP: temp function for injecting static config
func SetupMacPlatformConfiguration() (*vz.MacPlatformConfiguration, error) {
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(GetAuxiliaryStoragePath())
	if err != nil {
		return nil, fmt.Errorf("failed to create a new mac auxiliary storage: %w", err)
	}
	hardwareModel, err := vz.NewMacHardwareModelWithDataPath(
		GetHardwareModelPath(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new hardware model: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifierWithDataPath(
		GetMachineIdentifierPath(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new machine identifier: %w", err)
	}
	return vz.NewMacPlatformConfiguration(
		vz.WithMacAuxiliaryStorage(auxiliaryStorage),
		vz.WithMacHardwareModel(hardwareModel),
		vz.WithMacMachineIdentifier(machineIdentifier),
	)
}
