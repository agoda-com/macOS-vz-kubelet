package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resourcemanager"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/virtual-kubelet/virtual-kubelet/log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultPods is the default number of pods that can be run on a node.
	// Limiting number of pods to Apple Virtualization.framework internal limit
	DefaultPods = resourcemanager.MaxVirtualMachines

	// LabelCPUModelName is the label name for the CPU model name
	LabelCPUModelName = "feature.node.kubernetes.io/cpu-model.name"
)

// ConfigureNode takes a Kubernetes node object and applies provider specific configurations to the object.
func (p *MacOSVZProvider) ConfigureNode(ctx context.Context, n *corev1.Node) error {
	capacity, err := getNodeCapacity(ctx)
	if err != nil {
		return fmt.Errorf("error getting node capacity: %w", err)
	}
	n.Status.Capacity = capacity
	n.Status.Allocatable = capacity

	n.Status.Conditions = getNodeConditions()

	addr, err := p.nodeAddresses(ctx)
	if err != nil {
		return fmt.Errorf("error getting node addresses: %w", err)
	}
	n.Status.Addresses = addr
	n.Status.DaemonEndpoints = p.nodeDaemonEndpoints()

	hostInfo, err := host.InfoWithContext(ctx)
	if err != nil {
		return fmt.Errorf("error getting host information: %w", err)
	}
	n.Status.NodeInfo.MachineID = hostInfo.HostID
	n.Status.NodeInfo.KernelVersion = hostInfo.KernelVersion
	n.Status.NodeInfo.OSImage = hostInfo.OS + " " + hostInfo.PlatformVersion
	n.Status.NodeInfo.ContainerRuntimeVersion = "vz://" + hostInfo.PlatformVersion
	n.Status.NodeInfo.OperatingSystem = hostInfo.Platform
	n.Status.NodeInfo.Architecture = hostInfo.KernelArch

	n.Labels[corev1.LabelNodeExcludeBalancers] = "true"

	// report both old and new styles of OS and arch information
	os := strings.ToLower(hostInfo.Platform)
	n.Labels[corev1.LabelOSStable] = os
	n.Labels[corev1.LabelArchStable] = hostInfo.KernelArch

	// assign cpu model label if available
	c, err := cpu.InfoWithContext(ctx)
	if err == nil && len(c) > 0 {
		n.Labels[LabelCPUModelName] = utils.SanitizeAppleCPUModelForK8sLabel(c[0].ModelName)
	} else {
		log.G(ctx).WithError(err).Warn("Error getting cpu information, skipping cpu model label")
	}

	return nil
}

func (p *MacOSVZProvider) nodeAddresses(ctx context.Context) (addr []corev1.NodeAddress, err error) {
	if p.nodeIPAddress == "" {
		p.nodeIPAddress, err = retrieveNodeIPAddress(ctx)
		if err != nil {
			return nil, err
		}
	}

	return []corev1.NodeAddress{
		{
			Type:    corev1.NodeInternalIP,
			Address: p.nodeIPAddress,
		},
		{
			Type:    corev1.NodeHostName,
			Address: p.nodeName,
		},
	}, nil
}

// nodeDaemonEndpoints returns NodeDaemonEndpoints for the node status within Kubernetes.
func (p *MacOSVZProvider) nodeDaemonEndpoints() corev1.NodeDaemonEndpoints {
	return corev1.NodeDaemonEndpoints{
		KubeletEndpoint: corev1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}

// getNodeCapacity returns a resource list containing the capacity limits set for MacOSVZ.
func getNodeCapacity(ctx context.Context) (corev1.ResourceList, error) {
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return corev1.ResourceList{}, err
	}
	memory := *resource.NewQuantity(int64(v.Total), resource.BinarySI)

	c, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		return corev1.ResourceList{}, err
	}
	cpu := *resource.NewQuantity(int64(c), resource.DecimalSI)

	d, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		return corev1.ResourceList{}, err
	}
	ephemeralStorage := *resource.NewQuantity(int64(d.Total), resource.BinarySI)

	pods := *resource.NewQuantity(DefaultPods, resource.DecimalSI)

	return corev1.ResourceList{
		corev1.ResourceCPU:              cpu,
		corev1.ResourceMemory:           memory,
		corev1.ResourceEphemeralStorage: ephemeralStorage,
		corev1.ResourcePods:             pods,
	}, nil
}

// getNodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status within Kubernetes.
func getNodeConditions() []corev1.NodeCondition {
	// TODO: Implement proper node conditions with memory and disk health checks
	return []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               corev1.NodeMemoryPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               corev1.NodeDiskPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               corev1.NodeNetworkUnavailable,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

// retrieveNodeIPAddress retrieves the IP address of the node.
func retrieveNodeIPAddress(ctx context.Context) (string, error) {
	ifs, err := psnet.InterfacesWithContext(ctx)
	if err != nil {
		return "", err
	}

	return netutil.GetActiveInterface(ifs)
}
