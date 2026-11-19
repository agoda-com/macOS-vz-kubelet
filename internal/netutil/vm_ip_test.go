package netutil_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
)

const vmMAC = "fa:59:ae:a2:86:ed"

const arpWithVM = `? (172.22.0.1) at ec:c0:18:97:2c:4e on en0 ifscope [ethernet]
? (192.168.64.1) at 1e:1d:d3:ed:4:64 on bridge100 ifscope permanent [bridge]
? (192.168.64.4) at fa:59:ae:a2:86:ed on bridge100 ifscope [bridge]
`

const arpWithoutVM = `? (172.22.0.1) at ec:c0:18:97:2c:4e on en0 ifscope [ethernet]
`

const leaseForVM = `{
	name=ManagedlMachine
	ip_address=192.168.64.7
	hw_address=1,fa:59:ae:a2:86:ed
	lease=0x6aff286f
}
`

func resolver(t *testing.T, arpOut string, arpErr error, leases string) netutil.IPResolver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcpd_leases")
	if leases != "" {
		require.NoError(t, os.WriteFile(path, []byte(leases), 0o644))
	}
	return netutil.IPResolver{
		ARP: func(context.Context) ([]byte, error) {
			return []byte(arpOut), arpErr
		},
		LeasesPath: path,
		Interval:   10 * time.Millisecond,
	}
}

func TestIPResolverRetrieve(t *testing.T) {
	tests := []struct {
		name       string
		arpOut     string
		arpErr     error
		leases     string
		wantIP     string
		wantSource string
	}{
		{name: "ARP table first", arpOut: arpWithVM, leases: leaseForVM, wantIP: "192.168.64.4", wantSource: netutil.SourceARP},
		{name: "empty ARP table falls back to the leases", arpOut: "", leases: leaseForVM, wantIP: "192.168.64.7", wantSource: netutil.SourceDHCPLeases},
		{name: "ARP table without the VM falls back to the leases", arpOut: arpWithoutVM, leases: leaseForVM, wantIP: "192.168.64.7", wantSource: netutil.SourceDHCPLeases},
		{name: "failing arp falls back to the leases", arpErr: errors.New("exit status 1"), leases: leaseForVM, wantIP: "192.168.64.7", wantSource: netutil.SourceDHCPLeases},
		{name: "upper-case MAC in the ARP table", arpOut: "? (192.168.64.9) at FA:59:AE:A2:86:ED on bridge100 ifscope [bridge]\n", wantIP: "192.168.64.9", wantSource: netutil.SourceARP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			ip, source, err := resolver(t, tt.arpOut, tt.arpErr, tt.leases).Retrieve(ctx, vmMAC)
			require.NoError(t, err)
			assert.Equal(t, tt.wantIP, ip)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

func TestIPResolverRetrieveTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := resolver(t, arpWithoutVM, nil, "").Retrieve(ctx, vmMAC)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestIPResolverRetrieveKeepsPolling(t *testing.T) {
	// The VM's lease appears only after a few attempts, as when the guest is still booting.
	path := filepath.Join(t.TempDir(), "dhcpd_leases")
	var calls atomic.Int32
	r := netutil.IPResolver{
		ARP: func(context.Context) ([]byte, error) {
			if calls.Add(1) == 3 {
				require.NoError(t, os.WriteFile(path, []byte(leaseForVM), 0o644))
			}
			return nil, nil
		},
		LeasesPath: path,
		Interval:   10 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ip, source, err := r.Retrieve(ctx, vmMAC)
	require.NoError(t, err)
	assert.Equal(t, "192.168.64.7", ip)
	assert.Equal(t, netutil.SourceDHCPLeases, source)
	assert.GreaterOrEqual(t, calls.Load(), int32(3))
}
