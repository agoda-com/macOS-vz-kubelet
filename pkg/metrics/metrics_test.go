package metrics_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client/mocks"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/metrics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	vkslog "github.com/virtual-kubelet/virtual-kubelet/log/slog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// logEntry is one recorded log call: its level and the formatted message.
type logEntry struct {
	level slog.Level
	msg   string
}

// recorder is a slog.Handler capturing level+message, wired into vklog via the
// fork adapter. Edge logs fire post-Wait single-threaded, but g.Go closures may
// log concurrently for other pods, and slog.Handler.Handle must be concurrency
// safe, so the slice is mutex-guarded.
type recorder struct {
	mu      sync.Mutex
	entries []*logEntry
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, &logEntry{level: rec.Level, msg: rec.Message})
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) count(level slog.Level, substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		if e.level == level && strings.Contains(e.msg, substr) {
			n++
		}
	}
	return n
}

func runningPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      name,
			UID:       types.UID("ns/" + name),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func podListerWith(t *testing.T, pods ...*corev1.Pod) corev1listers.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range pods {
		require.NoError(t, indexer.Add(p))
	}
	return corev1listers.NewPodLister(indexer)
}

// One pod's stats failure must not fail the whole summary; it is skipped, healthy pods report.
func TestGetStatsSummarySkipsFailedPod(t *testing.T) {
	good1 := runningPod("pod-good-1")
	bad := runningPod("pod-bad")
	good2 := runningPod("pod-good-2")

	vzClient := mocks.NewVzClientInterface(t)
	healthy := []stats.ContainerStats{{Name: "c0"}}
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-good-1", mock.Anything).
		Return(healthy, nil)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Return(nil, errors.New("boom"))
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-good-2", mock.Anything).
		Return(healthy, nil)

	lister := podListerWith(t, good1, bad, good2)
	p := metrics.NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.NotNil(t, summary)

	names := map[string]struct{}{}
	for _, ps := range summary.Pods {
		names[ps.PodRef.Name] = struct{}{}
	}
	assert.Len(t, summary.Pods, 2)
	assert.Contains(t, names, "pod-good-1")
	assert.Contains(t, names, "pod-good-2")
	assert.NotContains(t, names, "pod-bad")
}

// Zero ContainerStats.StartTime (provider tracks no per-container start) must be
// backfilled from pod CreationTimestamp so summary doesn't serialize null.
func TestGetStatsSummarySetsContainerStartTime(t *testing.T) {
	pod := runningPod("pod-start")
	created := metav1.NewTime(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	pod.CreationTimestamp = created

	vzClient := mocks.NewVzClientInterface(t)
	cpu := uint64(1)
	mem := uint64(2)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-start", mock.Anything).
		Return([]stats.ContainerStats{{
			Name:   "macos",
			CPU:    &stats.CPUStats{UsageCoreNanoSeconds: &cpu},
			Memory: &stats.MemoryStats{WorkingSetBytes: &mem},
		}}, nil)

	lister := podListerWith(t, pod)
	p := metrics.NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.Len(t, summary.Pods, 1)
	require.Len(t, summary.Pods[0].Containers, 1)
	assert.Equal(t, created, summary.Pods[0].Containers[0].StartTime)
}

// Per-pod DeadlineExceeded (this pod's own dial/exec timeout) is skipped, not propagated.
// Guards against keying on errors.Is, which would fail the whole summary.
func TestGetStatsSummarySkipsPerPodDeadline(t *testing.T) {
	good := runningPod("pod-good")
	bad := runningPod("pod-bad")

	vzClient := mocks.NewVzClientInterface(t)
	healthy := []stats.ContainerStats{{Name: "c0"}}
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-good", mock.Anything).
		Return(healthy, nil)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Return(nil, context.DeadlineExceeded)

	lister := podListerWith(t, good, bad)
	p := metrics.NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	summary, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.NotNil(t, summary)

	names := map[string]struct{}{}
	for _, ps := range summary.Pods {
		names[ps.PodRef.Name] = struct{}{}
	}
	assert.Len(t, summary.Pods, 1)
	assert.Contains(t, names, "pod-good")
	assert.NotContains(t, names, "pod-bad")
}

// Scrape cancel via the call return (mock cancels mid-call, so the pre-call select
// misses it) must propagate, exercising the post-call ctx.Err() check.
func TestGetStatsSummaryPropagatesScrapeCancellation(t *testing.T) {
	bad := runningPod("pod-bad")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vzClient := mocks.NewVzClientInterface(t)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Run(func(_ context.Context, _ string, _ string, _ []corev1.Container) {
			cancel()
		}).
		Return(nil, context.Canceled)

	lister := podListerWith(t, bad)
	p := metrics.NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	summary, err := p.GetStatsSummary(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Nil(t, summary)
}

// A persistently-failing pod must log once on the rising edge, not every scrape;
// once it recovers it logs once on the falling edge. Guards against per-scrape
// Error spam burying real errors.
func TestGetStatsSummaryEdgeTriggeredSkipLogging(t *testing.T) {
	bad := runningPod("pod-flaky")

	vzClient := mocks.NewVzClientInterface(t)
	healthy := []stats.ContainerStats{{Name: "c0"}}
	// Three failing scrapes, then one recovered scrape.
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-flaky", mock.Anything).
		Return(nil, errors.New("boom")).Times(3)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-flaky", mock.Anything).
		Return(healthy, nil).Once()

	lister := podListerWith(t, bad)
	p := metrics.NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	rec := &recorder{}
	ctx := vklog.WithLogger(context.Background(), vkslog.FromSlog(slog.New(rec)))

	for range 3 {
		_, err := p.GetStatsSummary(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, rec.count(slog.LevelWarn, "failing"), "persistently wedged pod logs once across three failing scrapes")
	assert.Equal(t, 0, rec.count(slog.LevelError, "skipping"), "per-pod skip must never log at Error (old per-scrape spam)")

	_, err := p.GetStatsSummary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.count(slog.LevelInfo, "recovered"), "falling edge logs recovery once")
	assert.Equal(t, 1, rec.count(slog.LevelWarn, "failing"), "recovery scrape adds no new failing log")
}
