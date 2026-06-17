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

// macOSContainer builds the macOS container[0] for the e2e pods. postStartCommand is the
// exec postStart hook: the success pod uses a slow hook so the Ready=False gate window is
// observable, the failure pod one that exits non-zero.
func macOSContainer(postStartCommand []string) corev1.Container {
	return corev1.Container{
		Name:  "macos",
		Image: *macOSImage,
		// IfNotPresent reuses the shared image cache; PullAlways sets IgnoreImageCache=true
		// and re-pulls the ~137GB image every pod spawn.
		ImagePullPolicy: corev1.PullIfNotPresent,
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
					Command: postStartCommand,
				},
			},
		},
	}
}

// macOSPodSpec wraps the container(s) with the node selector, tolerations and image pull
// secret shared by every e2e pod.
func macOSPodSpec(secretName string, containers []corev1.Container) corev1.PodSpec {
	gracePeriod := int64(0)
	return corev1.PodSpec{
		TerminationGracePeriodSeconds: &gracePeriod,
		Containers:                    containers,
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
	}
}

func (s *providerSuite) newPod(ownerRef metav1.OwnerReference, secretName string) *corev1.Pod {
	// Slow hook keeps macos NotReady long enough for waitForPostStartGateThenReady to see
	// the gate window. sleep 8 stays under the 10s VZ_POSTSTART_TIMEOUT and writes the same
	// proof content, so exec-verify-macos-poststart-file is unaffected.
	macos := macOSContainer([]string{"/bin/bash", "-c", "sleep 8; echo \"macos postStart executed\" > $PROOF_FILE_PATH"})
	busybox := corev1.Container{
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
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("macos-vz-kubelet-e2e-test-pod-%d", time.Now().Unix()),
			Namespace:       s.namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: macOSPodSpec(secretName, []corev1.Container{macos, busybox}),
	}
}

// newFailingPostStartPod builds a minimal single-container macOS pod (no busybox sidecar)
// whose exec postStart hook exits non-zero, driving the pod to Failed.
func (s *providerSuite) newFailingPostStartPod(ownerRef metav1.OwnerReference, secretName string) *corev1.Pod {
	macos := macOSContainer([]string{"/bin/bash", "-c", "exit 1"})
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("macos-vz-kubelet-e2e-poststart-fail-pod-%d", time.Now().Unix()),
			Namespace:       s.namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: macOSPodSpec(secretName, []corev1.Container{macos}),
	}
}

func (s *providerSuite) createPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	createdPod, err := s.kube.CoreV1().Pods(s.namespace).Create(t.Context(), pod, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create pod")
	return createdPod
}

// containerStatusByName returns the named container's status, or nil if not reported yet.
func containerStatusByName(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// podReadyCondition reports whether the pod's PodReady condition is True.
func podReadyCondition(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitForPostStartGateThenReady asserts the postStart readiness gate. Gate window: while
// the exec hook runs, macos is Running (VM up, has IP) but Ready=false, Started=false and
// PodReady False. After the hook finishes it flips to Ready=true, Started=true, PodReady
// True. Requires both, with the gate window seen strictly before Ready. Polls at 1s so the
// short gate window is not missed (the 5s default interval would).
func (s *providerSuite) waitForPostStartGateThenReady(t *testing.T, podName string) {
	t.Helper()

	sawGate := false
	pollCtx, pollCancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer pollCancel()

	err := wait.PollUntilContextCancel(pollCtx, postStartPollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := s.kube.CoreV1().Pods(s.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return false, fmt.Errorf("pod entered unexpected phase %s before becoming Ready", pod.Status.Phase)
		}

		macos := containerStatusByName(pod, "macos")
		if macos == nil {
			t.Logf("Pod phase: %s (macos container status not reported yet)", pod.Status.Phase)
			return false, nil
		}

		ready := podReadyCondition(pod)
		started := macos.Started != nil && *macos.Started
		t.Logf("Pod phase: %s, macos Running=%t Ready=%t Started=%t PodReady=%t",
			pod.Status.Phase, macos.State.Running != nil, macos.Ready, started, ready)

		// Gate window: Running but hook not finished, so Ready/Started/PodReady all false.
		if macos.State.Running != nil && !macos.Ready && !started && !ready {
			sawGate = true
		}

		// Ready: hook finished; macos flips Ready+Started and PodReady is True.
		if macos.Ready && started && ready {
			require.True(t, sawGate, "macos container became Ready without first observing the postStart gate window (Running, Ready=false, Started=false, PodReady=false)")

			// Timestamp contract: Ready LastTransitionTime is stamped at hook finish, not VM
			// start. The hook sleeps 8s, so a correct binary lands Ready well after the
			// containers started; a stamp at VM start shows a near-zero delta.
			require.NotNil(t, macos.State.Running, "macos must be Running when Ready=true")
			// Baseline = the LATEST container Running.StartedAt. A buggy stamp tracks
			// lastUpdateTime, which the busybox sidecar can bump past the macos start, so
			// comparing only against macos could false-pass. busybox lookup is null-safe:
			// fall back to the macos start if it (or its Running state) is absent.
			latestStart := macos.State.Running.StartedAt.Time
			if busybox := containerStatusByName(pod, "busybox"); busybox != nil && busybox.State.Running != nil {
				if bs := busybox.State.Running.StartedAt.Time; bs.After(latestStart) {
					latestStart = bs
				}
			}
			var readyTransition time.Time
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady {
					readyTransition = cond.LastTransitionTime.Time
					break
				}
			}
			delta := readyTransition.Sub(latestStart)
			t.Logf("Ready LastTransitionTime - latest container Running.StartedAt = %s", delta)
			require.GreaterOrEqual(t, delta, 5*time.Second,
				"Ready LastTransitionTime (%s) must trail the latest container Running.StartedAt (%s) by the postStart hook duration; got delta %s (a stamp at VM start would be near zero)",
				readyTransition, latestStart, delta)

			t.Log("Pod is ready (postStart gate observed before Ready)")
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err, "failed to observe postStart gate then Ready")
	require.True(t, sawGate, "never observed the postStart gate window")
}

// waitForPodFailed polls until the pod reaches the Failed phase, returning the failed pod.
func (s *providerSuite) waitForPodFailed(t *testing.T, podName string) *corev1.Pod {
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
		case corev1.PodFailed:
			t.Log("Pod reached Failed phase")
			observed = pod
			return true, nil
		case corev1.PodSucceeded:
			return false, fmt.Errorf("pod entered unexpected phase %s, expected Failed", pod.Status.Phase)
		default:
			return false, nil
		}
	})
	require.NoError(t, err, "failed waiting for pod to reach Failed phase")
	return observed
}

// waitForPodEventReason polls for an event with the given reason on the named pod,
// returning its message. Events are eventually consistent, so it polls, not reads once.
func (s *providerSuite) waitForPodEventReason(t *testing.T, podName, reason string) string {
	t.Helper()

	var message string
	pollCtx, pollCancel := context.WithTimeout(t.Context(), *podCreationTimeout)
	defer pollCancel()

	fieldSelector := fmt.Sprintf("involvedObject.name=%s,reason=%s", podName, reason)
	err := wait.PollUntilContextCancel(pollCtx, *podCreationPollInterval, true, func(ctx context.Context) (bool, error) {
		events, err := s.kube.CoreV1().Events(s.namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fieldSelector,
		})
		if err != nil {
			return false, err
		}
		if len(events.Items) == 0 {
			t.Logf("Waiting for event reason %s on pod %s", reason, podName)
			return false, nil
		}
		message = events.Items[0].Message
		t.Logf("Found event reason %s on pod %s: %s", reason, podName, message)
		return true, nil
	})
	require.NoError(t, err, "failed to find event with reason %s for pod %s", reason, podName)
	return message
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

// execCode runs command in the container and decodes the exit code. A clean non-zero
// exit (utilexec.ExitError) returns (stdout, stderr, code, nil); a transport error
// returns code -1 with the error, so a wedge is never misread as an exit code.
// Mirrors the errors.As decode in waitForPostStartProof.
func (s *providerSuite) execCode(ctx context.Context, podName, containerName string, command []string) (string, string, int, error) {
	stdout, stderr, err := s.execContainer(ctx, podName, containerName, command)
	if err == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return stdout, stderr, exitErr.ExitStatus(), nil
	}
	return stdout, stderr, -1, err
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
