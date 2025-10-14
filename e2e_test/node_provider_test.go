package e2e_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/provider"

	docker "github.com/moby/moby/client"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

func setupNodeProvider(t *testing.T, kcl *kubernetes.Clientset, nodeName string, daemonEndpointPort int32) (context.CancelFunc, *nodeutil.Node, *kubernetes.Clientset) {
	t.Helper()

	baseCtx := log.WithLogger(t.Context(), log.L)
	ctx, cancel := context.WithCancel(baseCtx)

	platform, _, _, err := host.PlatformInformationWithContext(ctx)
	require.NoError(t, err)

	node, err := nodeutil.NewNode(
		nodeName,
		newProviderFactory(t, ctx, kcl, nodeName, platform, daemonEndpointPort),
		withClientset(kcl),
		withInstrumentedAuth(),
		withOptionalTLS(),
		withProviderRoutes(),
		withHTTPListenAddr(daemonEndpointPort),
		withNodeTaint(),
	)
	require.NoError(t, err)

	go func() {
		err := node.Run(ctx)
		assert.NoError(t, err)
	}()

	startupTimeout := 5 * time.Second
	assert.NoErrorf(t, node.WaitReady(ctx, startupTimeout), "error waiting for node to be ready: %v", err)

	return cancel, node, kcl
}

func newProviderFactory(t *testing.T, ctx context.Context, kcl *kubernetes.Clientset, nodeName, platform string, daemonEndpointPort int32) func(nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
	t.Helper()
	return func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartLogging(log.G(ctx).Infof)
		eventBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: kcl.CoreV1().Events(corev1.NamespaceAll)})
		eventRecorder := event.NewKubeEventRecorder(
			eventBroadcaster.NewRecorder(
				scheme.Scheme,
				corev1.EventSource{
					Component: provider.ComponentName,
					Host:      nodeName,
				},
			),
		)
		cachePath := t.TempDir()
		t.Logf("cachePath: %s", cachePath)

		dockerCl, err := docker.NewClientWithOpts(docker.WithHost(*dockerSocketPath))
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = dockerCl.Close()
		})

		vzClient := client.NewVzClientAPIs(ctx, eventRecorder, "", cachePath, dockerCl)

		providerConfig := provider.MacOSVZProviderConfig{
			NodeName:           nodeName,
			Platform:           platform,
			DaemonEndpointPort: daemonEndpointPort,

			K8sClient:     kcl,
			EventRecorder: eventRecorder,
			PodsLister:    cfg.Pods,
		}

		p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
		require.NoError(t, err)

		assert.NoError(t, p.ConfigureNode(ctx, cfg.Node))

		return p, nil, nil
	}
}

func withClientset(kcl *kubernetes.Clientset) func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		return nodeutil.WithClient(kcl)(cfg)
	}
}

func withInstrumentedAuth() func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(nodeutil.NoAuth(), cfg.Handler))
		return nil
	}
}

func withOptionalTLS() func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		if *certPath == "" || *keyPath == "" {
			return nil
		}
		return nodeutil.WithTLSConfig(nodeutil.WithKeyPairFromPath(*certPath, *keyPath), withCA)(cfg)
	}
}

func withProviderRoutes() func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		mux := http.NewServeMux()
		cfg.Handler = mux
		return nodeutil.AttachProviderRoutes(mux)(cfg)
	}
}

func withHTTPListenAddr(daemonEndpointPort int32) func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		cfg.HTTPListenAddr = fmt.Sprintf(":%d", daemonEndpointPort)
		return nil
	}
}

func withNodeTaint() func(*nodeutil.NodeConfig) error {
	return func(cfg *nodeutil.NodeConfig) error {
		taint := corev1.Taint{
			Key:    taintKey,
			Value:  taintValue,
			Effect: corev1.TaintEffect(taintEffect),
		}
		cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, taint)
		return nil
	}
}

func withCA(cfg *tls.Config) error {
	if *clientCACert == "" {
		return nil
	}
	if err := nodeutil.WithCAFromPath(*clientCACert)(cfg); err != nil {
		return fmt.Errorf("error getting CA from path: %w", err)
	}
	cfg.ClientAuth = tls.NoClientCert
	return nil
}

func nodeOwnerReference(node *corev1.Node) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       node.Name,
		UID:        node.UID,
	}
}
