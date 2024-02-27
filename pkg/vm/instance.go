package vm

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/config"
)

type VirtualMachineInstance struct {
	Namespace string
	Name      string
	IPAddress string

	CreationTime time.Time
	StartTime    time.Time

	macAddr string

	*vz.VirtualMachine
}

func NewVirtualMachineInstance(namespace, name string, config *config.VirtualMachineConfiguration) (*VirtualMachineInstance, error) {
	vm, err := vz.NewVirtualMachine(config.VirtualMachineConfiguration)
	if err != nil {
		return nil, err
	}

	vmi := &VirtualMachineInstance{
		Namespace: namespace,
		Name:      name,

		CreationTime: time.Now(),

		macAddr: normalizeMACAddress(config.MACAddress.String()),

		VirtualMachine: vm,
	}

	return vmi, nil
}

func (vmi *VirtualMachineInstance) Start(opts ...vz.VirtualMachineStartOption) error {
	// go routine to retrieve ip address until successfull
	go func() {
		for {
			ipAddress, err := vmi.retrieveIPAddress()
			if err == nil {
				vmi.IPAddress = ipAddress
				break
			}
		}
	}()
	// go routine to assign start time when VM state switched to Running using StateChangedNotify function
	go func() {
		for state := range vmi.StateChangedNotify() {
			if state == vz.VirtualMachineStateRunning {
				vmi.StartTime = time.Now()
				break
			}
		}
	}()

	return vmi.VirtualMachine.Start(opts...)
}

func (vmi *VirtualMachineInstance) retrieveIPAddress() (string, error) {
	// Execute the arp command
	cmd := exec.Command("arp", "-an")
	cmdOutput, err := cmd.Output()
	if err != nil {
		return "", err
	}

	// Scan the command output line by line
	scanner := bufio.NewScanner(strings.NewReader(string(cmdOutput)))
	for scanner.Scan() {
		line := scanner.Text()

		// Check if the line contains the target MAC address
		if strings.Contains(strings.ToLower(line), strings.ToLower(vmi.macAddr)) {
			// Example arp output line: "? (192.168.1.2) at 0:1a:2b:3c:4d:5e on en0 ifscope [ethernet]"
			// Split the line into fields and extract the IP address (field 1)
			fields := strings.Fields(line)
			if len(fields) > 1 {
				ipAddress := strings.Trim(fields[1], "()")
				return ipAddress, nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("MAC address %s not found", vmi.macAddr)
}

// normalizeMACAddress normalizes MAC addresses by ensuring all hex digits are in lowercase
// and leading zeros in each octet are removed.
func normalizeMACAddress(mac string) string {
	parts := strings.Split(mac, ":")
	for i, part := range parts {
		if len(part) == 2 && part[0] == '0' {
			parts[i] = part[1:]
		}
	}
	return strings.Join(parts, ":")
}
