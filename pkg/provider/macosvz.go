package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	dto "github.com/prometheus/client_model/go"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type MacOSVZProvider struct {
	macOSClient client.VirtualMachineClient
	podsL       corev1listers.PodLister

	nodeName           string
	platform           string
	daemonEndpointPort int32

	cpu              resource.Quantity
	memory           resource.Quantity
	ephemeralStorage resource.Quantity
	pods             resource.Quantity

	machineID               string
	kernelVersion           string
	osImage                 string
	containerRuntimeVersion string
	operatingSystem         string
	architecture            string

	nodeutil.Provider
}

// NewMacOSVZProvider creates a new MacOSVZ provider.
func NewMacOSVZProvider(ctx context.Context, macOSClient client.VirtualMachineClient, podsL corev1listers.PodLister, nodeName string, platform string, daemonEndpointPort int32) (*MacOSVZProvider, error) {
	if platform != "darwin" {
		return nil, errdefs.InvalidInputf("platform type %q is not supported", platform)
	}

	var p MacOSVZProvider

	p.macOSClient = macOSClient
	p.podsL = podsL

	p.nodeName = nodeName
	p.platform = platform
	p.daemonEndpointPort = daemonEndpointPort

	if err := p.setupNodeCapacity(ctx); err != nil {
		return nil, err
	}

	if err := p.setupHostInfo(ctx); err != nil {
		return nil, err
	}

	return &p, nil
}

var (
	errNotImplemented = fmt.Errorf("not implemented by MacOS provider")
)

// CreatePod takes a Kubernetes Pod and deploys it within the MacOS provider.
func (p *MacOSVZProvider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	log.G(ctx).Infof("Received CreatePod request for %s/%s.", pod.Namespace, pod.Name)

	// TODO: better container selection for cases like gitlab runner
	rl := pod.Spec.Containers[0].Resources.Requests

	cpu, err := extractCPURequest(ctx, rl)
	if err != nil {
		return errdefs.AsInvalidInput(err)
	}
	memorySize, err := extractMemoryRequest(ctx, rl)
	if err != nil {
		return errdefs.AsInvalidInput(err)
	}

	return p.macOSClient.CreateVirtualMachine(ctx, pod.Namespace, pod.Name, cpu, uint64(memorySize))
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider.
func (p *MacOSVZProvider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	log.G(ctx).Infof("Received UpdatePod request for %s/%s.", pod.Namespace, pod.Name)

	return errNotImplemented
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider.
func (p *MacOSVZProvider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	log.G(ctx).Infof("Received DeletePod request for %s/%s.", pod.Namespace, pod.Name)

	return p.macOSClient.DeleteVirtualMachine(ctx, pod.Namespace, pod.Name)
}

// GetPod retrieves a pod by name from the provider (can be cached).
func (p *MacOSVZProvider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	log.G(ctx).Infof("Received GetPod request for %s/%s.", namespace, name)

	vm, err := p.macOSClient.GetVirtualMachine(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	// TODO: check if pod doesnt exist in k8s, will it reconcile?
	return p.virtualMachineToPod(ctx, vm, namespace, name)
}

// GetPodStatus retrieves the status of a pod by name from the provider.
func (p *MacOSVZProvider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	log.G(ctx).Infof("Received GetPodStatus request for %s/%s.", namespace, name)

	vm, err := p.macOSClient.GetVirtualMachine(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	return p.getPodStatusFromVirtualMachine(ctx, vm)
}

// GetPods retrieves a list of all pods running on the provider (can be cached).
func (p *MacOSVZProvider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	log.G(ctx).Info("Received GetPods request.")

	vms, err := p.macOSClient.GetVirtualMachineListResult(ctx)
	if err != nil {
		return nil, err
	}

	var pods []*v1.Pod
	for _, vm := range vms {
		pod, err := p.virtualMachineToPod(ctx, vm, vm.Namespace, vm.Name)
		if err != nil {
			return nil, err
		}
		pods = append(pods, pod)
	}

	return pods, nil
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *MacOSVZProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	log.G(ctx).Infof("Received GetContainerLogs request for %s/%s/%s.", namespace, podName, containerName)
	return nil, errNotImplemented
}

// RunInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *MacOSVZProvider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	log.G(ctx).Infof("Received RunInContainer request for %s/%s/%s.", namespace, podName, containerName)
	return errNotImplemented
}

// AttachToContainer attaches to the executing process of a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *MacOSVZProvider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	log.G(ctx).Infof("Received AttachToContainer request for %s/%s/%s.", namespace, podName, containerName)
	return errNotImplemented
}

// GetStatsSummary gets the stats for the node, including running pods
func (p *MacOSVZProvider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	log.G(ctx).Info("Received GetStatsSummary request.")
	return nil, errNotImplemented
}

// GetMetricsResource gets the metrics for the node, including running pods
func (p *MacOSVZProvider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	log.G(ctx).Info("Received GetMetricsResource request.")
	return nil, errNotImplemented
}

// PortForward forwards a local port to a port on the pod
func (p *MacOSVZProvider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	log.G(ctx).Infof("Received PortForward request for %s/%s:%d.", namespace, pod, port)
	return errNotImplemented
}
