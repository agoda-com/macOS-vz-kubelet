package netutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
)

const leases = `{
	name=ManagedlMachine
	ip_address=192.168.64.4
	hw_address=1,fa:59:ae:a2:86:ed
	identifier=1,fa:59:ae:a2:86:ed
	lease=0x6aff286f
}
{
	name=ManagedlMachine
	ip_address=192.168.65.2
	hw_address=1,2:a7:b6:fb:ad:47
	identifier=1,2:a7:b6:fb:ad:47
	lease=0x6958311f
}
{
	name=ManagedlMachine
	ip_address=192.168.64.9
	hw_address=1,fa:59:ae:a2:86:ed
	lease=0x6958311f
}
{
	hw_address=1,aa:bb:cc:dd:ee:1
}
`

func writeLeases(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcpd_leases")
	require.NoError(t, os.WriteFile(path, []byte(leases), 0o644))
	return path
}

func TestLookupDHCPLease(t *testing.T) {
	path := writeLeases(t)
	tests := []struct {
		name   string
		mac    string
		wantIP string
		wantOK bool
	}{
		{name: "first (newest) match wins", mac: "fa:59:ae:a2:86:ed", wantIP: "192.168.64.4", wantOK: true},
		{name: "case and leading zeros do not matter", mac: "02:A7:B6:FB:AD:47", wantIP: "192.168.65.2", wantOK: true},
		{name: "entry without an IP is ignored", mac: "aa:bb:cc:dd:ee:01"},
		{name: "unknown address", mac: "aa:bb:cc:dd:ee:ff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, ok, err := netutil.LookupDHCPLease(path, tt.mac)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantIP, ip)
		})
	}
}

func TestLookupDHCPLeaseMissingFile(t *testing.T) {
	_, ok, err := netutil.LookupDHCPLease(filepath.Join(t.TempDir(), "missing"), "fa:59:ae:a2:86:ed")
	assert.Error(t, err)
	assert.False(t, ok)
}
