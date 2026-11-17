package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client/mocks"
)

// fakeHost returns the queued samples in order, repeating the last one.
type fakeHost struct {
	samples []hostSample
	calls   int
}

func (f *fakeHost) sample(context.Context) hostSample {
	i := min(f.calls, len(f.samples)-1)
	f.calls++
	return f.samples[i]
}

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func fullSample(at time.Time, busy time.Duration) hostSample {
	return hostSample{
		time:     at,
		bootTime: t0.Add(-24 * time.Hour),
		cpuBusy:  busy,
		memory: &mem.VirtualMemoryStat{
			Total:     96 << 30,
			Free:      10 << 30,
			Available: 30 << 30, // free + inactive
			Used:      66 << 30, // total - available
		},
		fs: &disk.UsageStat{
			Total:       2000 << 30,
			Free:        1500 << 30,
			Used:        12 << 30, // the sealed system volume only
			InodesTotal: 1000,
			InodesFree:  400,
			InodesUsed:  600,
		},
		network: []psnet.IOCountersStat{
			{Name: "lo0", BytesRecv: 9e12, BytesSent: 9e12},
			{Name: "bridge100", BytesRecv: 8e12, BytesSent: 8e12},
			{Name: "vmenet0", BytesRecv: 7e12, BytesSent: 7e12},
			{Name: "en1", BytesRecv: 10, BytesSent: 20, Errin: 1, Errout: 2},
			{Name: "en0", BytesRecv: 5000, BytesSent: 6000, Errin: 3, Errout: 4},
		},
	}
}

func newTestProvider(t *testing.T, host hostStatsSource) *MacOSVZPodMetricsProvider {
	t.Helper()
	p := NewMacOSVZPodMetricsProvider("test-node", internalListerWith(t), mocks.NewVzClientInterface(t))
	p.host = host
	return p
}

func TestNodeStatsFromHostSample(t *testing.T) {
	p := newTestProvider(t, &fakeHost{samples: []hostSample{fullSample(t0, 100*time.Second)}})

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	n := summary.Node

	assert.Equal(t, "test-node", n.NodeName)
	assert.Equal(t, t0.Add(-24*time.Hour), n.StartTime.Time)

	require.NotNil(t, n.CPU)
	assert.Equal(t, uint64(100*time.Second), *n.CPU.UsageCoreNanoSeconds)
	assert.Nil(t, n.CPU.UsageNanoCores, "no rate before a second reading")

	require.NotNil(t, n.Memory)
	assert.Equal(t, uint64(30<<30), *n.Memory.AvailableBytes)
	assert.Equal(t, uint64(86<<30), *n.Memory.UsageBytes, "usage is total - free")
	assert.Equal(t, uint64(66<<30), *n.Memory.WorkingSetBytes, "working set leaves out inactive pages")

	require.NotNil(t, n.Fs)
	assert.Equal(t, uint64(2000<<30), *n.Fs.CapacityBytes)
	assert.Equal(t, uint64(1500<<30), *n.Fs.AvailableBytes)
	assert.Equal(t, uint64(500<<30), *n.Fs.UsedBytes, "used covers the whole APFS container")
	assert.Equal(t, uint64(1000), *n.Fs.Inodes)
	assert.Equal(t, uint64(400), *n.Fs.InodesFree)
	assert.Equal(t, uint64(600), *n.Fs.InodesUsed)

	require.NotNil(t, n.Network)
	assert.Equal(t, "en0", n.Network.Name, "default is the busiest physical interface")
	assert.Equal(t, uint64(5000), *n.Network.RxBytes)
	assert.Equal(t, uint64(6000), *n.Network.TxBytes)
	assert.Equal(t, uint64(3), *n.Network.RxErrors)
	assert.Equal(t, uint64(4), *n.Network.TxErrors)
	names := make([]string, 0, len(n.Network.Interfaces))
	for _, i := range n.Network.Interfaces {
		names = append(names, i.Name)
	}
	assert.Equal(t, []string{"en0", "en1"}, names, "loopback, bridges and VM interfaces are left out")
}

func TestNodeCPURateFromConsecutiveScrapes(t *testing.T) {
	host := &fakeHost{samples: []hostSample{
		fullSample(t0, 100*time.Second),
		// 30 core-seconds over 15 seconds: 2 cores busy on average.
		fullSample(t0.Add(15*time.Second), 130*time.Second),
	}}
	p := newTestProvider(t, host)

	_, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.NotNil(t, summary.Node.CPU.UsageNanoCores)
	assert.Equal(t, uint64(2_000_000_000), *summary.Node.CPU.UsageNanoCores)
	assert.Equal(t, uint64(130*time.Second), *summary.Node.CPU.UsageCoreNanoSeconds)
}

func TestNodeCPURateSkippedWhenCounterGoesBackwards(t *testing.T) {
	host := &fakeHost{samples: []hostSample{
		fullSample(t0, 100*time.Second),
		fullSample(t0.Add(15*time.Second), 40*time.Second),
		fullSample(t0.Add(30*time.Second), 70*time.Second),
	}}
	p := newTestProvider(t, host)

	_, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.Nil(t, summary.Node.CPU.UsageNanoCores, "a counter that went backwards gives no rate")

	summary, err = p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.NotNil(t, summary.Node.CPU.UsageNanoCores, "the rate recovers from the next reading")
	assert.Equal(t, uint64(2_000_000_000), *summary.Node.CPU.UsageNanoCores)
}

func TestNodeStatsLeavesOutFailedSources(t *testing.T) {
	s := fullSample(t0, 100*time.Second)
	s.cpuBusyErr = errors.New("cpu")
	s.memoryErr = errors.New("memory")
	s.fsErr = errors.New("fs")
	s.networkErr = errors.New("network")
	s.bootTimeErr = errors.New("boot")
	p := newTestProvider(t, &fakeHost{samples: []hostSample{s}})

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err, "host stats failures never fail the summary")
	n := summary.Node
	assert.Equal(t, "test-node", n.NodeName)
	assert.True(t, n.StartTime.IsZero())
	assert.Nil(t, n.CPU)
	assert.Nil(t, n.Memory)
	assert.Nil(t, n.Fs)
	assert.Nil(t, n.Network)
}

func TestNodeStatsWithoutHostSource(t *testing.T) {
	p := newTestProvider(t, nil)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "test-node", summary.Node.NodeName)
	assert.Nil(t, summary.Node.CPU)
}

// The real gopsutil source must produce sane readings on the host running the tests.
func TestGopsutilHostSample(t *testing.T) {
	s := gopsutilHost{fsPath: "/"}.sample(context.Background())

	require.NoError(t, s.bootTimeErr)
	assert.True(t, s.bootTime.Before(time.Now()))

	require.NoError(t, s.cpuBusyErr)
	assert.Positive(t, s.cpuBusy)

	require.NoError(t, s.memoryErr)
	require.NotNil(t, s.memory)
	assert.Positive(t, s.memory.Total)
	assert.LessOrEqual(t, s.memory.Available, s.memory.Total)

	require.NoError(t, s.fsErr)
	require.NotNil(t, s.fs)
	assert.Positive(t, s.fs.Total)

	require.NoError(t, s.networkErr)
	assert.NotEmpty(t, s.network)
}
