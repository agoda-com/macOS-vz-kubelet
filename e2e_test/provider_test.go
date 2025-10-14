package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func TestCreatePod(t *testing.T) {
	suite := newProviderSuite(t)

	t.Run("macos-image-upload", func(t *testing.T) {
		suite.uploadMacOSImageIfRequested(t)
	})

	registryHost := suite.registryHost(t)

	suite.ensureNamespace(t)

	node := suite.getNode(t)
	ownerRef := nodeOwnerReference(node)

	secretName := suite.createRegistrySecret(t, ownerRef, registryHost)
	pod := suite.newPod(ownerRef, secretName)
	createdPod := suite.createPod(t, pod)
	_ = suite.waitForPodReady(t, createdPod.Name)

	t.Run("exec-verify-macos-poststart-file", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}

		output := suite.waitForPostStartProof(t, createdPod.Name, "macos", macosProofFilePath, "macos postStart executed")
		t.Logf("macos postStart file content: %s", output)
		assert.Contains(t, output, "macos postStart executed", "macos postStart file content should contain 'macos postStart executed'")
	})

	t.Run("exec-verify-busybox-poststart-file", func(t *testing.T) {
		if *certPath == "" || *keyPath == "" {
			t.SkipNow()
		}

		output := suite.waitForPostStartProof(t, createdPod.Name, "busybox", busyboxProofFilePath, "busybox postStart executed")
		t.Logf("busybox postStart file content: %s", output)
		assert.Contains(t, output, "busybox postStart executed", "busybox postStart file content should contain 'busybox postStart executed'")
	})

	t.Run("get-container-stats", func(t *testing.T) {
		summary := suite.statsSummary(t)

		require.NotNil(t, summary.Node, "node stats should not be nil")
		assert.NotEmpty(t, summary.Node.NodeName, "node name in stats should not be empty")

		var foundPodStats *statsv1alpha1.PodStats
		for i := range summary.Pods {
			ps := summary.Pods[i]
			if ps.PodRef.Name == createdPod.Name && ps.PodRef.Namespace == suite.namespace {
				foundPodStats = &ps
				break
			}
		}
		require.NotNil(t, foundPodStats, "stats for pod %s/%s not found", suite.namespace, createdPod.Name)

		expectedContainers := map[string]bool{
			"macos":   false,
			"busybox": false,
		}

		for _, cs := range foundPodStats.Containers {
			t.Logf("Checking stats for container: %s", cs.Name)
			if _, ok := expectedContainers[cs.Name]; ok {
				require.NotNil(t, cs.CPU, "CPU stats for container %s should not be nil", cs.Name)
				assert.NotNil(t, cs.CPU.UsageCoreNanoSeconds, "CPU UsageCoreNanoSeconds for container %s should not be nil", cs.Name)
				assert.True(t, *cs.CPU.UsageCoreNanoSeconds > 0, "CPU UsageCoreNanoSeconds for container %s should be > 0", cs.Name)

				require.NotNil(t, cs.Memory, "Memory stats for container %s should not be nil", cs.Name)
				assert.NotNil(t, cs.Memory.WorkingSetBytes, "Memory WorkingSetBytes for container %s should not be nil", cs.Name)
				assert.True(t, *cs.Memory.WorkingSetBytes > 0, "Memory WorkingSetBytes for container %s should be > 0", cs.Name)
				expectedContainers[cs.Name] = true
			}
		}

		for name, found := range expectedContainers {
			assert.True(t, found, "stats for container %s were not found or not checked", name)
		}
	})

	suite.deletePod(t, createdPod.Name)
}
