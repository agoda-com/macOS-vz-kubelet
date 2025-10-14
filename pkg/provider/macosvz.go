package provider

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/metrics"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

const (
	ComponentName = "macos-vz-kubelet"

	// Timeout for deleting VZ group on failure.
	DefaultDeleteVZGroupGracePeriodSeconds int64 = 10
)

type MacOSVZProviderConfig struct {
	NodeName           string
	Platform           string
	InternalIP         string
	DaemonEndpointPort int32

	K8sClient     kubernetes.Interface
	EventRecorder event.EventRecorder
	PodsLister    corev1listers.PodLister
}

type MacOSVZProvider struct {
	vzClient  client.VzClientInterface
	k8sClient kubernetes.Interface
	podLister corev1listers.PodLister

	eventRecorder event.EventRecorder

	nodeName           string
	nodeIPAddress      string
	platform           string
	daemonEndpointPort int32

	*metrics.MacOSVZPodMetricsProvider
}

// NewMacOSVZProvider creates a new MacOSVZ provider.
func NewMacOSVZProvider(ctx context.Context, vzClient client.VzClientInterface, config MacOSVZProviderConfig) (p *MacOSVZProvider, err error) {
	if config.Platform != "darwin" {
		return nil, errdefs.InvalidInputf("platform type %q is not supported", config.Platform)
	}

	p = &MacOSVZProvider{}
	p.vzClient = vzClient

	p.k8sClient = config.K8sClient
	p.podLister = config.PodsLister

	p.nodeName = config.NodeName
	p.platform = config.Platform

	p.nodeIPAddress = config.InternalIP
	p.daemonEndpointPort = config.DaemonEndpointPort

	p.eventRecorder = config.EventRecorder

	p.MacOSVZPodMetricsProvider = metrics.NewMacOSVZPodMetricsProvider(p.nodeName, p.podLister, p.vzClient)
	return p, nil
}

var (
	errNotImplemented = fmt.Errorf("not implemented by MacOS provider")
)

// CreatePod takes a Kubernetes Pod and deploys it within the MacOS provider.
func (p *MacOSVZProvider) CreatePod(ctx context.Context, pod *corev1.Pod) (err error) {
	ctx = event.WithObjectRef(ctx, corev1.ObjectReference{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		UID:       pod.UID,
	})
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.CreatePod")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received CreatePod request")

	configMaps, serviceAccountToken, err := p.extractPodCredentials(ctx, pod)
	if err != nil {
		return err
	}

	registryCreds, err := p.resolveImagePullCredentials(ctx, pod)
	if err != nil {
		p.eventRecorder.FailedToResolveImagePullSecrets(ctx, err)
		return errdefs.AsInvalidInput(err)
	}

	return p.vzClient.CreateVirtualizationGroup(ctx, pod, serviceAccountToken, configMaps, registryCreds)
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider.
func (p *MacOSVZProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.UpdatePod")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received UpdatePod request")
	return errNotImplemented
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider.
func (p *MacOSVZProvider) DeletePod(ctx context.Context, pod *corev1.Pod) (err error) {
	ctx = event.WithObjectRef(ctx, corev1.ObjectReference{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		UID:       pod.UID,
	})
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.DeletePod")
	defer span.End()
	log.G(ctx).Debug("Received DeletePod request")

	// Execute delete request in go routine to avoid blocking the virtual kubelet thread
	go p.handleDeletePod(ctx, pod)

	return nil
}

func (p *MacOSVZProvider) handleDeletePod(ctx context.Context, pod *corev1.Pod) {
	var err error
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.handleDeletePod")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	gracePeriod := int64(0)
	if pod.DeletionGracePeriodSeconds != nil {
		gracePeriod = *pod.DeletionGracePeriodSeconds
	}

	canDeleteFast := true
	// there is no reason to run pre-stop hooks if grace period is 0 or pod is not running
	if pod.Status.Phase == corev1.PodRunning && gracePeriod > 0 {
		hookErr := p.handlePreStopHooks(ctx, pod, gracePeriod)
		// If pre-stop hooks failed, we should not delete the pod from the provider
		// and let the kubelet handle the pod deletion after the grace period
		// for pod event visibility.
		canDeleteFast = hookErr == nil
	}

	err = p.vzClient.DeleteVirtualizationGroup(ctx, pod.Namespace, pod.Name, gracePeriod)
	if err != nil {
		log.G(ctx).WithError(err).Error("Failed to delete virtualization group")
		return
	}

	if !canDeleteFast {
		return
	}

	// If we successfully drained virtualization group from the provider,
	// we can take upon ourselves to delete the pod from k8s before original grace period ended.
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: new(int64),
	}
	err = p.k8sClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, deleteOptions)
	if err != nil {
		log.G(ctx).WithError(err).Warn("Failed to delete pod from k8s")
	}
}

// handlePreStopHooks handles the pre-stop hooks for the containers in the pod.
func (p *MacOSVZProvider) handlePreStopHooks(ctx context.Context, pod *corev1.Pod, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.handlePreStopHooks")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	g := errgroup.Group{}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(gracePeriod)*time.Second)
	defer cancel()

	discardingExec := node.DiscardingExecIO()
	for _, container := range pod.Spec.Containers {
		if lifecycle := container.Lifecycle; lifecycle != nil && lifecycle.PreStop != nil && lifecycle.PreStop.Exec != nil {
			containerName := container.Name
			command := lifecycle.PreStop.Exec.Command
			g.Go(func() error {
				if err := p.vzClient.ExecuteContainerCommand(ctx, pod.Namespace, pod.Name, containerName, command, discardingExec); err != nil {
					p.eventRecorder.FailedPreStopHook(ctx, containerName, command, err)
					return err
				}
				return nil
			})
		}
	}

	return g.Wait()
}

// GetPod retrieves a pod by name from the provider (can be cached).
func (p *MacOSVZProvider) GetPod(ctx context.Context, namespace, name string) (pod *corev1.Pod, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.GetPod")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received GetPod request")

	vg, err := p.vzClient.GetVirtualizationGroup(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	pod, err = p.virtualizationGroupToPod(ctx, vg, namespace, name)
	if err != nil {
		// If error here occurs, usually its related to the pod being told to be forgotten,
		// e.g. force deletion or specific deletion cases where kubelet doesnt have opportunity to respond.
		// For now, we just delete the VM and return nil (providing client didn't fail).
		return nil, p.vzClient.DeleteVirtualizationGroup(ctx, namespace, name, DefaultDeleteVZGroupGracePeriodSeconds)
	}

	return pod, nil
}

// GetPodStatus retrieves the status of a pod by name from the provider.
func (p *MacOSVZProvider) GetPodStatus(ctx context.Context, namespace, name string) (ps *corev1.PodStatus, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.GetPodStatus")
	logger := log.G(ctx)
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	logger.Debug("Received GetPodStatus request")

	vg, err := p.vzClient.GetVirtualizationGroup(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	pod, err := p.podLister.Pods(namespace).Get(name)
	if err != nil {
		return nil, err
	}

	ps = p.buildPodStatus(ctx, vg, pod)
	if pod.DeletionTimestamp == nil && (ps.Phase == corev1.PodFailed || ps.Phase == corev1.PodSucceeded) {
		// If the pod is in a failed or succeeded state and is not scheduled for deletion,
		// it will never be queried for status again by design. We should delete it from
		// the provider to avoid any potential resource leaks.
		if err := p.vzClient.DeleteVirtualizationGroup(ctx, namespace, name, DefaultDeleteVZGroupGracePeriodSeconds); err != nil {
			logger.WithError(err).Debugf("Failed to force delete virtualization group for pod %s/%s", namespace, name)
		}
	}

	return ps, nil
}

// GetPods retrieves a list of all pods running on the provider (can be cached).
func (p *MacOSVZProvider) GetPods(ctx context.Context) (pods []*corev1.Pod, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.GetPods")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received GetPods request")

	vgs, err := p.vzClient.GetVirtualizationGroupListResult(ctx)
	if err != nil {
		return nil, err
	}

	for nm, vg := range vgs {
		pod, err := p.virtualizationGroupToPod(ctx, vg, nm.Namespace, nm.Name)
		if err != nil {
			return nil, err
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *MacOSVZProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (in io.ReadCloser, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.GetContainerLogs")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received GetContainerLogs request")
	return p.vzClient.GetContainerLogs(ctx, namespace, podName, containerName, opts)
}

// RunInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *MacOSVZProvider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.RunInContainer")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace":     namespace,
		"podName":       podName,
		"containerName": containerName,
		"cmd":           cmd,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received RunInContainer request")
	return p.vzClient.ExecuteContainerCommand(ctx, namespace, podName, containerName, cmd, attach)
}

// AttachToContainer attaches to the executing process of a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *MacOSVZProvider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.AttachToContainer")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received AttachToContainer request")
	return p.vzClient.AttachToContainer(ctx, namespace, podName, containerName, attach)
}

// PortForward forwards a local port to a port on the pod
func (p *MacOSVZProvider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSVZProvider.PortForward")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	log.G(ctx).Debug("Received PortForward request")
	return errNotImplemented
}
