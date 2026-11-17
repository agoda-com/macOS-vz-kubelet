package metrics

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
	"github.com/virtual-kubelet/virtual-kubelet/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// hostSample is one reading of the host's resource usage. Each part carries its own
// error so a failure in one source (e.g. network counters) does not hide the others.
type hostSample struct {
	time time.Time

	bootTime    time.Time
	bootTimeErr error

	// cpuBusy is the cumulative busy time (user+system+nice) summed over all cores.
	cpuBusy    time.Duration
	cpuBusyErr error

	memory    *mem.VirtualMemoryStat
	memoryErr error

	fs    *disk.UsageStat
	fsErr error

	network    []psnet.IOCountersStat
	networkErr error
}

// hostStatsSource reads the host's resource usage. It is an interface so tests can
// supply fixed samples.
type hostStatsSource interface {
	sample(ctx context.Context) hostSample
}

// gopsutilHost reads host usage with gopsutil.
type gopsutilHost struct {
	// fsPath is the filesystem reported as the node filesystem.
	fsPath string
}

func (h gopsutilHost) sample(ctx context.Context) hostSample {
	s := hostSample{time: time.Now()}

	var boot uint64
	if boot, s.bootTimeErr = host.BootTimeWithContext(ctx); s.bootTimeErr == nil {
		s.bootTime = time.Unix(int64(boot), 0) //nolint:gosec // boot time fits in int64
	}

	// Sum the per-core counters rather than reading the host-wide one: on macOS both
	// are 32-bit tick counters, and the host-wide sum wraps far sooner on many-core
	// hosts (about two weeks of full load on 32 cores, versus over a year per core).
	var perCore []cpu.TimesStat
	if perCore, s.cpuBusyErr = cpu.TimesWithContext(ctx, true); s.cpuBusyErr == nil {
		var busy float64
		for _, c := range perCore {
			busy += c.User + c.System + c.Nice
		}
		s.cpuBusy = time.Duration(busy * float64(time.Second))
	}

	s.memory, s.memoryErr = mem.VirtualMemoryWithContext(ctx)
	s.fs, s.fsErr = disk.UsageWithContext(ctx, h.fsPath)
	s.network, s.networkErr = psnet.IOCountersWithContext(ctx, true)

	return s
}

// cpuReading is the previous cumulative CPU reading, kept between scrapes to derive
// the current usage rate.
type cpuReading struct {
	busy time.Duration
	time time.Time
}

// nodeStats builds the node part of the stats summary from a host sample. It updates
// p.prevCPU, so callers must hold p.metricsSync.
func (p *MacOSVZPodMetricsProvider) nodeStats(ctx context.Context) stats.NodeStats {
	n := stats.NodeStats{NodeName: p.nodeName}
	if p.host == nil {
		return n
	}

	s := p.host.sample(ctx)
	now := metav1.NewTime(s.time)

	if s.bootTimeErr == nil {
		n.StartTime = metav1.NewTime(s.bootTime)
	} else {
		log.G(ctx).WithError(s.bootTimeErr).Debug("failed to read host boot time")
	}

	if s.cpuBusyErr == nil {
		n.CPU = p.nodeCPUStats(s, now)
	} else {
		log.G(ctx).WithError(s.cpuBusyErr).Debug("failed to read host cpu times")
	}

	if s.memoryErr == nil && s.memory != nil {
		n.Memory = nodeMemoryStats(s.memory, now)
	} else {
		log.G(ctx).WithError(s.memoryErr).Debug("failed to read host memory")
	}

	if s.fsErr == nil && s.fs != nil {
		n.Fs = nodeFsStats(s.fs, now)
	} else {
		log.G(ctx).WithError(s.fsErr).Debug("failed to read host filesystem usage")
	}

	if s.networkErr == nil {
		n.Network = nodeNetworkStats(s.network, now)
	} else {
		log.G(ctx).WithError(s.networkErr).Debug("failed to read host network counters")
	}

	return n
}

// nodeCPUStats reports the cumulative busy time and, from the second scrape on, the
// average usage since the previous scrape.
func (p *MacOSVZPodMetricsProvider) nodeCPUStats(s hostSample, now metav1.Time) *stats.CPUStats {
	busyNanos := uint64(s.cpuBusy.Nanoseconds()) //nolint:gosec // busy time is never negative
	cs := &stats.CPUStats{Time: now, UsageCoreNanoSeconds: &busyNanos}

	prev := p.prevCPU
	p.prevCPU = &cpuReading{busy: s.cpuBusy, time: s.time}

	if prev == nil {
		return cs
	}
	elapsed := s.time.Sub(prev.time)
	delta := s.cpuBusy - prev.busy
	// A counter that went backwards (a wrapped or reset counter) gives no usable rate.
	if elapsed <= 0 || delta < 0 {
		return cs
	}
	nanoCores := uint64(float64(delta) / elapsed.Seconds()) //nolint:gosec // non-negative
	cs.UsageNanoCores = &nanoCores
	return cs
}

// nodeMemoryStats maps macOS memory onto kubelet semantics, matching the per-VM stats:
// usage counts everything not free, and the working set leaves out the reclaimable
// inactive pages (gopsutil's Used is total - free - inactive).
func nodeMemoryStats(m *mem.VirtualMemoryStat, now metav1.Time) *stats.MemoryStats {
	available := m.Available
	workingSet := m.Used
	usage := m.Used
	if m.Total >= m.Free {
		usage = m.Total - m.Free
	}
	return &stats.MemoryStats{
		Time:            now,
		AvailableBytes:  &available,
		UsageBytes:      &usage,
		WorkingSetBytes: &workingSet,
	}
}

// nodeFsStats reports the node filesystem. On APFS the root volume is the small sealed
// system volume, so its own "used" figure misses the data volume; capacity minus
// available covers every volume in the container.
func nodeFsStats(u *disk.UsageStat, now metav1.Time) *stats.FsStats {
	capacity := u.Total
	available := u.Free
	var used uint64
	if capacity >= available {
		used = capacity - available
	}
	fs := &stats.FsStats{
		Time:           now,
		CapacityBytes:  &capacity,
		AvailableBytes: &available,
		UsedBytes:      &used,
	}
	if u.InodesTotal > 0 {
		inodes, free, usedInodes := u.InodesTotal, u.InodesFree, u.InodesUsed
		fs.Inodes, fs.InodesFree, fs.InodesUsed = &inodes, &free, &usedInodes
	}
	return fs
}

// virtualInterfacePrefixes are host interfaces that are not physical uplinks: loopback,
// the VM bridges and their members, tunnels and Apple wireless/peer-to-peer links.
var virtualInterfacePrefixes = []string{
	"lo", "bridge", "vmenet", "utun", "gif", "stf", "awdl", "llw", "anpi", "ap", "feth",
}

// nodeNetworkStats reports the host's physical interfaces. The default interface is
// the one that has moved the most bytes, which on a build host is its uplink.
func nodeNetworkStats(counters []psnet.IOCountersStat, now metav1.Time) *stats.NetworkStats {
	ns := &stats.NetworkStats{Time: now}

	var busiest uint64
	for _, c := range counters {
		if isVirtualInterface(c.Name) {
			continue
		}
		rxBytes, rxErrors, txBytes, txErrors := c.BytesRecv, c.Errin, c.BytesSent, c.Errout
		iface := stats.InterfaceStats{
			Name:     c.Name,
			RxBytes:  &rxBytes,
			RxErrors: &rxErrors,
			TxBytes:  &txBytes,
			TxErrors: &txErrors,
		}
		ns.Interfaces = append(ns.Interfaces, iface)
		if total := rxBytes + txBytes; ns.Name == "" || total > busiest {
			busiest = total
			ns.InterfaceStats = iface
		}
	}
	sort.Slice(ns.Interfaces, func(i, j int) bool { return ns.Interfaces[i].Name < ns.Interfaces[j].Name })

	return ns
}

func isVirtualInterface(name string) bool {
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
