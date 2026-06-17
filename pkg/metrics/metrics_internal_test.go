package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func runningPodWithUID(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      name,
			UID:       uid,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func internalListerWith(t *testing.T, pods ...*corev1.Pod) corev1listers.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range pods {
		require.NoError(t, indexer.Add(p))
	}
	return corev1listers.NewPodLister(indexer)
}

// Drives one pod through two failing scrapes then a recovery; asserts the UID is
// recorded on the rising edge, stays while failing, and is dropped on recovery.
func TestFailingPodsSetTransitions(t *testing.T) {
	bad := runningPodWithUID("pod-bad", types.UID("uid-bad"))

	vzClient := mocks.NewVzClientInterface(t)
	healthy := []stats.ContainerStats{{Name: "c0"}}

	// Scrape 1+2: fail. Scrape 3: recover.
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Return(nil, errors.New("boom")).Twice()
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Return(healthy, nil).Once()

	lister := internalListerWith(t, bad)
	p := NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	require.Empty(t, p.failingPods)

	_, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.Contains(t, p.failingPods, types.UID("uid-bad"), "rising edge records the UID")

	_, err = p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.Contains(t, p.failingPods, types.UID("uid-bad"), "still failing stays in the set")

	_, err = p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, p.failingPods, types.UID("uid-bad"), "recovery removes the UID")
}

// A pod that vanishes while failing is pruned (no recovery is observable for a gone
// pod), so the set does not leak entries under churn.
func TestFailingPodsPrunesVanishedPod(t *testing.T) {
	bad := runningPodWithUID("pod-gone", types.UID("uid-gone"))

	vzClient := mocks.NewVzClientInterface(t)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-gone", mock.Anything).
		Return(nil, errors.New("boom")).Once()

	lister := internalListerWith(t, bad)
	p := NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)

	_, err := p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	require.Contains(t, p.failingPods, types.UID("uid-gone"))

	// Pod removed from the lister; next scrape has no entry for it.
	emptyLister := internalListerWith(t)
	p.podLister = emptyLister

	_, err = p.GetStatsSummary(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, p.failingPods, types.UID("uid-gone"), "vanished pod pruned")
	assert.Empty(t, p.failingPods)
}

// A cancelled scrape must NOT mutate failingPods: the scrape is incomplete, so its
// (empty) failure set is unreliable. Pins the early-return invariant against refactor.
func TestFailingPodsUntouchedOnCancelledScrape(t *testing.T) {
	bad := runningPodWithUID("pod-bad", types.UID("uid-seeded"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vzClient := mocks.NewVzClientInterface(t)
	vzClient.EXPECT().
		GetVirtualizationGroupStats(mock.Anything, "ns", "pod-bad", mock.Anything).
		Run(func(_ context.Context, _ string, _ string, _ []corev1.Container) {
			cancel()
		}).
		Return(nil, context.Canceled)

	lister := internalListerWith(t, bad)
	p := NewMacOSVZPodMetricsProvider("test-node", lister, vzClient)
	p.failingPods[types.UID("uid-seeded")] = struct{}{}

	summary, err := p.GetStatsSummary(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Nil(t, summary)
	assert.Contains(t, p.failingPods, types.UID("uid-seeded"), "cancelled scrape must not reset the set")
}
