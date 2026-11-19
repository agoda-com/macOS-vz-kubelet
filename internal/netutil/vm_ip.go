package netutil

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// IP address sources reported by IPResolver.Retrieve.
const (
	SourceARP        = "arp"
	SourceDHCPLeases = "dhcp-leases"
)

// IPResolver finds the IP address of a VM on the shared NAT network from its MAC address. It polls the ARP table
// and, when that has no entry for the address, bootpd's leases file. On macOS 27 `arp -an` can come back empty
// although bootpd has leased the VM an address (see also openai/tart#1359), so the leases file is the fallback.
type IPResolver struct {
	// ARP returns the output of `arp -an`.
	ARP func(ctx context.Context) ([]byte, error)
	// LeasesPath is bootpd's leases file.
	LeasesPath string
	// Interval is the time between attempts.
	Interval time.Duration
}

// DefaultIPResolver returns an IPResolver that uses the system's `arp` and DHCPLeasesPath, once a second.
func DefaultIPResolver() IPResolver {
	return IPResolver{
		ARP: func(ctx context.Context) ([]byte, error) {
			return exec.CommandContext(ctx, "arp", "-an").Output()
		},
		LeasesPath: DHCPLeasesPath,
		Interval:   time.Second,
	}
}

// RetrieveVMIP returns the IP address of the VM with macAddr and its source, using DefaultIPResolver.
func RetrieveVMIP(ctx context.Context, macAddr string) (ip string, source string, err error) {
	return DefaultIPResolver().Retrieve(ctx, macAddr)
}

// Retrieve returns the IP address of the VM with macAddr, and where it was found (SourceARP or SourceDHCPLeases).
// It tries until the context ends. It logs what each source saw while it waits and when it gives up, so a failure
// can be diagnosed from the kubelet's log.
func (r IPResolver) Retrieve(ctx context.Context, macAddr string) (ip string, source string, err error) {
	logger := log.G(ctx).WithField("mac", macAddr)
	start := time.Now()
	want := strings.ToLower(NormalizeMACAddress(macAddr))
	var arpLines, bridgeLines int
	var bridgeSample []string
	var leaseErr error
	for attempt := 1; ; attempt++ {
		out, arpErr := r.ARP(ctx)
		if arpErr == nil {
			arpLines, bridgeLines, bridgeSample = 0, 0, nil
			for _, line := range strings.Split(string(out), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				arpLines++
				if strings.Contains(line, " on bridge") {
					bridgeLines++
					if len(bridgeSample) < 3 {
						bridgeSample = append(bridgeSample, strings.TrimSpace(line))
					}
				}
				if strings.Contains(strings.ToLower(line), want) {
					// "? (192.168.64.2) at 1a:2b:3c:4d:5e:6f on bridge100 ifscope [bridge]"
					if fields := strings.Fields(line); len(fields) > 1 {
						return strings.Trim(fields[1], "()"), SourceARP, nil
					}
				}
			}
		} else if ctx.Err() == nil {
			logger.WithError(arpErr).Warn("arp -an failed")
		}

		leaseIP, ok, err := LookupDHCPLease(r.LeasesPath, macAddr)
		leaseErr = err
		if ok {
			return leaseIP, SourceDHCPLeases, nil
		}

		if attempt%10 == 1 {
			logger.WithField("attempt", attempt).
				WithField("arpLines", arpLines).
				WithField("arpBridgeLines", bridgeLines).
				WithField("leasesError", errString(leaseErr)).
				Info("waiting for the VM's IP address")
		}

		select {
		case <-ctx.Done():
			logger.WithField("seconds", int(time.Since(start).Seconds())).
				WithField("arpLines", arpLines).
				WithField("arpBridgeLines", bridgeLines).
				WithField("arpBridgeSample", strings.Join(bridgeSample, " | ")).
				WithField("leasesError", errString(leaseErr)).
				Warn("VM IP lookup timed out: the address was in neither the ARP table nor the DHCP leases")
			return "", "", ctx.Err()
		case <-time.After(r.Interval):
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
