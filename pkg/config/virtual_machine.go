package config

import (
	"crypto/rand"
	"fmt"
	"net"
	"time"

	"github.com/Code-Hex/vz/v3"
)

type VirtualMachineConfiguration struct {
	MACAddress net.HardwareAddr

	*vz.VirtualMachineConfiguration
}

func NewVirtualMachineConfiguration(platformConfig *PlatformConfiguration, cpuCount uint, memorySize uint64, networkInterfaceIdentifier string) (*VirtualMachineConfiguration, error) {
	bootloader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("failed to create a new macos bootloader: %w", err)
	}

	config, err := vz.NewVirtualMachineConfiguration(
		bootloader,
		cpuCount,
		memorySize,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new virtual machine configuration: %w", err)
	}

	// Set the platform configuration
	config.SetPlatformVirtualMachineConfiguration(platformConfig)

	// Create a graphics device configuration
	graphicsDeviceConfig, err := createGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create graphics device configuration: %w", err)
	}
	config.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{
		graphicsDeviceConfig,
	})

	// Attach the disk image to the virtual machine
	diskImageAttachment, err := vz.NewDiskImageStorageDeviceAttachment(
		platformConfig.BlockStoragePath,
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create disk image storage device attachment: %w", err)
	}
	blockDeviceConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskImageAttachment)
	if err != nil {
		return nil, fmt.Errorf("failed to create block device configuration: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDeviceConfig})

	// Create a network device configuration
	networkDeviceConfig, err := createNetworkDeviceConfiguration(networkInterfaceIdentifier)
	if err != nil {
		return nil, fmt.Errorf("failed to create network device configuration: %w", err)
	}
	// Set the MAC address
	macStr, err := generateRandomMAC()
	if err != nil {
		return nil, fmt.Errorf("failed to generate random mac address: %w", err)
	}
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mac address: %w", err)
	}
	macAddr, err := vz.NewMACAddress(mac)
	if err != nil {
		return nil, fmt.Errorf("failed to create mac address: %w", err)
	}
	networkDeviceConfig.SetMACAddress(macAddr)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkDeviceConfig,
	})

	// Create a pointing device configuration
	usbScreenPointingDevice, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create pointing device configuration: %w", err)
	}
	pointingDevices := []vz.PointingDeviceConfiguration{usbScreenPointingDevice}
	trackpad, err := vz.NewMacTrackpadConfiguration()
	if err == nil {
		pointingDevices = append(pointingDevices, trackpad)
	}
	config.SetPointingDevicesVirtualMachineConfiguration(pointingDevices)

	// Create keyboard device configuration
	keyboardDeviceConfig, err := vz.NewUSBKeyboardConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create keyboard device configuration: %w", err)
	}
	config.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{
		keyboardDeviceConfig,
	})

	// Create audio device configuration
	audioDeviceConfig, err := createAudioDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create audio device configuration: %w", err)
	}
	config.SetAudioDevicesVirtualMachineConfiguration([]vz.AudioDeviceConfiguration{
		audioDeviceConfig,
	})

	// Validate the configuration
	validated, err := config.Validate()
	if err != nil {
		return nil, fmt.Errorf("failed to validate configuration: %w", err)
	}
	if !validated {
		return nil, fmt.Errorf("invalid configuration")
	}

	return &VirtualMachineConfiguration{
		MACAddress:                  mac,
		VirtualMachineConfiguration: config,
	}, nil
}

func createGraphicsDeviceConfiguration() (*vz.MacGraphicsDeviceConfiguration, error) {
	graphicDeviceConfig, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	graphicsDisplayConfig, err := vz.NewMacGraphicsDisplayConfiguration(1920, 1200, 80)
	if err != nil {
		return nil, err
	}
	graphicDeviceConfig.SetDisplays(
		graphicsDisplayConfig,
	)
	return graphicDeviceConfig, nil
}

func createNetworkDeviceConfiguration(networkInterfaceIdentifier string) (*vz.VirtioNetworkDeviceConfiguration, error) {
	var attachment vz.NetworkDeviceAttachment
	var err error
	if networkInterfaceIdentifier != "" {
		var networkInterface vz.BridgedNetwork
		networkInterfaces := vz.NetworkInterfaces()
		for _, b := range networkInterfaces {
			if b.Identifier() == networkInterfaceIdentifier {
				networkInterface = b
				break
			}
		}
		if networkInterface == nil {
			return nil, fmt.Errorf("network interface %s not found", networkInterfaceIdentifier)
		}

		attachment, err = vz.NewBridgedNetworkDeviceAttachment(networkInterface)
		if err != nil {
			return nil, err
		}
	} else {
		attachment, err = vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, err
		}
	}

	return vz.NewVirtioNetworkDeviceConfiguration(attachment)
}

func createAudioDeviceConfiguration() (*vz.VirtioSoundDeviceConfiguration, error) {
	audioConfig, err := vz.NewVirtioSoundDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create sound device configuration: %w", err)
	}
	inputStream, err := vz.NewVirtioSoundDeviceHostInputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create input stream configuration: %w", err)
	}
	outputStream, err := vz.NewVirtioSoundDeviceHostOutputStreamConfiguration()
	if err != nil {
		return nil, fmt.Errorf("failed to create output stream configuration: %w", err)
	}
	audioConfig.SetStreams(
		inputStream,
		outputStream,
	)
	return audioConfig, nil
}

// generateRandomMAC generates a random MAC address.
// It incorporates part of the current Unix timestamp to reduce the likelihood of duplicates.
func generateRandomMAC() (string, error) {
	buf := make([]byte, 6)
	_, err := rand.Read(buf[2:])
	if err != nil {
		return "", err
	}

	// Incorporate the current Unix time into the first 2 bytes
	now := uint32(time.Now().Unix())
	buf[0] = byte(now>>24) & 0xFE // Ensure unicast and locally administered
	buf[1] = byte(now >> 16)

	mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
	return mac, nil
}
