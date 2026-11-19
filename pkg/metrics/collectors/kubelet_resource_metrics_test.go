package collectors_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/metrics/collectors"
)

func ptr(v uint64) *uint64 { return &v }

func TestResourceMetricsCollectorNodeMetrics(t *testing.T) {
	now := metav1.NewTime(time.Unix(1794950000, 0))
	summary := &stats.Summary{
		Node: stats.NodeStats{
			NodeName: "node",
			CPU:      &stats.CPUStats{Time: now, UsageCoreNanoSeconds: ptr(uint64(150 * time.Second))},
			Memory:   &stats.MemoryStats{Time: now, WorkingSetBytes: ptr(4 << 30)},
		},
		Pods: []stats.PodStats{{
			PodRef: stats.PodReference{Name: "pod", Namespace: "ns"},
			Containers: []stats.ContainerStats{{
				Name:   "c",
				CPU:    &stats.CPUStats{Time: now, UsageCoreNanoSeconds: ptr(uint64(10 * time.Second))},
				Memory: &stats.MemoryStats{Time: now, WorkingSetBytes: ptr(1 << 30)},
			}},
		}},
	}

	want := `
# HELP container_cpu_usage_seconds_total [ALPHA] Cumulative cpu time consumed by the container in core-seconds
# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{container="c",namespace="ns",pod="pod"} 10 1794950000000
# HELP container_memory_working_set_bytes [ALPHA] Current working set of the container in bytes
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{container="c",namespace="ns",pod="pod"} 1.073741824e+09 1794950000000
# HELP node_cpu_usage_seconds_total [ALPHA] Cumulative cpu time consumed by the node in core-seconds
# TYPE node_cpu_usage_seconds_total counter
node_cpu_usage_seconds_total 150 1794950000000
# HELP node_memory_working_set_bytes [ALPHA] Current working set of the node in bytes
# TYPE node_memory_working_set_bytes gauge
node_memory_working_set_bytes 4.294967296e+09 1794950000000
# HELP scrape_error [ALPHA] 1 if there was an error while getting container metrics, 0 otherwise
# TYPE scrape_error gauge
scrape_error 0
`
	err := testutil.CustomCollectAndCompare(collectors.NewKubeletResourceMetricsCollector(summary), strings.NewReader(want),
		"container_cpu_usage_seconds_total", "container_memory_working_set_bytes",
		"node_cpu_usage_seconds_total", "node_memory_working_set_bytes", "scrape_error")
	require.NoError(t, err)
}

func TestResourceMetricsCollectorWithoutNodeStats(t *testing.T) {
	summary := &stats.Summary{Node: stats.NodeStats{NodeName: "node"}}

	registry := compbasemetrics.NewKubeRegistry()
	registry.CustomMustRegister(collectors.NewKubeletResourceMetricsCollector(summary))
	families, err := registry.Gather()
	require.NoError(t, err)

	for _, f := range families {
		require.NotContains(t, f.GetName(), "node_", "no node metrics without node stats")
	}
}
