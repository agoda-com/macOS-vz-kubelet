package collectors

import (
	"time"

	compbasemetrics "k8s.io/component-base/metrics"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// defining metrics
var (
	nodeCPUUsageDesc = compbasemetrics.NewDesc("node_cpu_usage_seconds_total",
		"Cumulative cpu time consumed by the node in core-seconds",
		nil,
		nil,
		compbasemetrics.ALPHA,
		"")

	nodeMemoryUsageDesc = compbasemetrics.NewDesc("node_memory_working_set_bytes",
		"Current working set of the node in bytes",
		nil,
		nil,
		compbasemetrics.ALPHA,
		"")

	containerCPUUsageDesc = compbasemetrics.NewDesc("container_cpu_usage_seconds_total",
		"Cumulative cpu time consumed by the container in core-seconds",
		[]string{"container", "pod", "namespace"},
		nil,
		compbasemetrics.ALPHA,
		"")

	containerMemoryUsageDesc = compbasemetrics.NewDesc("container_memory_working_set_bytes",
		"Current working set of the container in bytes",
		[]string{"container", "pod", "namespace"},
		nil,
		compbasemetrics.ALPHA,
		"")

	resourceScrapeResultDesc = compbasemetrics.NewDesc("scrape_error",
		"1 if there was an error while getting container metrics, 0 otherwise",
		nil,
		nil,
		compbasemetrics.ALPHA,
		"")
)

// NewResourceMetricsCollector returns a metrics.StableCollector which exports resource metrics
// nolint: ireturn
func NewKubeletResourceMetricsCollector(podStats *stats.Summary) compbasemetrics.StableCollector {
	return &resourceMetricsCollector{
		providerPodStats: podStats,
	}
}

type resourceMetricsCollector struct {
	compbasemetrics.BaseStableCollector

	providerPodStats *stats.Summary
}

// Check if resourceMetricsCollector implements necessary interface
var _ compbasemetrics.StableCollector = &resourceMetricsCollector{}

// DescribeWithStability implements compbasemetrics.StableCollector
func (rc *resourceMetricsCollector) DescribeWithStability(ch chan<- *compbasemetrics.Desc) {
	ch <- nodeCPUUsageDesc
	ch <- nodeMemoryUsageDesc
	ch <- containerCPUUsageDesc
	ch <- containerMemoryUsageDesc
	ch <- resourceScrapeResultDesc
}

// CollectWithStability implements compbasemetrics.StableCollector
// Since new containers are frequently created and removed, using the Gauge would
// leak metric collectors for containers or pods that no longer exist.  Instead, implement
// custom collector in a way that only collects metrics for active containers.
func (rc *resourceMetricsCollector) CollectWithStability(ch chan<- compbasemetrics.Metric) {
	var errorCount float64
	defer func() {
		ch <- compbasemetrics.NewLazyConstMetric(resourceScrapeResultDesc, compbasemetrics.GaugeValue, errorCount)
	}()

	statsSummary := *rc.providerPodStats
	rc.collectNodeCPUMetrics(ch, statsSummary.Node)
	rc.collectNodeMemoryMetrics(ch, statsSummary.Node)
	for _, pod := range statsSummary.Pods {
		for _, container := range pod.Containers {
			rc.collectContainerCPUMetrics(ch, pod, container)
			rc.collectContainerMemoryMetrics(ch, pod, container)
		}
	}
}

// implement collector methods and validate that correct data is used

func (rc *resourceMetricsCollector) collectNodeCPUMetrics(ch chan<- compbasemetrics.Metric, s stats.NodeStats) {
	if s.CPU == nil || s.CPU.UsageCoreNanoSeconds == nil {
		return
	}

	ch <- compbasemetrics.NewLazyMetricWithTimestamp(s.CPU.Time.Time,
		compbasemetrics.NewLazyConstMetric(nodeCPUUsageDesc, compbasemetrics.CounterValue,
			float64(*s.CPU.UsageCoreNanoSeconds)/float64(time.Second)))
}

func (rc *resourceMetricsCollector) collectNodeMemoryMetrics(ch chan<- compbasemetrics.Metric, s stats.NodeStats) {
	if s.Memory == nil || s.Memory.WorkingSetBytes == nil {
		return
	}

	ch <- compbasemetrics.NewLazyMetricWithTimestamp(s.Memory.Time.Time,
		compbasemetrics.NewLazyConstMetric(nodeMemoryUsageDesc, compbasemetrics.GaugeValue,
			float64(*s.Memory.WorkingSetBytes)))
}

func (rc *resourceMetricsCollector) collectContainerCPUMetrics(ch chan<- compbasemetrics.Metric, pod stats.PodStats, s stats.ContainerStats) {
	if s.CPU == nil || s.CPU.UsageCoreNanoSeconds == nil {
		return
	}

	ch <- compbasemetrics.NewLazyMetricWithTimestamp(s.CPU.Time.Time,
		compbasemetrics.NewLazyConstMetric(containerCPUUsageDesc, compbasemetrics.CounterValue,
			float64(*s.CPU.UsageCoreNanoSeconds)/float64(time.Second), s.Name, pod.PodRef.Name, pod.PodRef.Namespace))
}

func (rc *resourceMetricsCollector) collectContainerMemoryMetrics(ch chan<- compbasemetrics.Metric, pod stats.PodStats, s stats.ContainerStats) {
	if s.Memory == nil || s.Memory.WorkingSetBytes == nil {
		return
	}

	ch <- compbasemetrics.NewLazyMetricWithTimestamp(s.Memory.Time.Time,
		compbasemetrics.NewLazyConstMetric(containerMemoryUsageDesc, compbasemetrics.GaugeValue,
			float64(*s.Memory.WorkingSetBytes), s.Name, pod.PodRef.Name, pod.PodRef.Namespace))
}
