package resourcemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	vzio "github.com/agoda-com/macOS-vz-kubelet/internal/io"
	"github.com/agoda-com/macOS-vz-kubelet/internal/node"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// buildVMStatsCommand returns the macOS guest stats script as a Mode-B command
// (a list of separate shell statements written to the guest shell stdin, NOT an
// ["sh","-c",...] exec): the embedded awk/perl/JXA single quotes only survive
// Mode B, because the sh -c path ($'...') in internal/utils.BuildExecCommandString
// does not escape them.
//
// macOS exposes NO cumulative CPU counter via sysctl/CLI (kern.cp_time is FreeBSD-only).
// HOST_CPU_LOAD_INFO - the tick counter top/Activity Monitor read - is reachable only via
// a Mach call, here through stock osascript + ObjC.bindFunction (no Xcode/tooling, works on
// every macOS). Ticks are aggregate across all cores, monotonic since boot (include exited
// procs), unit getconf CLK_TCK; busy = user+system+nice. Two reads ~1s apart: the second is
// the cumulative counter (cpuUsageCoreNanoSeconds), the delta gives the instantaneous
// cpuUsageNanoCores (ticks are uint32 so a wrap makes the delta negative - clamp to 0).
// Page size derived at runtime (Apple
// Silicon = 16384, not 4096); vm_stat run ONCE. Memory: Usage = active+inactive+wired+
// compressor (matches top "used"); WorkingSet excludes reclaimable inactive; both x pageSize.
func buildVMStatsCommand() []string {
	return []string{
		`JXA='ObjC.import("Foundation"); ObjC.bindFunction("mach_host_self",["unsigned int",[]]); ObjC.bindFunction("host_statistics",["int",["unsigned int","int","void *","void *"]]); var info=$.NSMutableData.dataWithLength(16); var cnt=$.NSMutableData.alloc.initWithBase64EncodedStringOptions($("BAAAAA=="),0); $.host_statistics($.mach_host_self(),3,info.mutableBytes,cnt.mutableBytes); ObjC.unwrap(info.base64EncodedStringWithOptions(0));'`,
		`clkTck=$(getconf CLK_TCK)`,
		`b1=$(osascript -l JavaScript -e "$JXA"); t1=$(perl -MTime::HiRes=time -e 'printf "%.6f", time')`,
		`sleep 1`,
		`b2=$(osascript -l JavaScript -e "$JXA"); t2=$(perl -MTime::HiRes=time -e 'printf "%.6f", time')`,
		`set -- $(echo "$b1" | base64 -d | od -An -tu4); u1=$1; s1=$2; n1=$4`,
		`set -- $(echo "$b2" | base64 -d | od -An -tu4); u2=$1; s2=$2; n2=$4`,
		`busy1=$((u1+s1+n1)); busy2=$((u2+s2+n2))`,
		`cpuUsageCoreNanoSeconds=$(( busy2 * 1000000000 / clkTck ))`,
		`cpuUsageNanoCores=$(awk -v db=$((busy2-busy1)) -v c=$clkTck -v t1=$t1 -v t2=$t2 'BEGIN{dt=t2-t1; if(dt<=0)dt=1; if(db<0)db=0; printf "%.0f", db*1000000000/c/dt}')`,
		`vmStat=$(vm_stat)`,
		`pageSize=$(sysctl -n vm.pagesize 2>/dev/null || echo "$vmStat" | awk '/page size of/ {print $8}')`,
		`active=$(echo "$vmStat" | awk '/Pages active/ {gsub(/\./,"",$3); print $3}')`,
		`inactive=$(echo "$vmStat" | awk '/Pages inactive/ {gsub(/\./,"",$3); print $3}')`,
		`wired=$(echo "$vmStat" | awk '/Pages wired down/ {gsub(/\./,"",$4); print $4}')`,
		`compressor=$(echo "$vmStat" | awk '/Pages occupied by compressor/ {gsub(/\./,"",$5); print $5}')`,
		`memoryUsageBytes=$(( (active + inactive + wired + compressor) * pageSize ))`,
		`memoryWorkingSetBytes=$(( (active + wired + compressor) * pageSize ))`,
		`memoryRssBytes=$(( active * pageSize ))`,
		`echo "{\"cpuUsageNanoCores\": $cpuUsageNanoCores, \"cpuUsageCoreNanoSeconds\": $cpuUsageCoreNanoSeconds, \"memoryUsageBytes\": $memoryUsageBytes, \"memoryRssBytes\": $memoryRssBytes, \"memoryWorkingSetBytes\": $memoryWorkingSetBytes}"`,
	}
}

// GetVirtualMachineStats retrieves the stats of the specified virtual machine.
func (c *MacOSClient) GetVirtualMachineStats(ctx context.Context, namespace, name string) (stats.ContainerStats, error) {
	cmd := buildVMStatsCommand()

	stdout, stderr, attach := newStatsExecIO()

	if err := c.ExecInVirtualMachine(ctx, namespace, name, cmd, attach); err != nil {
		return stats.ContainerStats{}, fmt.Errorf("error executing script: %w", err)
	}

	statsData, err := parseStatsJSON(stdout.Bytes())
	if err != nil {
		return stats.ContainerStats{}, fmt.Errorf("error parsing JSON output (stderr=%q): %w", truncateForError(stderr.String(), maxStderrInError), err)
	}

	time := metav1.NewTime(time.Now())
	return stats.ContainerStats{
		CPU: &stats.CPUStats{
			Time:                 time,
			UsageNanoCores:       statsData.CPUUsageNanoCores,
			UsageCoreNanoSeconds: statsData.CPUUsageCoreNanoSeconds,
		},
		Memory: &stats.MemoryStats{
			Time:            time,
			UsageBytes:      statsData.MemoryUsageBytes,
			WorkingSetBytes: statsData.MemoryWorkingSetBytes,
			RSSBytes:        statsData.MemoryRSSBytes,
		},
	}, nil
}

// maxStderrInError bounds the stderr embedded in the parse-failure error: that error is
// logged per scrape, so an unbounded guest stderr would balloon the log line.
const maxStderrInError = 1024

// truncateForError caps s to limit bytes, appending an ASCII omitted-count marker.
// limit must be non-negative; a negative limit panics the slice. Slicing at limit
// may split a UTF-8 rune; harmless since the caller embeds it via %q.
func truncateForError(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("...(%d more bytes)", len(s)-limit)
}

// newStatsExecIO returns the stats exec IO. stdout and stderr MUST be distinct buffers:
// x/crypto/ssh copies the two streams on separate goroutines, so a shared bytes.Buffer
// races and stderr corrupts the JSON. Single source of truth so the test drives real wiring.
func newStatsExecIO() (stdout, stderr *bytes.Buffer, attach *node.ExecIO) {
	stdout = &bytes.Buffer{}
	stderr = &bytes.Buffer{}
	attach = node.NewExecIO(false, nil,
		vzio.NewBufferWriteCloser(stdout),
		vzio.NewBufferWriteCloser(stderr),
		nil)
	return
}

type vmStatsData struct {
	CPUUsageNanoCores       json.Number `json:"cpuUsageNanoCores"`
	CPUUsageCoreNanoSeconds json.Number `json:"cpuUsageCoreNanoSeconds"`
	MemoryUsageBytes        json.Number `json:"memoryUsageBytes"`
	MemoryRSSBytes          json.Number `json:"memoryRssBytes"`
	MemoryWorkingSetBytes   json.Number `json:"memoryWorkingSetBytes"`
}

type parsedVMStatsData struct {
	CPUUsageNanoCores       *uint64 `json:"cpuUsageNanoCores"`
	CPUUsageCoreNanoSeconds *uint64 `json:"cpuUsageCoreNanoSeconds"`
	MemoryUsageBytes        *uint64 `json:"memoryUsageBytes"`
	MemoryRSSBytes          *uint64 `json:"memoryRssBytes"`
	MemoryWorkingSetBytes   *uint64 `json:"memoryWorkingSetBytes"`
}

func parseStatsJSON(data []byte) (*parsedVMStatsData, error) {
	// Unmarshal into intermediate structure
	var statsData vmStatsData
	if err := json.Unmarshal(data, &statsData); err != nil {
		return nil, err
	}

	// Conversion function for json.Number to *uint64
	convert := func(num json.Number) (*uint64, error) {
		val, err := num.Int64()
		if err != nil {
			return nil, err
		}
		uval := uint64(val)
		return &uval, nil
	}

	// Populate the final ParsedVMStatsData struct
	parsedData := &parsedVMStatsData{}
	var err error

	if parsedData.CPUUsageNanoCores, err = convert(statsData.CPUUsageNanoCores); err != nil {
		return nil, fmt.Errorf("cpuUsageNanoCores: %w", err)
	}
	if parsedData.CPUUsageCoreNanoSeconds, err = convert(statsData.CPUUsageCoreNanoSeconds); err != nil {
		return nil, fmt.Errorf("cpuUsageCoreNanoSeconds: %w", err)
	}
	if parsedData.MemoryUsageBytes, err = convert(statsData.MemoryUsageBytes); err != nil {
		return nil, fmt.Errorf("memoryUsageBytes: %w", err)
	}
	if parsedData.MemoryRSSBytes, err = convert(statsData.MemoryRSSBytes); err != nil {
		return nil, fmt.Errorf("memoryRssBytes: %w", err)
	}
	if parsedData.MemoryWorkingSetBytes, err = convert(statsData.MemoryWorkingSetBytes); err != nil {
		return nil, fmt.Errorf("memoryWorkingSetBytes: %w", err)
	}

	return parsedData, nil
}
