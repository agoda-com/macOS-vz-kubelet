package client_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	eventmocks "github.com/agoda-com/macOS-vz-kubelet/pkg/event/mocks"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	rm "github.com/agoda-com/macOS-vz-kubelet/pkg/resourcemanager"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// fakeContainersClient records CreateContainer params for assertions.
// Hand-rolled, not a generated mock: pkg/resourcemanager only typechecks on
// darwin and mockery is not part of `make generate`.
type fakeContainersClient struct {
	mu          sync.Mutex
	createdWith []rm.ContainerParams
}

func (f *fakeContainersClient) CreateContainer(_ context.Context, params rm.ContainerParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdWith = append(f.createdWith, params)
	return nil
}

func (f *fakeContainersClient) RemoveContainers(context.Context, string, string, int64) error {
	return nil
}

func (f *fakeContainersClient) GetContainers(context.Context, string, string) ([]resource.Container, error) {
	return nil, nil
}

func (f *fakeContainersClient) GetContainersListResult(context.Context) (map[types.NamespacedName][]resource.Container, error) {
	return map[types.NamespacedName][]resource.Container{}, nil
}

func (f *fakeContainersClient) GetContainerLogs(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeContainersClient) ExecInContainer(context.Context, string, string, string, []string, api.AttachIO) error {
	return nil
}

func (f *fakeContainersClient) AttachToContainer(context.Context, string, string, string, api.AttachIO) error {
	return nil
}

func (f *fakeContainersClient) IsContainerPresent(context.Context, string, string, string) bool {
	return false
}

func (f *fakeContainersClient) GetContainerStats(context.Context, string, string, string) (stats.ContainerStats, error) {
	return stats.ContainerStats{}, nil
}

var _ rm.ContainersClient = (*fakeContainersClient)(nil)

// permissiveEventRecorder returns a recorder that tolerates (but never requires)
// every event the create path can emit. The wiring tests give container[0] valid
// macOS resources so admission accepts the pod; the macOS goroutine then reaches
// CreateVirtualMachine, which fires PullingImage synchronously and spawns a detached
// download goroutine emitting further events. The strict default recorder fails on
// any unexpected call, so allow them all with .Maybe() to keep the sidecar-mapping
// assertions decoupled from the macOS-VM path (which needs live cgo to succeed).
func permissiveEventRecorder(t *testing.T) *eventmocks.EventRecorder {
	t.Helper()
	er := eventmocks.NewEventRecorder(t)
	er.EXPECT().PullingImage(mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().PulledImage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().BackOffPullImage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedToPullImage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedToResolveImagePullSecrets(mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedToValidateOCI(mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().StartedContainer(mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedToStartContainer(mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedPostStartProbe(mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	er.EXPECT().FailedPostStartHook(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	return er
}

func TestGetVirtualizationGroupStats_MissingVM_NoPanic(t *testing.T) {
	ctx := context.Background()
	er := eventmocks.NewEventRecorder(t)

	// nil docker client leaves ContainerClient unset (NewDockerClient errors on a
	// nil client).
	c := client.NewVzClientAPIs(ctx, er, "", t.TempDir(), nil)

	// Proves the stats path does not panic for a missing VM with a nil container
	// client. It does NOT exercise the sidecar-loop nil-guard: GetVirtualMachineStats
	// errors first for a missing VM and returns before the guard. The guard is
	// structural safety for a present VM with sidecars and a nil container client.
	containers := []corev1.Container{
		{Name: "macos"},
		{Name: "sidecar"},
	}
	assert.NotPanics(t, func() {
		_, err := c.GetVirtualizationGroupStats(ctx, "default", "missing", containers)
		assert.Error(t, err)
	})
}

// validMacOSResources is a known-good macOS container[0] resource block: an
// integer CPU and a fixed memory request that clear vm.ValidateCPUCount /
// vm.ValidateMemorySize so admission accepts the spec.
func validMacOSResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resourceapi.MustParse("1"),
		corev1.ResourceMemory: resourceapi.MustParse("1Gi"),
	}}
}

func TestCreateVirtualizationGroup_RejectsInvalidMacOSResources(t *testing.T) {
	ctx := context.Background()
	er := eventmocks.NewEventRecorder(t)

	// Single macOS container, nil ContainerClient. No cpu request -> cpu=0,
	// which vm.ValidateCPUCount rejects.
	c := client.NewVzClientAPIs(ctx, er, "", t.TempDir(), nil)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "macos", Image: "localhost/macos:latest"},
			},
		},
	}

	creds := resource.NewRegistryCredentialStore(nil)

	err := c.CreateVirtualizationGroup(ctx, pod, "", nil, creds)
	assert.True(t, errdefs.IsInvalidInput(err), "want InvalidInput, got %v", err)

	_, getErr := c.GetVirtualizationGroup(ctx, "default", "p")
	assert.True(t, errdefs.IsNotFound(getErr), "want NotFound (nothing registered), got %v", getErr)
}

func TestCreateVirtualizationGroup_RejectsUnparseableImageReference(t *testing.T) {
	ctx := context.Background()
	er := eventmocks.NewEventRecorder(t)

	// Single macOS container, valid cpu/mem, nil ContainerClient. The image is a
	// bare ref with no registry domain, which remote.NewRepository rejects.
	c := client.NewVzClientAPIs(ctx, er, "", t.TempDir(), nil)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "macos", Image: "busybox:1.37.0", Resources: validMacOSResources()},
			},
		},
	}

	creds := resource.NewRegistryCredentialStore(nil)

	err := c.CreateVirtualizationGroup(ctx, pod, "", nil, creds)
	// Pre-fix this returned success and registered the VM (the bad ref only
	// surfaced async in the downloader).
	assert.True(t, errdefs.IsInvalidInput(err), "want InvalidInput, got %v", err)

	_, getErr := c.GetVirtualizationGroup(ctx, "default", "p")
	assert.True(t, errdefs.IsNotFound(getErr), "want NotFound (nothing registered), got %v", getErr)
}

func TestCreateVirtualizationGroup_InvalidMacOSDoesNotCreateSidecars(t *testing.T) {
	ctx := context.Background()
	er := eventmocks.NewEventRecorder(t)
	fake := &fakeContainersClient{}

	// macOS container[0] with no cpu request (cpu=0, rejected) plus a sidecar.
	// nil docker client at construction, fake wired directly so a passing
	// admission would reach it; an invalid macOS spec must short-circuit first.
	c := client.NewVzClientAPIs(ctx, er, "", t.TempDir(), nil)
	c.ContainerClient = fake

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "macos", Image: "localhost/macos:latest"},
				{Name: "sidecar", Image: "localhost/sidecar:latest"},
			},
		},
	}

	creds := resource.NewRegistryCredentialStore(nil)

	err := c.CreateVirtualizationGroup(ctx, pod, "", nil, creds)
	assert.True(t, errdefs.IsInvalidInput(err), "want InvalidInput, got %v", err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Pre-fix the sidecar goroutine ran regardless of the macOS path and recorded 1.
	assert.Empty(t, fake.createdWith, "sidecar must not be created when macOS container[0] is invalid")
}

func TestCreateVirtualizationGroup_AllowsUnqualifiedSidecarImageRef(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	er := permissiveEventRecorder(t)
	fake := &fakeContainersClient{}

	// Valid macOS container[0]; sidecar uses a bare Docker-Hub ref. The Linux
	// backend expands short names, so admission must NOT reject it (oras would).
	c := client.NewVzClientAPIs(ctx, er, "", t.TempDir(), nil)
	c.ContainerClient = fake

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "macos", Image: "localhost/macos:latest", Resources: validMacOSResources()},
				{Name: "sidecar", Image: "redis:7"},
			},
		},
	}

	creds := resource.NewRegistryCredentialStore(nil)

	err := c.CreateVirtualizationGroup(ctx, pod, "", nil, creds)
	assert.False(t, errdefs.IsInvalidInput(err), "bare sidecar ref must not be rejected at admission, got %v", err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.createdWith, 1)
	assert.Equal(t, "sidecar", fake.createdWith[0].Name)
}
