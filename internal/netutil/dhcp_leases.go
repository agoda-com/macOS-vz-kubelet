package netutil

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// DHCPLeasesPath is where macOS's DHCP server (bootpd) records the leases it hands out, including those for
// Virtualization.framework VMs on the shared NAT network.
const DHCPLeasesPath = "/var/db/dhcpd_leases"

// LookupDHCPLease returns the IP address leased to macAddr in the leases file at path, and whether one was found.
// bootpd lists the newest lease first, so the first match is the current one.
func LookupDHCPLease(path, macAddr string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	ip, ok := findDHCPLease(f, macAddr)
	return ip, ok, nil
}

// findDHCPLease parses bootpd's leases format:
//
//	{
//		name=...
//		ip_address=192.168.64.4
//		hw_address=1,fa:59:ae:a2:86:ed
//		...
//	}
func findDHCPLease(r io.Reader, macAddr string) (string, bool) {
	want := strings.ToLower(NormalizeMACAddress(macAddr))
	var ip, hw string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "{":
			ip, hw = "", ""
		case strings.HasPrefix(line, "ip_address="):
			ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "hw_address="):
			// "<hardware type>,<address>", type 1 is Ethernet
			if _, addr, found := strings.Cut(strings.TrimPrefix(line, "hw_address="), ","); found {
				hw = strings.ToLower(NormalizeMACAddress(addr))
			}
		case line == "}":
			if ip != "" && hw == want {
				return ip, true
			}
		}
	}
	return "", false
}
