package provider_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	clientmock "github.com/agoda-com/macOS-vz-kubelet/pkg/client/mocks"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/provider"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func SetupEnvTest(tb testing.TB) *rest.Config {
	tb.Helper()

	if testing.Short() || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		tb.SkipNow()
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoErrorf(tb, err, "start envtest: %v", err)

	tb.Cleanup(func() {
		if err := env.Stop(); err != nil {
			tb.Log("stop envtest: ", err)
		}
	})

	return cfg
}

func TestNodeConfiguration(t *testing.T) {
	nodeName := "test-node"
	daemonEndpointPort := int32(10250)
	ctx, cancel, node, kcl := setupNodeProvider(t, nodeName, "", daemonEndpointPort)

	// get node from k8s
	knode, err := kcl.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	t.Run("Capacity", func(t *testing.T) {
		assert.EqualValues(t, knode.Status.Capacity, knode.Status.Allocatable, "capacity and allocatable should be equal")

		rcpu := knode.Status.Capacity[corev1.ResourceCPU]
		rmem := knode.Status.Capacity[corev1.ResourceMemory]
		rstorage := knode.Status.Capacity[corev1.ResourceEphemeralStorage]
		rpods := knode.Status.Capacity[corev1.ResourcePods]

		c, err := cpu.CountsWithContext(ctx, true)
		require.NoError(t, err)
		assert.Equal(t, rcpu.Value(), int64(c), "cpu capacity should be equal to the node cpu capacity")

		v, err := mem.VirtualMemoryWithContext(ctx)
		require.NoError(t, err)
		assert.Equal(t, rmem.Value(), int64(v.Total), "memory capacity should be equal to the node memory capacity")

		d, err := disk.UsageWithContext(ctx, "/")
		require.NoError(t, err)
		assert.Equal(t, rstorage.Value(), int64(d.Total), "storage capacity should be equal to the node storage capacity")

		maxPodsAllowed := int64(provider.DefaultPods)
		assert.Equal(t, rpods.Value(), maxPodsAllowed, "pods capacity should be equal to the node pods capacity")
	})

	t.Run("Conditions", func(t *testing.T) {
		conditions := knode.Status.Conditions

		// Should have all expected k8s conditions as normal: Ready, MemoryPressure, DiskPressure, NetworkUnavailable
		assert.Len(t, conditions, 4, "node should have 4 default conditions")
		assert.True(t, containsConditionWithStatus(conditions, corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}), "node should have Ready condition")
		assert.True(t, containsConditionWithStatus(conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}), "node should have MemoryPressure condition")
		assert.True(t, containsConditionWithStatus(conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse}), "node should have DiskPressure condition")
		assert.True(t, containsConditionWithStatus(conditions, corev1.NodeCondition{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionFalse}), "node should have NetworkUnavailable condition")
	})

	t.Run("Addresses", func(t *testing.T) {
		ifs, err := psnet.InterfacesWithContext(ctx)
		require.NoError(t, err)
		ip, err := netutil.GetActiveInterface(ifs)
		require.NoError(t, err)

		assert.Equal(t, knode.Status.Addresses, []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: ip,
			},
			{
				Type:    corev1.NodeHostName,
				Address: nodeName,
			},
		}, "node should have internal IP address")
		assert.Equal(t, knode.Status.DaemonEndpoints, corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{
				Port: daemonEndpointPort,
			},
		}, "node should have daemon endpoint")
	})

	t.Run("NodeInfo", func(t *testing.T) {
		hostInfo, err := host.InfoWithContext(ctx)
		require.NoError(t, err)

		assert.Equal(t, knode.Status.NodeInfo.MachineID, hostInfo.HostID, "node machine ID should be equal to the host machine ID")
		assert.Equal(t, knode.Status.NodeInfo.KernelVersion, hostInfo.KernelVersion, "node kernel version should be equal to the host kernel version")
		assert.Equal(t, knode.Status.NodeInfo.OSImage, hostInfo.OS+" "+hostInfo.PlatformVersion, "node OS image should be equal to the host OS image")
		assert.Equal(t, knode.Status.NodeInfo.ContainerRuntimeVersion, "vz://"+hostInfo.PlatformVersion, "node container runtime version should be equal to the host container runtime version")
		assert.Equal(t, knode.Status.NodeInfo.OperatingSystem, hostInfo.Platform, "node operating system should be equal to the host operating system")
		assert.Equal(t, knode.Status.NodeInfo.Architecture, hostInfo.KernelArch, "node architecture should be equal to the host architecture")
	})

	t.Run("Labels", func(t *testing.T) {
		labels := knode.Labels

		// external lb exclusion labels
		assert.Equal(t, labels[corev1.LabelNodeExcludeBalancers], "true", "node should have exclude from external load balancers label")

		// os and arch labels
		hostInfo, err := host.InfoWithContext(ctx)
		require.NoError(t, err)
		os := strings.ToLower(hostInfo.Platform)
		assert.Equal(t, labels[corev1.LabelOSStable], os, "node should have kubernetes os label")
		assert.Equal(t, labels[corev1.LabelArchStable], hostInfo.KernelArch, "node should have kubernetes arch label")

		// cpu model label
		c, err := cpu.InfoWithContext(ctx)
		require.NoError(t, err)
		assert.Equal(t, labels[provider.LabelCPUModelName], utils.SanitizeAppleCPUModelForK8sLabel(c[0].ModelName), "node should have cpu model label")

		// added on top labels
		assert.Equal(t, labels[corev1.LabelHostname], nodeName, "node should have hostname label")
		assert.Equal(t, labels["type"], "virtual-kubelet", "node should have type label")
		assert.Equal(t, labels["kubernetes.io/role"], "agent", "node should have role label")
	})

	// Stop the node
	cancel()
	<-node.Done()
	assert.NoError(t, node.Err(), "node should shutdown without error")
}

func TestNodeConfiguration_StaticIP(t *testing.T) {
	nodeName := "test-node"
	nodeIPAddress := "10.0.0.4"
	daemonEndpointPort := int32(10250)
	ctx, cancel, node, kcl := setupNodeProvider(t, nodeName, nodeIPAddress, daemonEndpointPort)

	// get node from k8s
	knode, err := kcl.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	assert.Equal(t, knode.Status.Addresses, []corev1.NodeAddress{
		{
			Type:    corev1.NodeInternalIP,
			Address: nodeIPAddress,
		},
		{
			Type:    corev1.NodeHostName,
			Address: nodeName,
		},
	})

	// Stop the node
	cancel()
	<-node.Done()
	assert.NoError(t, node.Err(), "node should shutdown without error")
}

// Helper function to setup Kubernetes client and node provider
func setupNodeProvider(t *testing.T, nodeName string, nodeIPAddress string, daemonEndpointPort int32) (context.Context, context.CancelFunc, *nodeutil.Node, *kubernetes.Clientset) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	platform, _, _, err := host.PlatformInformationWithContext(ctx)
	require.NoError(t, err)

	kcfg := SetupEnvTest(t)
	kcl, err := kubernetes.NewForConfig(kcfg)
	require.NoErrorf(t, err, "create client: %v", err)

	node, err := nodeutil.NewNode(
		nodeName,
		func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
			vzClient := clientmock.NewVzClientInterface(t)
			vzClient.On("GetVirtualizationGroupListResult", mock.Anything).Return(map[types.NamespacedName]*client.VirtualizationGroup{}, nil)

			require.NoError(t, err)

			providerConfig := provider.MacOSVZProviderConfig{
				NodeName:           nodeName,
				Platform:           platform,
				InternalIP:         nodeIPAddress,
				DaemonEndpointPort: daemonEndpointPort,
			}

			p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
			require.NoError(t, err)

			assert.NoError(t, p.ConfigureNode(ctx, cfg.Node))

			return p, nil, nil
		},
		func(cfg *nodeutil.NodeConfig) error {
			return nodeutil.WithClient(kcl)(cfg)
		},
	)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		errCh <- node.Run(ctx)
		select {
		case err := <-errCh:
			assert.NoError(t, err)
		case <-ctx.Done():
		}
	}()

	startupTimeout := 5 * time.Second
	assert.NoErrorf(t, node.WaitReady(ctx, startupTimeout), "error waiting for node to be ready: %v", err)

	return ctx, cancel, node, kcl
}

func containsConditionWithStatus(conditions []corev1.NodeCondition, condition corev1.NodeCondition) bool {
	for _, c := range conditions {
		if c.Type == condition.Type && c.Status == condition.Status {
			return true
		}
	}
	return false
}
