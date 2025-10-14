package client

import (
	"context"
	"io"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// VirtualizationGroup represents a group of macOS virtual machines and containers.
type VirtualizationGroup struct {
	MacOSVirtualMachine resource.VirtualMachine
	Containers          []resource.Container
}

// VzClientInterface defines the methods that a VzClient implementation should provide.
type VzClientInterface interface {
	CreateVirtualizationGroup(ctx context.Context, pod *corev1.Pod, serviceAccountToken string, configMaps map[string]*corev1.ConfigMap, creds resource.RegistryCredentialStore) error
	DeleteVirtualizationGroup(ctx context.Context, namespace, name string, gracePeriod int64) error
	GetVirtualizationGroup(ctx context.Context, namespace, name string) (*VirtualizationGroup, error)
	GetVirtualizationGroupListResult(ctx context.Context) (map[types.NamespacedName]*VirtualizationGroup, error)
	GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error)
	ExecuteContainerCommand(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error
	AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error
	GetVirtualizationGroupStats(ctx context.Context, namespace, name string, containers []corev1.Container) ([]stats.ContainerStats, error)
}
