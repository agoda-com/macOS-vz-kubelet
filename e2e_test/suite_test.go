package e2e_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/distribution/reference"
	"github.com/mitchellh/go-homedir"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	docker "github.com/moby/moby/client"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

type providerSuite struct {
	cancel     context.CancelFunc
	node       *nodeutil.Node
	kube       *kubernetes.Clientset
	restConfig *rest.Config
	registry   registryFixture
	namespace  string
}

func newProviderSuite(t *testing.T) *providerSuite {
	t.Helper()

	if *namespace == "" {
		t.Skip("namespace flag or POD_NAMESPACE env var must be provided for e2e tests")
	}

	home, err := homedir.Dir()
	require.NoError(t, err)

	kubeconfigPath := filepath.Join(home, ".kube", "config")
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	require.NoError(t, err)

	kcl, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	logger := logrus.StandardLogger()
	logger.SetLevel(logrus.InfoLevel)
	log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))

	cancel, node, kcl := setupNodeProvider(t, kcl, *nodeName, daemonEndpointPort)

	suite := &providerSuite{
		cancel:     cancel,
		node:       node,
		kube:       kcl,
		restConfig: restConfig,
		namespace:  *namespace,
	}
	suite.registerNodeCleanup(t)

	registryDockerClient, err := docker.NewClientWithOpts(docker.WithHost(*dockerSocketPath), docker.WithAPIVersionNegotiation())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = registryDockerClient.Close()
	})

	registry, err := ensureLocalRegistry(t, registryDockerClient, *registryUsername, *registryPassword)
	require.NoError(t, err)
	suite.registry = registry

	return suite
}

func (s *providerSuite) registerNodeCleanup(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		s.cancel()
		<-s.node.Done()
		assert.NoError(t, s.node.Err(), "node should shutdown without error")

		zero := int64(0)
		err := s.kube.CoreV1().Nodes().Delete(context.Background(), *nodeName, metav1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		require.NoError(t, err)
	})
}

func (s *providerSuite) uploadMacOSImageIfRequested(t *testing.T) {
	t.Helper()

	if *macOSImageDir == "" {
		t.Skip("macos-image-dir flag not provided")
	}

	pushCtx, cancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer cancel()

	require.NoError(t, pushMacOSImageToRegistry(pushCtx, s.registry, *macOSImageDir, *macOSImage))
}

func (s *providerSuite) registryHost(t *testing.T) string {
	t.Helper()

	namedImage, err := reference.ParseNormalizedNamed(*macOSImage)
	require.NoError(t, err, "invalid macOS image reference")
	return reference.Domain(namedImage)
}

func (s *providerSuite) ensureNamespace(t *testing.T) {
	t.Helper()

	_, err := s.kube.CoreV1().Namespaces().Get(t.Context(), s.namespace, metav1.GetOptions{})
	if err == nil {
		return
	}
	if !apierrors.IsNotFound(err) {
		require.NoError(t, err, "failed to get namespace")
		return
	}

	_, err = s.kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: s.namespace},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create namespace")
}

func (s *providerSuite) createRegistrySecret(t *testing.T, ownerRef metav1.OwnerReference, registryHost string) string {
	t.Helper()

	secretName := fmt.Sprintf("macos-vz-registry-secret-%d", time.Now().Unix())
	dockerConfigData, err := buildDockerConfigJSON(registryHost, s.registry.Username, s.registry.Password)
	require.NoError(t, err, "failed to build docker config json")

	_, err = s.kube.CoreV1().Secrets(s.namespace).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       s.namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerConfigData,
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create image pull secret")

	return secretName
}

func (s *providerSuite) newPod(ownerRef metav1.OwnerReference, secretName string) *corev1.Pod {
	gracePeriod := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("macos-vz-kubelet-e2e-test-pod-%d", time.Now().Unix()),
			Namespace:       s.namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &gracePeriod,
			Containers: []corev1.Container{
				{
					Name:  "macos",
					Image: *macOSImage,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("12Gi"),
						},
					},
					Env: []corev1.EnvVar{
						{
							Name:  "PROOF_FILE_PATH",
							Value: macosProofFilePath,
						},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"/bin/bash", "-c", "echo \"macos postStart executed\" > $PROOF_FILE_PATH"},
							},
						},
					},
				},
				{
					Name:  "busybox",
					Image: *busyboxImage,
					Command: []string{
						"sh",
						"-c",
						"trap : TERM INT; sleep infinity & wait",
					},
					Env: []corev1.EnvVar{
						{
							Name:  "PROOF_FILE_PATH",
							Value: busyboxProofFilePath,
						},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo 'busybox postStart executed' > $PROOF_FILE_PATH"},
							},
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/os": "darwin",
			},
			Tolerations: []corev1.Toleration{
				{
					Key:      taintKey,
					Operator: corev1.TolerationOpEqual,
					Value:    taintValue,
					Effect:   corev1.TaintEffect(taintEffect),
				},
			},
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: secretName,
				},
			},
		},
	}
}

func (s *providerSuite) createPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	createdPod, err := s.kube.CoreV1().Pods(s.namespace).Create(t.Context(), pod, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create pod")
	return createdPod
}

func (s *providerSuite) waitForPodReady(t *testing.T, podName string) *corev1.Pod {
	t.Helper()

	var observed *corev1.Pod
	pollCtx, pollCancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer pollCancel()

	err := wait.PollUntilContextCancel(pollCtx, *podCreationPollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := s.kube.CoreV1().Pods(s.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		t.Logf("Pod phase: %s", pod.Status.Phase)

		switch pod.Status.Phase {
		case corev1.PodRunning:
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					t.Log("Pod is ready")
					observed = pod
					return true, nil
				}
			}
			return false, nil
		case corev1.PodPending:
			return false, nil
		default:
			return false, fmt.Errorf("pod entered unexpected phase %s, expected Pending or Running", pod.Status.Phase)
		}
	})
	require.NoError(t, err, "failed to get pod status")
	return observed
}

func (s *providerSuite) waitForPostStartProof(t *testing.T, podName, containerName, proofPath, expected string) string {
	t.Helper()

	var output string
	pollCtx, pollCancel := context.WithTimeout(t.Context(), client.PostStartCommandTimeout)
	defer pollCancel()

	err := wait.PollUntilContextCancel(pollCtx, postStartPollInterval, true, func(ctx context.Context) (bool, error) {
		stdout, stderr, err := s.execContainer(ctx, podName, containerName, []string{"/bin/sh", "-c", fmt.Sprintf("if [ -s %q ]; then cat %q; else exit 1; fi", proofPath, proofPath)})
		if err != nil {
			var exitErr utilexec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitStatus() != 0 {
				t.Logf("waiting for proof file in %s container: %v (stderr: %s)", containerName, err, stderr)
				return false, nil
			}
			return false, err
		}

		output = strings.TrimSpace(stdout)
		if strings.Contains(output, expected) {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err, "failed to verify %s postStart file", containerName)

	return output
}

func (s *providerSuite) execContainer(ctx context.Context, podName, containerName string, command []string) (string, string, error) {
	req := s.kube.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(s.namespace).
		SubResource("exec").
		Param("container", containerName)

	for _, c := range command {
		req.Param("command", c)
	}
	req.Param("stdout", "true").Param("stderr", "true")

	exec, err := remotecommand.NewSPDYExecutor(s.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), stderr.String(), err
	}

	return stdout.String(), stderr.String(), nil
}

func (s *providerSuite) statsSummary(t *testing.T) statsv1alpha1.Summary {
	t.Helper()

	scheme := "http"
	httpClient := &http.Client{}
	if *certPath != "" && *keyPath != "" {
		scheme = "https"
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	statsURL := fmt.Sprintf("%s://localhost:%d/stats/summary", scheme, daemonEndpointPort)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, statsURL, nil)
	require.NoError(t, err, "failed to create http request for stats")

	resp, err := httpClient.Do(req)
	require.NoError(t, err, "failed to get stats summary")
	defer func() {
		require.NoError(t, resp.Body.Close(), "failed to close stats summary response body")
	}()

	require.Equal(t, http.StatusOK, resp.StatusCode, "stats summary request failed with non-200 status")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "failed to read stats summary response body")

	var summary statsv1alpha1.Summary
	err = json.Unmarshal(body, &summary)
	require.NoError(t, err, "failed to unmarshal stats summary")

	return summary
}

func (s *providerSuite) deletePod(t *testing.T, podName string) {
	t.Helper()

	deleteGracePeriod := int64(15)
	err := s.kube.CoreV1().Pods(s.namespace).Delete(t.Context(), podName, metav1.DeleteOptions{
		GracePeriodSeconds: &deleteGracePeriod,
	})
	require.NoError(t, err, "failed to delete pod")

	pollCtx, pollCancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer pollCancel()
	err = wait.PollUntilContextCancel(pollCtx, *podCreationPollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := s.kube.CoreV1().Pods(s.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				t.Log("Pod deleted successfully")
				return true, nil
			}
			return false, err
		}
		t.Logf("Waiting for pod %s to be deleted", podName)
		return false, nil
	})
	require.NoError(t, err, "error waiting for pod to be deleted")
}

func (s *providerSuite) getNode(t *testing.T) *corev1.Node {
	t.Helper()

	knode, err := s.kube.CoreV1().Nodes().Get(t.Context(), *nodeName, metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")
	return knode
}
