package netutil

import (
	"context"
	"fmt"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/pcap"
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
