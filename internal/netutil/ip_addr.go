package netutil

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

// CaptureIPWithTcpDump captures the IP address of the device with the specified MAC address using tcpdump.
// It listens on the specified interface and filters packets based on the MAC address.
// The function returns the IP address of the device or an error if the operation fails.
// The function blocks until it captures a packet or the context is canceled.
func CaptureIPWithTcpDump(ctx context.Context, interfaceName, macAddr string) (string, error) {
	handle, err := pcap.OpenLive(interfaceName, 1600, true, pcap.BlockForever)
	if err != nil {
		return "", fmt.Errorf("failed to open device: %w", err)
	}
	defer handle.Close()

	filter := fmt.Sprintf("ether src %s and ip and not src host 0.0.0.0", macAddr)
	if err := handle.SetBPFFilter(filter); err != nil {
		return "", fmt.Errorf("failed to set BPF filter: %w", err)
	}

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	packets := packetSource.Packets()

	for {
		select {
		case packet := <-packets:
			if networkLayer := packet.NetworkLayer(); networkLayer != nil {
				src, _ := networkLayer.NetworkFlow().Endpoints()
				return src.String(), nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// RetrieveIPFromARPTable retrieves the IP address of the device with the specified MAC address from the ARP table.
// The function executes the arp command and scans the output to find the IP address of the device.
// The function returns the IP address of the device or an error if the operation fails.
// The function blocks until it finds the IP address or the context is canceled.
func RetrieveIPFromARPTable(ctx context.Context, macAddr string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			// Execute the arp command
			cmd := exec.CommandContext(ctx, "arp", "-an")
			cmdOutput, err := cmd.Output()
			if err != nil {
				return "", err
			}

			// Scan the command output line by line
			scanner := bufio.NewScanner(strings.NewReader(string(cmdOutput)))
			for scanner.Scan() {
				line := scanner.Text()

				// Check if the line contains the target MAC address
				if strings.Contains(strings.ToLower(line), strings.ToLower(macAddr)) {
					// Example arp output line: "? (192.168.1.2) at 0:1a:2b:3c:4d:5e on en0 ifscope [ethernet]"
					// Split the line into fields and extract the IP address (field 1)
					fields := strings.Fields(line)
					if len(fields) > 1 {
						ipAddress := strings.Trim(fields[1], "()")
						return ipAddress, nil
					}
				}
			}

			// Wait for a second before retrying
			time.Sleep(time.Second)
		}
	}
}
