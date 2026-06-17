package resourcemanager

import (
	"bytes"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"
)

// statsJSONLine mirrors the stats script stdout.
const statsJSONLine = `{"cpuUsageNanoCores": 1, "cpuUsageCoreNanoSeconds": 2, "memoryUsageBytes": 3, "memoryRssBytes": 4, "memoryWorkingSetBytes": 5}`

// stderrWarnLine mimics a guest vm_stat/bc stderr warning.
const stderrWarnLine = "vm_stat: warning could not read pages\n"

// Aliased buffers would intermix the two streams.
func TestNewStatsExecIOSeparatesStreams(t *testing.T) {
	stdout, stderr, attach := newStatsExecIO()

	_, err := io.WriteString(attach.Stdout(), "OUT")
	require.NoError(t, err)
	_, err = io.WriteString(attach.Stderr(), "ERR")
	require.NoError(t, err)

	assert.Equal(t, "OUT", stdout.String())
	assert.Equal(t, "ERR", stderr.String())
}

// Model ssh's two io.Copy goroutines (one per stream). Aliased buffers race (-race)
// and leak stderr into stdout.
func TestStatsExecIOConcurrentWritesNoRace(t *testing.T) {
	const iterations = 200

	stdout, _, attach := newStatsExecIO()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			_, _ = io.WriteString(attach.Stdout(), statsJSONLine)
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			_, _ = io.WriteString(attach.Stderr(), stderrWarnLine)
		}
	}()
	wg.Wait()

	assert.NotContains(t, stdout.String(), "vm_stat", "stderr bytes must not leak into stdout")
}

// Short stderr (under cap) is embedded verbatim, no marker.
func TestTruncateForErrorShortUnchanged(t *testing.T) {
	const s = "short"
	got := truncateForError(s, 10)
	assert.Equal(t, s, got)
}

// Long stderr is capped to first limit bytes plus an omitted-count marker.
func TestTruncateForErrorLongTruncated(t *testing.T) {
	const limit = 8
	s := strings.Repeat("a", 50)

	got := truncateForError(s, limit)

	assert.True(t, strings.HasPrefix(got, s[:limit]), "must keep the first limit bytes")
	assert.Contains(t, got, "more bytes", "must carry the omitted-count marker")
	assert.Contains(t, got, "42", "marker reports the omitted byte count")
	assert.NotEqual(t, s, got)
}

// Marker must be ASCII (house rule + remote pre-receive hook reject non-ASCII).
func TestTruncateForErrorMarkerASCII(t *testing.T) {
	got := truncateForError(strings.Repeat("x", 50), 8)
	for i := range len(got) {
		require.Less(t, got[i], byte(0x80), "byte %d must be ASCII", i)
	}
}

// Lock the JSON field names to the parsed struct.
func TestParseStatsJSON(t *testing.T) {
	parsed, err := parseStatsJSON([]byte(statsJSONLine))
	require.NoError(t, err)
	require.NotNil(t, parsed)

	require.NotNil(t, parsed.CPUUsageNanoCores)
	require.NotNil(t, parsed.CPUUsageCoreNanoSeconds)
	require.NotNil(t, parsed.MemoryUsageBytes)
	require.NotNil(t, parsed.MemoryRSSBytes)
	require.NotNil(t, parsed.MemoryWorkingSetBytes)

	assert.Equal(t, uint64(1), *parsed.CPUUsageNanoCores)
	assert.Equal(t, uint64(2), *parsed.CPUUsageCoreNanoSeconds)
	assert.Equal(t, uint64(3), *parsed.MemoryUsageBytes)
	assert.Equal(t, uint64(4), *parsed.MemoryRSSBytes)
	assert.Equal(t, uint64(5), *parsed.MemoryWorkingSetBytes)
}

// runStatsScriptViaShell runs cmd the way the guest does in Mode B: sh reads the
// statements from stdin (one per line), mirroring internal/ssh ExecuteCommand's
// shell path. stdout and stderr are kept separate, as production does. Returns stdout.
func runStatsScriptViaShell(t *testing.T, cmd []string) []byte {
	t.Helper()
	// sh, not the guest login shell: the stats script is pure POSIX, so any POSIX
	// shell validates it; this mirrors the Mode-B stdin path in internal/ssh.
	c := exec.CommandContext(t.Context(), "sh")
	c.Stdin = strings.NewReader(strings.Join(cmd, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	require.NoError(t, c.Run(), "stats script failed; stderr=%q", stderr.String())
	return stdout.Bytes()
}

// The stats command MUST stay Mode B (separate statements over stdin). If a future
// edit collapses it to ["sh","-c",script], BuildExecCommandString would $'...'-wrap
// it and break the embedded awk/perl single quotes. BuildExecCommandString returning
// an error proves the command does NOT match the Mode-A sh -c shape.
func TestBuildVMStatsCommandStaysModeB(t *testing.T) {
	cmd := buildVMStatsCommand()
	require.NotEmpty(t, cmd)
	_, err := utils.BuildExecCommandString(cmd, nil)
	require.Error(t, err, "stats command must remain Mode-B; Mode-A $'...' would break its single-quoted awk/perl")
}

// TestVMStatsCommandExecutesOnHost runs the real stats script on the macOS host
// (the CI test job runs on a macOS runner) and checks the production parser accepts
// it and the values satisfy structural invariants. Skipped off-darwin and under
// -short (it spawns osascript and sleeps). Asserts invariants, never magnitudes:
// CLK_TCK / page size / idle load vary by host.
func TestVMStatsCommandExecutesOnHost(t *testing.T) {
	if testing.Short() {
		t.Skip("runs osascript and sleeps; skipped under -short")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("stats script needs macOS host tools (osascript, vm_stat)")
	}

	first, err := parseStatsJSON(runStatsScriptViaShell(t, buildVMStatsCommand()))
	require.NoError(t, err)
	require.NotNil(t, first.CPUUsageNanoCores)
	require.NotNil(t, first.CPUUsageCoreNanoSeconds)
	require.NotNil(t, first.MemoryUsageBytes)
	require.NotNil(t, first.MemoryRSSBytes)
	require.NotNil(t, first.MemoryWorkingSetBytes)

	assert.Positive(t, *first.MemoryUsageBytes)
	assert.Positive(t, *first.MemoryWorkingSetBytes)
	assert.Positive(t, *first.MemoryRSSBytes)
	assert.LessOrEqual(t, *first.MemoryWorkingSetBytes, *first.MemoryUsageBytes, "working set excludes reclaimable inactive, so <= usage")
	assert.LessOrEqual(t, *first.MemoryRSSBytes, *first.MemoryUsageBytes, "rss (active only) <= usage")
	assert.Positive(t, *first.CPUUsageCoreNanoSeconds, "cumulative CPU counter must be > 0")

	// Monotonic cumulative counter: a second read >= the first (the core CPU-counter property).
	second, err := parseStatsJSON(runStatsScriptViaShell(t, buildVMStatsCommand()))
	require.NoError(t, err)
	require.NotNil(t, second.CPUUsageCoreNanoSeconds)
	assert.GreaterOrEqual(t, *second.CPUUsageCoreNanoSeconds, *first.CPUUsageCoreNanoSeconds, "cumulative CPU counter must be monotonic across reads")
}
