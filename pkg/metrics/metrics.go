package metrics

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/metrics/collectors"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	compbasemetrics "k8s.io/component-base/metrics"
)

type MacOSVZPodMetricsProvider struct {
	nodeName    string
	metricsSync sync.Mutex

	podLister corev1listers.PodLister
	vzClient  client.VzClientInterface

	// failingPods is the set of pods whose stats scrape is currently failing, for
	// edge-triggered skip logging (log on state change, not every scrape). Keyed by
	// UID so same-name recreation is a distinct pod. Guarded by metricsSync, but only
	// ever touched in the post-Wait single-threaded section, never by the g.Go closures.
	failingPods map[types.UID]struct{}
}

// NewMacOSVZPodMetricsProvider creates a new MacOSVZPodMetricsProvider
func NewMacOSVZPodMetricsProvider(nodeName string, podLister corev1listers.PodLister, vzClient client.VzClientInterface) *MacOSVZPodMetricsProvider {
	return &MacOSVZPodMetricsProvider{
		nodeName:    nodeName,
		podLister:   podLister,
		vzClient:    vzClient,
		failingPods: make(map[types.UID]struct{}),
	}
}

// podStat is the single self-describing outcome slot per pod index, carrying the pod
// ref plus the outcome (stats on success, err on per-pod skip). The consume loop reads
// pod identity FROM THE SLOT, so dispatch and consume stay decoupled, not lockstep-tied
// to a parallel pods[i] index.
type podStat struct {
	pod   *corev1.Pod
	stats *stats.PodStats
	err   error
}

// GetStatsSummary gets the stats for the node, including running pods
func (p *MacOSVZPodMetricsProvider) GetStatsSummary(ctx context.Context) (summary *stats.Summary, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZPodMetricsProvider.GetStatsSummary")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received GetStatsSummary request")

	p.metricsSync.Lock()
	defer p.metricsSync.Unlock()

	log.G(ctx).Debug("Acquired metrics lock")

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	pods, listErr := p.podLister.List(labels.Everything())
	if listErr != nil {
		log.G(ctx).WithError(listErr).Errorf("failed to retrieve pods list")
	}

	g := errgroup.Group{}
	// One slot per pod index; disjoint writes, no mutex. A nil-pod slot is a
	// non-running pod skipped at dispatch.
	results := make([]podStat, len(pods))

	for i, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		g.Go(func() (err error) {
			ctx, span := trace.StartSpan(ctx, "getPodMetrics")
			defer func() {
				span.SetStatus(err)
				span.End()
			}()
			ctx = span.WithFields(ctx, log.Fields{
				"UID":       string(pod.UID),
				"Name":      pod.Name,
				"Namespace": pod.Namespace,
			})

			// Cancelled scrape is fatal; propagate. Per-pod stats error below is log-and-skip.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			cs, err := p.vzClient.GetVirtualizationGroupStats(ctx, pod.Namespace, pod.Name, pod.Spec.Containers)
			if err != nil {
				// Scrape cancel/deadline fails loud; key on ctx not errors.Is, since a
				// per-pod dial/exec timeout also surfaces DeadlineExceeded but leaves
				// ctx.Err()==nil (skip it, else one VM zeroes the whole summary).
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Defer logging to the post-Wait edge detection; logging here would spam
				// once per scrape (~15s) for a persistently-wedged pod.
				results[i] = podStat{pod: pod, err: err}
				return nil
			}

			// ContainerStats.StartTime is otherwise unset (serializes null); mirror
			// PodStats.StartTime from pod creation, the provider tracks no per-container start.
			for j := range cs {
				if cs[j].StartTime.IsZero() {
					cs[j].StartTime = pod.CreationTimestamp
				}
			}

			results[i] = podStat{pod: pod, stats: &stats.PodStats{
				PodRef: stats.PodReference{
					Name:      pod.Name,
					Namespace: pod.Namespace,
					UID:       string(pod.UID),
				},
				StartTime:  pod.CreationTimestamp,
				Containers: cs,
			}}
			return nil
		})
	}

	// g.Wait errors only on a cancelled scrape now; per-pod failures already skipped.
	// On that early return we must NOT mutate failingPods (the scrape is incomplete).
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("failed to get stats for all pods: %w", err)
	}

	var s stats.Summary
	s.Node = stats.NodeStats{
		NodeName: p.nodeName,
	}

	// Edge-triggered skip logging, single-threaded under metricsSync: log on the
	// rising (newly failing) and falling (recovered) edges only. Rebuild the set from
	// this scrape's failures so succeeded/vanished pods are pruned (no unbounded growth
	// under CI pod churn); a pod that vanished while failing is dropped silently since
	// no recovery is observable for a gone pod.
	failing := make(map[types.UID]struct{})
	for i := range results {
		r := results[i]
		if r.pod == nil {
			continue
		}
		_, wasFailing := p.failingPods[r.pod.UID]
		if r.stats != nil {
			if wasFailing {
				log.G(ctx).Infof("virtualization group stats recovered for pod %s/%s", r.pod.Namespace, r.pod.Name)
			}
			s.Pods = append(s.Pods, *r.stats)
			continue
		}
		failing[r.pod.UID] = struct{}{}
		if !wasFailing {
			log.G(ctx).WithError(r.err).Warnf("virtualization group stats failing for pod %s/%s; skipping until it recovers", r.pod.Namespace, r.pod.Name)
		}
	}
	if listErr == nil {
		// A failed pod list yields no reliable failure set; keep the prior one so a
		// transient lister error does not spuriously reset edge state.
		p.failingPods = failing
	}

	return &s, nil
}

// GetMetricsResource gets the metrics for the node, including running pods
func (p *MacOSVZPodMetricsProvider) GetMetricsResource(ctx context.Context) (mf []*dto.MetricFamily, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZPodMetricsProvider.GetMetricsResource")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received GetMetricsResource request")

	statsSummary, err := p.GetStatsSummary(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching MetricsResource: %w", err)
	}

	registry := compbasemetrics.NewKubeRegistry()
	registry.CustomMustRegister(collectors.NewKubeletResourceMetricsCollector(statsSummary))

	metricFamily, err := registry.Gather()
	if err != nil {
		return nil, fmt.Errorf("error gathering metrics: %w", err)
	}

	return metricFamily, nil
}
