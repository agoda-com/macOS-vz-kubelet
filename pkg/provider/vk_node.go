package provider

import (
	"context"
	"net"
	"strings"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"

	"github.com/virtual-kubelet/virtual-kubelet/log"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultPods = 2
)

// ConfigureNode takes a Kubernetes node object and applies provider specific configurations to the object.
func (p *MacOSVZProvider) ConfigureNode(ctx context.Context, n *v1.Node) {
	capacity := p.capacity()
	n.Status.Capacity = capacity
	n.Status.Allocatable = capacity

	n.Status.Conditions = p.nodeConditions()
	n.Status.Addresses = p.nodeAddresses(ctx)
	n.Status.DaemonEndpoints = p.nodeDaemonEndpoints()

	n.Status.NodeInfo.MachineID = p.machineID
	n.Status.NodeInfo.KernelVersion = p.kernelVersion
	n.Status.NodeInfo.OSImage = p.osImage
	n.Status.NodeInfo.ContainerRuntimeVersion = p.containerRuntimeVersion
	n.Status.NodeInfo.OperatingSystem = p.operatingSystem
	n.Status.NodeInfo.Architecture = p.architecture

	n.ObjectMeta.Labels["alpha.service-controller.kubernetes.io/exclude-balancer"] = "true"
	n.ObjectMeta.Labels["node.kubernetes.io/exclude-from-external-load-balancers"] = "true"

	// report both old and new styles of OS and arch information
	os := strings.ToLower(p.operatingSystem)
	n.ObjectMeta.Labels["beta.kubernetes.io/os"] = os
	n.ObjectMeta.Labels["kubernetes.io/os"] = os
	n.ObjectMeta.Labels["beta.kubernetes.io/arch"] = p.architecture
	n.ObjectMeta.Labels["kubernetes.io/arch"] = p.architecture
}

// capacity returns a resource list containing the capacity limits set for MacOSVZ.
func (p *MacOSVZProvider) capacity() v1.ResourceList {
	resourceList := v1.ResourceList{
		v1.ResourceCPU:              p.cpu,
		v1.ResourceMemory:           p.memory,
		v1.ResourceEphemeralStorage: p.ephemeralStorage,
		v1.ResourcePods:             p.pods,
	}

	return resourceList
}

// nodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status within Kubernetes.
func (p *MacOSVZProvider) nodeConditions() []v1.NodeCondition {
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

// nodeAddresses returns the addresses for the node.
func (p *MacOSVZProvider) nodeAddresses(ctx context.Context) []v1.NodeAddress {
	ifs, err := psnet.InterfacesWithContext(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("Error getting network interfaces")
	}

	addr := ""
	for _, i := range ifs {
		// en0 is a default interface on Apple Silicon machines
		// for now, assuming that all machines provided are act as so
		if i.Name == "en0" {
			for _, a := range i.Addrs {
				ip, _, err := net.ParseCIDR(a.Addr)
				if err != nil {
					log.G(ctx).WithError(err).Error("Error parsing CIDR")
				}
				if ip.To4() != nil {
					addr = ip.String()
				}
			}
			break
		}
	}

	return []v1.NodeAddress{
		{
			Type:    v1.NodeInternalIP,
			Address: addr,
		},
		{
			Type:    v1.NodeHostName,
			Address: p.nodeName,
		},
	}
}

// nodeDaemonEndpoints returns NodeDaemonEndpoints for the node status within Kubernetes.
func (p *MacOSVZProvider) nodeDaemonEndpoints() v1.NodeDaemonEndpoints {
	return v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}

// setupNodeCapacity sets the capacity of the node based on the host's resources.
func (p *MacOSVZProvider) setupNodeCapacity(ctx context.Context) error {
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("Error getting memory capacity")
		return err
	}
	p.memory = *resource.NewQuantity(int64(v.Total), resource.BinarySI)

	c, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		log.G(ctx).WithError(err).Error("Error getting cpu capacity")
		return err
	}
	p.cpu = *resource.NewQuantity(int64(c), resource.DecimalSI)

	d, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		log.G(ctx).WithError(err).Error("Error getting disk capacity")
		return err
	}
	p.ephemeralStorage = *resource.NewQuantity(int64(d.Total), resource.BinarySI)

	p.pods = *resource.NewQuantity(defaultPods, resource.DecimalSI)

	return nil
}

func (p *MacOSVZProvider) setupHostInfo(ctx context.Context) error {
	info, err := host.InfoWithContext(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("Error getting host info")
		return err
	}

	p.machineID = info.HostID
	p.kernelVersion = info.KernelVersion
	p.osImage = info.OS + " " + info.PlatformVersion
	p.containerRuntimeVersion = "vz://" + info.PlatformVersion
	p.operatingSystem = info.Platform
	p.architecture = info.KernelArch

	return nil
}
