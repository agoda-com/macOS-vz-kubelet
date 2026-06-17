package e2e_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/events"
)

func TestCreatePod(t *testing.T) {
	suite := newProviderSuite(t)

	t.Run("macos-image-upload", func(t *testing.T) {
		suite.uploadMacOSImageIfRequested(t)
	})

	registryHost := suite.registryHost(t)

	suite.ensureNamespace(t)

	node := suite.getNode(t)
	ownerRef := nodeOwnerReference(node)

	secretName := suite.createRegistrySecret(t, ownerRef, registryHost)
	pod := suite.newPod(ownerRef, secretName)
	createdPod := suite.createPod(t, pod)
	suite.waitForPostStartGateThenReady(t, createdPod.Name)

	t.Run("exec-verify-macos-poststart-file", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}

		output := suite.waitForPostStartProof(t, createdPod.Name, "macos", macosProofFilePath, "macos postStart executed")
		t.Logf("macos postStart file content: %s", output)
		assert.Contains(t, output, "macos postStart executed", "macos postStart file content should contain 'macos postStart executed'")
	})

	t.Run("exec-verify-busybox-poststart-file", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}

		output := suite.waitForPostStartProof(t, createdPod.Name, "busybox", busyboxProofFilePath, "busybox postStart executed")
		t.Logf("busybox postStart file content: %s", output)
		assert.Contains(t, output, "busybox postStart executed", "busybox postStart file content should contain 'busybox postStart executed'")
	})

	t.Run("get-container-stats", func(t *testing.T) {
		summary := suite.statsSummary(t)

		require.NotNil(t, summary.Node, "node stats should not be nil")
		assert.NotEmpty(t, summary.Node.NodeName, "node name in stats should not be empty")

		var foundPodStats *statsv1alpha1.PodStats
		for i := range summary.Pods {
			ps := summary.Pods[i]
			if ps.PodRef.Name == createdPod.Name && ps.PodRef.Namespace == suite.namespace {
				foundPodStats = &ps
				break
			}
		}
		require.NotNil(t, foundPodStats, "stats for pod %s/%s not found", suite.namespace, createdPod.Name)

		expectedContainers := map[string]bool{
			"macos":   false,
			"busybox": false,
		}

		for _, cs := range foundPodStats.Containers {
			t.Logf("Checking stats for container: %s", cs.Name)
			if _, ok := expectedContainers[cs.Name]; ok {
				require.NotNil(t, cs.CPU, "CPU stats for container %s should not be nil", cs.Name)
				assert.NotNil(t, cs.CPU.UsageCoreNanoSeconds, "CPU UsageCoreNanoSeconds for container %s should not be nil", cs.Name)
				assert.True(t, *cs.CPU.UsageCoreNanoSeconds > 0, "CPU UsageCoreNanoSeconds for container %s should be > 0", cs.Name)

				require.NotNil(t, cs.Memory, "Memory stats for container %s should not be nil", cs.Name)
				assert.NotNil(t, cs.Memory.WorkingSetBytes, "Memory WorkingSetBytes for container %s should not be nil", cs.Name)
				assert.True(t, *cs.Memory.WorkingSetBytes > 0, "Memory WorkingSetBytes for container %s should be > 0", cs.Name)

				// StartTime was never set before the fix and serialized null.
				assert.False(t, cs.StartTime.IsZero(), "StartTime for container %s should be set", cs.Name)
				expectedContainers[cs.Name] = true
			}
		}

		for name, found := range expectedContainers {
			assert.True(t, found, "stats for container %s were not found or not checked", name)
		}

		// Backend-specific ground-truth regressions, each catching defects in its own stats path:
		// macos derives CPU/memory from the in-guest script, busybox from the Docker cgroup path
		// (cgroup-v2 UsageNanoCores nil / mis-scaled, RSS/WorkingSet reading v1-only keys).
		suite.assertMacOSIdleCPU(t, createdPod.Name)
		suite.assertMacOSMemoryGroundTruth(t, createdPod.Name)
		suite.assertBusyboxCPUUnderLoad(t, createdPod.Name)
		suite.assertBusyboxMemoryGroundTruth(t, createdPod.Name)
	})

	t.Run("exec-long-to-completion", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}
		suite.testExecLongToCompletion(t, createdPod.Name)
	})

	t.Run("exec-exit-code-fidelity", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}
		suite.testExecExitCodeFidelity(t, createdPod.Name)
	})

	suite.deletePod(t, createdPod.Name)
}

// macOSContainerStats finds the macos container's stats in a fresh summary for the named pod.
func (s *providerSuite) macOSContainerStats(t *testing.T, summary statsv1alpha1.Summary, podName string) *statsv1alpha1.ContainerStats {
	t.Helper()
	for i := range summary.Pods {
		ps := summary.Pods[i]
		if ps.PodRef.Name != podName || ps.PodRef.Namespace != s.namespace {
			continue
		}
		for j := range ps.Containers {
			if ps.Containers[j].Name == "macos" {
				return &ps.Containers[j]
			}
		}
	}
	require.FailNow(t, "macos container stats not found", "pod %s/%s", s.namespace, podName)
	return nil
}

// assertMacOSIdleCPU is the idle-CPU regression check. The old bug fabricated the cumulative
// counter as ncpu*(now-boottime)*1e9, so the derived rate pinned at ~ncpu cores (constant
// ~100%/core). An idle VM must instead derive well under ncpu cores from two real samples.
func (s *providerSuite) assertMacOSIdleCPU(t *testing.T, podName string) {
	t.Helper()

	stdout, _, err := s.execContainer(t.Context(), podName, "macos", []string{"/bin/sh", "-c", "sysctl -n hw.ncpu"})
	require.NoError(t, err, "failed to read guest ncpu")
	ncpu, err := strconv.Atoi(strings.TrimSpace(stdout))
	require.NoError(t, err, "failed to parse guest ncpu %q", stdout)

	csA := s.macOSContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, csA.CPU)
	require.NotNil(t, csA.CPU.UsageCoreNanoSeconds)
	a := *csA.CPU.UsageCoreNanoSeconds
	ta := csA.CPU.Time.Time

	time.Sleep(12 * time.Second)

	csB := s.macOSContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, csB.CPU)
	require.NotNil(t, csB.CPU.UsageCoreNanoSeconds)
	b := *csB.CPU.UsageCoreNanoSeconds
	tb := csB.CPU.Time.Time

	cores := float64(b-a) / (tb.Sub(ta).Seconds() * 1e9)
	t.Logf("idle macos CPU: cores=%.3f ncpu=%d (sample delta %d ns over %s)", cores, ncpu, b-a, tb.Sub(ta))

	assert.GreaterOrEqual(t, b, a, "cumulative UsageCoreNanoSeconds must be monotonic")
	assert.Less(t, cores, float64(ncpu)*0.5,
		"idle macos VM derived %.3f cores; the old bug pins this near ncpu (%d), an idle VM must be well under half", cores, ncpu)
}

// assertMacOSMemoryGroundTruth validates the page-size + WorkingSet-formula fix against an
// independent in-guest computation. The old bug hardcoded 4096-byte pages (Apple Silicon uses
// 16384) and a wrong formula, underreporting ~4x (ratio ~0.25); the fix must land near 1.0.
func (s *providerSuite) assertMacOSMemoryGroundTruth(t *testing.T, podName string) {
	t.Helper()

	// Single-quote-free command: execContainer routes through BuildExecCommandString, which
	// wraps an `sh -c` exec in $'...' WITHOUT escaping single quotes, so an in-guest awk program
	// would break the quoting. Fetch the raw page size + vm_stat and recompute in Go instead.
	stdout, _, err := s.execContainer(t.Context(), podName, "macos", []string{"/bin/sh", "-c", "sysctl -n vm.pagesize; vm_stat"})
	require.NoError(t, err, "failed to read guest vm_stat")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "unexpected vm_stat output: %q", stdout)
	pageSize, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	require.NoError(t, err, "failed to parse guest page size %q", lines[0])

	vmStatPages := func(prefix string) uint64 {
		for _, ln := range lines[1:] {
			if !strings.HasPrefix(ln, prefix) {
				continue
			}
			v := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(ln, prefix)), ".")
			n, perr := strconv.ParseUint(v, 10, 64)
			require.NoError(t, perr, "failed to parse %q value %q", prefix, v)
			return n
		}
		require.FailNow(t, "vm_stat field not found", "prefix %q in %q", prefix, stdout)
		return 0
	}
	expectedWS := (vmStatPages("Pages active:") + vmStatPages("Pages wired down:") + vmStatPages("Pages occupied by compressor:")) * pageSize
	require.Positive(t, expectedWS, "in-guest WorkingSet must be positive")

	cs := s.macOSContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, cs.Memory)
	require.NotNil(t, cs.Memory.WorkingSetBytes)
	gotWS := *cs.Memory.WorkingSetBytes

	ratio := float64(gotWS) / float64(expectedWS)
	t.Logf("macos memory: gotWS=%d expectedWS=%d ratio=%.3f", gotWS, expectedWS, ratio)

	assert.Greater(t, gotWS, uint64(512*1024*1024), "a booted macOS VM should report > 512MiB WorkingSet")
	assert.GreaterOrEqual(t, ratio, 0.6, "reported WorkingSet (%d) too far below in-guest truth (%d); old 4x-low bug yields ratio ~0.25", gotWS, expectedWS)
	assert.LessOrEqual(t, ratio, 1.5, "reported WorkingSet (%d) too far above in-guest truth (%d)", gotWS, expectedWS)
}

// busyboxContainerStats finds the busybox sidecar's stats in a fresh summary for the named
// pod.
func (s *providerSuite) busyboxContainerStats(t *testing.T, summary statsv1alpha1.Summary, podName string) *statsv1alpha1.ContainerStats {
	t.Helper()
	for i := range summary.Pods {
		ps := summary.Pods[i]
		if ps.PodRef.Name != podName || ps.PodRef.Namespace != s.namespace {
			continue
		}
		for j := range ps.Containers {
			if ps.Containers[j].Name == "busybox" {
				return &ps.Containers[j]
			}
		}
	}
	require.FailNow(t, "busybox container stats not found", "pod %s/%s", s.namespace, podName)
	return nil
}

// assertBusyboxCPUUnderLoad drives ~1 core of CPU inside the busybox sidecar and checks both
// counters against that load. The Docker cgroup-v2 backend has two CPU defects this catches:
//   - UsageNanoCores is gated on a non-empty PercpuUsage array, which cgroup v2 leaves empty,
//     so the field is nil (require.NotNil catches it).
//   - even when set, the rate omits the *onlineCPUs factor; systemDelta is summed across all
//     CPUs, so a real 1-core load computes ~1e9/nproc. For nproc>=2 that is < 0.5e9, which the
//     lower tolerance bound catches.
//
// The derived-cores check (cumulative counter over two samples) is backend-agnostic and immune
// to the nanocores bug, so it stays the primary load proof; the instantaneous-field bounds are
// the bug-specific catchers, kept generous to tolerate a shared host.
func (s *providerSuite) assertBusyboxCPUUnderLoad(t *testing.T, podName string) {
	t.Helper()
	ctx := t.Context()

	nprocOut, _, err := s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", "nproc"})
	require.NoError(t, err, "failed to read busybox nproc")
	nproc, err := strconv.Atoi(strings.TrimSpace(nprocOut))
	require.NoError(t, err, "failed to parse busybox nproc %q", nprocOut)
	require.Positive(t, nproc, "busybox nproc must be positive")

	// One detached busy loop ~= 1 core. The exec stream closes when the command returns, so the
	// burner is nohup-backgrounded and orphan-safe (busybox setsid may be absent; nohup + & is
	// the portable form). Its PID is recorded so cleanup kills it deterministically rather than
	// relying on pod teardown.
	const burnPIDFile = "/tmp/busybox_cpu_burn.pid"
	start := fmt.Sprintf("nohup sh -c 'while true; do :; done' >/dev/null 2>&1 & echo $! > %s; cat %s", burnPIDFile, burnPIDFile)
	pidOut, _, err := s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", start})
	require.NoError(t, err, "failed to start busybox CPU burner")
	burnPID := strings.TrimSpace(pidOut)
	require.NotEmpty(t, burnPID, "busybox CPU burner PID not captured")
	t.Logf("busybox CPU burner pid=%s nproc=%d", burnPID, nproc)

	t.Cleanup(func() {
		stop := fmt.Sprintf("kill -9 $(cat %s) 2>/dev/null; rm -f %s", burnPIDFile, burnPIDFile)
		if _, _, cerr := s.execContainer(context.Background(), podName, "busybox", []string{"sh", "-c", stop}); cerr != nil {
			t.Logf("busybox CPU burner cleanup error (best effort): %v", cerr)
		}
	})

	// Confirm the burner is actually alive before sampling, else a load assertion would chase a
	// dead process.
	aliveOut, _, err := s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", fmt.Sprintf("kill -0 $(cat %s) 2>/dev/null && echo alive || echo dead", burnPIDFile)})
	require.NoError(t, err, "failed to probe busybox CPU burner")
	require.Equal(t, "alive", strings.TrimSpace(aliveOut), "busybox CPU burner is not running")

	csA := s.busyboxContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, csA.CPU)
	require.NotNil(t, csA.CPU.UsageCoreNanoSeconds)
	a := *csA.CPU.UsageCoreNanoSeconds
	ta := csA.CPU.Time.Time

	time.Sleep(12 * time.Second)

	csB := s.busyboxContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, csB.CPU)
	require.NotNil(t, csB.CPU.UsageCoreNanoSeconds)
	b := *csB.CPU.UsageCoreNanoSeconds
	tb := csB.CPU.Time.Time

	cores := float64(b-a) / (tb.Sub(ta).Seconds() * 1e9)
	t.Logf("busybox CPU under load: cores=%.3f nproc=%d (sample delta %d ns over %s)", cores, nproc, b-a, tb.Sub(ta))

	assert.GreaterOrEqual(t, b, a, "cumulative UsageCoreNanoSeconds must be monotonic")
	assert.Greater(t, cores, 0.5, "busybox under a 1-core burner derived only %.3f cores; expected well above 0.5", cores)

	// Instantaneous field. require.NotNil is the HARD regression catcher: nil on cgroup v2 today
	// because the field is gated on a non-empty PercpuUsage array, empty on v2.
	require.NotNil(t, csB.CPU.UsageNanoCores, "busybox CPU UsageNanoCores is nil; cgroup-v2 leaves PercpuUsage empty, gating the field off")

	// Magnitude lower-bound is sampled, not single-shot: Docker's single-read ContainerStats
	// derives UsageNanoCores over its own ~1s internal pre-read window (not the 12s above), so on
	// a busy shared host the burner can be scheduled out during one micro-window and dip a lone
	// sample below 0.5e9. Take several consecutive reads and require the MAX to clear it, which
	// absorbs a scheduled-out window while still proving the value is real (the missing
	// *onlineCPUs factor scales a true 1-core load to ~1e9/nproc, caught for nproc>2).
	const nanoCoreSamples = 3
	upper := uint64(float64(nproc+1) * 1e9)
	var maxNanoCores uint64
	for i := range nanoCoreSamples {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		cs := s.busyboxContainerStats(t, s.statsSummary(t), podName)
		require.NotNil(t, cs.CPU)
		require.NotNil(t, cs.CPU.UsageNanoCores)
		v := *cs.CPU.UsageNanoCores
		t.Logf("busybox CPU UsageNanoCores sample %d/%d = %d", i+1, nanoCoreSamples, v)
		if v > maxNanoCores {
			maxNanoCores = v
		}
		// Upper bound holds per-sample: no sample should exceed ~1 core of headroom.
		assert.LessOrEqual(t, v, upper,
			"busybox UsageNanoCores sample (%d) above (nproc+1) cores (%d); load should be ~1 core", v, upper)
	}
	t.Logf("busybox CPU UsageNanoCores max=%d over %d samples, lower bound=%d", maxNanoCores, nanoCoreSamples, uint64(0.5e9))
	assert.GreaterOrEqual(t, maxNanoCores, uint64(0.5e9),
		"busybox UsageNanoCores max over %d samples (%d) below 0.5 core under a 1-core load; the missing *onlineCPUs factor scales a real core down to ~1e9/nproc", nanoCoreSamples, maxNanoCores)
}

// assertBusyboxMemoryGroundTruth validates the sidecar's reported RSS and WorkingSet against
// the container's own cgroup memory, read from inside busybox. The Docker backend reads
// cgroup-v1-only keys: RSS from Stats["rss"] (cgroup v2 names it "anon", so RSS reports 0) and
// WorkingSet as Usage-Stats["cache"] (cgroup v2 has no "cache", so WorkingSet collapses to
// Usage instead of the correct Usage-inactive_file). Ground truth: memory.current minus
// inactive_file for WorkingSet, anon for RSS. Falls back to cgroup v1 keys, and skips on an
// unrecognized cgroup memory layout rather than false-failing.
func (s *providerSuite) assertBusyboxMemoryGroundTruth(t *testing.T, podName string) {
	t.Helper()
	ctx := t.Context()

	// Generate modest page cache so inactive_file > 0, which makes the correct WorkingSet
	// (Usage-inactive_file) diverge from the buggy one (Usage) and turns the WS upper bound into
	// a real catcher. The file is left RESIDENT (no rm here): page cache is reclaimed quickly on
	// an idle container, so inactive_file would be back near 0 by the kubelet stats read and the
	// two WorkingSet values would re-converge. Holding the file keeps inactive_file non-zero at
	// BOTH the cgroup-truth read and the stats-summary read below; cleanup removes it after.
	const wsFile = "/tmp/wsfill"
	_, _, err := s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=32 2>/dev/null; cat %s >/dev/null", wsFile, wsFile)})
	require.NoError(t, err, "failed to generate busybox page cache")
	t.Cleanup(func() {
		if _, _, cerr := s.execContainer(context.Background(), podName, "busybox", []string{"sh", "-c", fmt.Sprintf("rm -f %s", wsFile)}); cerr != nil {
			t.Logf("busybox page-cache cleanup error (best effort): %v", cerr)
		}
	})

	// Read the cgroup files raw and parse in Go. cgroup v2 first (this host), then v1.
	read := func(path string) (string, bool) {
		out, _, rerr := s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", fmt.Sprintf("cat %s 2>/dev/null", path)})
		out = strings.TrimSpace(out)
		return out, rerr == nil && out != ""
	}
	statValue := func(stat, key string) (uint64, bool) {
		for _, ln := range strings.Split(stat, "\n") {
			fields := strings.Fields(ln)
			if len(fields) == 2 && fields[0] == key {
				v, perr := strconv.ParseUint(fields[1], 10, 64)
				return v, perr == nil
			}
		}
		return 0, false
	}

	var expectedRSS, expectedWS uint64
	if current, ok := read("/sys/fs/cgroup/memory.current"); ok {
		usage, perr := strconv.ParseUint(current, 10, 64)
		require.NoError(t, perr, "failed to parse memory.current %q", current)
		stat, ok := read("/sys/fs/cgroup/memory.stat")
		require.True(t, ok, "cgroup v2 memory.stat unreadable")
		anon, ok := statValue(stat, "anon")
		require.True(t, ok, "cgroup v2 memory.stat missing anon")
		inactiveFile, ok := statValue(stat, "inactive_file")
		require.True(t, ok, "cgroup v2 memory.stat missing inactive_file")
		expectedRSS = anon
		expectedWS = usage - inactiveFile
		t.Logf("busybox cgroup v2 truth: current=%d anon=%d inactive_file=%d", usage, anon, inactiveFile)
	} else if usageStr, ok := read("/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok {
		usage, perr := strconv.ParseUint(usageStr, 10, 64)
		require.NoError(t, perr, "failed to parse memory.usage_in_bytes %q", usageStr)
		stat, ok := read("/sys/fs/cgroup/memory/memory.stat")
		require.True(t, ok, "cgroup v1 memory.stat unreadable")
		rss, ok := statValue(stat, "rss")
		require.True(t, ok, "cgroup v1 memory.stat missing rss")
		inactiveFile, ok := statValue(stat, "total_inactive_file")
		require.True(t, ok, "cgroup v1 memory.stat missing total_inactive_file")
		expectedRSS = rss
		expectedWS = usage - inactiveFile
		t.Logf("busybox cgroup v1 truth: usage=%d rss=%d total_inactive_file=%d", usage, rss, inactiveFile)
	} else {
		t.Skip("busybox cgroup memory layout unrecognized (neither v2 memory.current nor v1 memory.usage_in_bytes); cannot establish ground truth")
	}
	require.Positive(t, expectedRSS, "in-container RSS ground truth must be positive")
	require.Positive(t, expectedWS, "in-container WorkingSet ground truth must be positive")

	// Re-touch the file so its pages are warm at the stats read too, then fetch the summary
	// immediately. The cgroup-truth reads above cost a few exec round-trips, during which idle
	// reclaim could trim inactive_file; this keeps it non-zero at the kubelet read so the
	// reported and truth WorkingSet are measured against the same resident cache.
	_, _, err = s.execContainer(ctx, podName, "busybox", []string{"sh", "-c", fmt.Sprintf("cat %s >/dev/null", wsFile)})
	require.NoError(t, err, "failed to re-warm busybox page cache before stats read")

	cs := s.busyboxContainerStats(t, s.statsSummary(t), podName)
	require.NotNil(t, cs.Memory)
	require.NotNil(t, cs.Memory.RSSBytes)
	require.NotNil(t, cs.Memory.WorkingSetBytes)
	gotRSS := *cs.Memory.RSSBytes
	gotWS := *cs.Memory.WorkingSetBytes

	rssRatio := float64(gotRSS) / float64(expectedRSS)
	wsRatio := float64(gotWS) / float64(expectedWS)
	t.Logf("busybox memory: gotRSS=%d expectedRSS=%d (ratio %.3f) gotWS=%d expectedWS=%d (ratio %.3f)",
		gotRSS, expectedRSS, rssRatio, gotWS, expectedWS, wsRatio)

	// RSS=0 today on cgroup v2 (reads Stats["rss"], named "anon" in v2); the clean catcher.
	assert.Positive(t, gotRSS, "busybox RSSBytes is 0; cgroup v2 names the RSS key 'anon', not 'rss'")
	assert.GreaterOrEqual(t, rssRatio, 0.5, "busybox RSSBytes (%d) too far below in-container truth (%d)", gotRSS, expectedRSS)
	assert.LessOrEqual(t, rssRatio, 2.0, "busybox RSSBytes (%d) too far above in-container truth (%d)", gotRSS, expectedRSS)

	// WorkingSet collapses to Usage today (reads Stats["cache"], absent in v2); with page cache
	// present, truth = Usage-inactive_file is meaningfully below Usage, so the upper bound bites.
	assert.GreaterOrEqual(t, wsRatio, 0.5, "busybox WorkingSetBytes (%d) too far below in-container truth (%d)", gotWS, expectedWS)
	assert.LessOrEqual(t, wsRatio, 2.0, "busybox WorkingSetBytes (%d) too far above in-container truth (%d); old code reports Usage, not Usage-inactive_file", gotWS, expectedWS)
}

// testExecLongToCompletion proves a healthy long non-PTY exec runs to natural
// completion and is never spuriously reaped by the exec/attach context.AfterFunc
// cancel wiring. The top real-job risk: a regression here kills every long build
// stage. Assert only the terminal sentinel, never intermediate tick timing
// (wall-clock-sensitive on a loaded baremetal runner).
func (s *providerSuite) testExecLongToCompletion(t *testing.T, pod string) {
	t.Helper()
	const n = 8
	script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do i=$((i+1)); echo "tick $i"; sleep 1; done; echo DONE`, n)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second) // generous; exec is a deadline-less path
	defer cancel()
	stdout, stderr, err := s.execContainer(ctx, pod, "macos", []string{"/bin/sh", "-c", script})
	require.NoError(t, err, "healthy long exec must complete, not be reaped")
	lines := strings.Fields(strings.TrimSpace(stdout))
	require.NotEmpty(t, lines)
	assert.Equal(t, "DONE", lines[len(lines)-1], "script must reach its end (no spurious reap)")
	if errOut := strings.TrimSpace(stderr); errOut != "" {
		t.Logf("long exec emitted stderr (non-fatal): %q", errOut)
	}
}

// testExecExitCodeFidelity proves the exec transport surfaces the exact non-zero
// exit code end-to-end and keeps stdout/stderr separate. Thin by design: it proves
// the transport, not the full code table (that lives in internal/execerror unit tests).
func (s *providerSuite) testExecExitCodeFidelity(t *testing.T, pod string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, want := range []int{0, 42} {
		_, _, code, err := s.execCode(ctx, pod, "macos", []string{"/bin/sh", "-c", fmt.Sprintf("exit %d", want)})
		require.NoError(t, err, "exit %d: transport error must not mask the code", want)
		assert.Equal(t, want, code)
	}
	stdout, stderr, code, err := s.execCode(ctx, pod, "macos",
		[]string{"/bin/sh", "-c", "echo to_stdout; echo to_stderr >&2; exit 3"})
	require.NoError(t, err)
	assert.Equal(t, 3, code)
	assert.Equal(t, "to_stdout", strings.TrimSpace(stdout))
	assert.Equal(t, "to_stderr", strings.TrimSpace(stderr))
}

// TestPostStartHookFailure asserts the failure contract: an exec postStart hook that
// exits non-zero drives the macOS pod to Failed and emits a FailedPostStartHook event.
// Runs serially (no t.Parallel), so only one VM exists at a time; minimal
// single-container pod (no busybox sidecar) keeps it cheap.
func TestPostStartHookFailure(t *testing.T) {
	suite := newProviderSuite(t)

	registryHost := suite.registryHost(t)
	suite.ensureNamespace(t)

	node := suite.getNode(t)
	ownerRef := nodeOwnerReference(node)

	secretName := suite.createRegistrySecret(t, ownerRef, registryHost)
	pod := suite.newFailingPostStartPod(ownerRef, secretName)
	createdPod := suite.createPod(t, pod)

	failedPod := suite.waitForPodFailed(t, createdPod.Name)
	require.Equal(t, corev1.PodFailed, failedPod.Status.Phase, "pod should reach Failed phase after postStart hook exits non-zero")

	message := suite.waitForPodEventReason(t, createdPod.Name, events.FailedPostStartHook)
	assert.Contains(t, message, "macos", "FailedPostStartHook event should reference the macos container")

	suite.deletePod(t, createdPod.Name)
}

// TestExecDeleteDuringExec proves a pod delete during an in-flight exec aborts the
// exec promptly (an error, not a hang) and tears the VM down to NotFound. Mirrors the
// GitLab job-cancel path: a long non-PTY exec, pod deleted mid-stream. Owns its pod
// (it deletes it), so a top-level test rather than a reuse-the-VM subtest.
func TestExecDeleteDuringExec(t *testing.T) {
	if *certPath == "" || *keyPath == "" {
		t.Skip("exec requires apiserver client certs")
	}
	suite := newProviderSuite(t)

	registryHost := suite.registryHost(t)
	suite.ensureNamespace(t)
	node := suite.getNode(t)
	ownerRef := nodeOwnerReference(node)
	secretName := suite.createRegistrySecret(t, ownerRef, registryHost)

	// Relies on the macOS image already pushed to the local registry by TestCreatePod
	// (the job-start docker prune clears stale volumes; CI runs the package in order).
	pod := suite.newPod(ownerRef, secretName)
	createdPod := suite.createPod(t, pod)
	suite.waitForPostStartGateThenReady(t, createdPod.Name)

	execCtx, execCancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer execCancel()

	errCh := make(chan error, 1)
	go func() {
		// Long single-quote-free loop; the delete (not the loop's own length) must end it.
		_, _, err := suite.execContainer(execCtx, createdPod.Name, "macos",
			[]string{"/bin/sh", "-c", "i=0; while [ $i -lt 600 ]; do i=$((i+1)); sleep 1; done"})
		errCh <- err
	}()

	time.Sleep(4 * time.Second) // let the exec establish before deleting

	// Two-part no-hang guarantee: deletePod blocks until the pod reaches NotFound
	// (teardown not wedged by the in-flight exec), then the select proves that exec
	// then returns an error, not a hang. deletePod stays on the main goroutine
	// (it uses require), so the exec runs in the goroutine above, not the delete.
	suite.deletePod(t, createdPod.Name)

	select {
	case err := <-errCh:
		require.Error(t, err, "in-flight exec must return an error when its pod is deleted, not hang")
	case <-time.After(time.Minute):
		t.Fatal("in-flight exec did not return within 1m after pod deletion (hang)")
	}
}
