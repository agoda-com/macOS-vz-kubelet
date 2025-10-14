package provider_test

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	clientmocks "github.com/agoda-com/macOS-vz-kubelet/pkg/client/mocks"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/provider"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

// check that provider implements nodeutil.Provider
var _ nodeutil.Provider = &provider.MacOSVZProvider{}

const (
	defaultPlatform = "darwin"
)

func TestNewMacOSVZProvider_UnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)

	providerConfig := provider.MacOSVZProviderConfig{
		Platform: "unsupported",
	}

	_, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.Error(t, err)
	assert.Error(t, err, "unsupported platform")
}

func TestCreatePod(t *testing.T) {
	tests := []struct {
		name               string
		pod                *corev1.Pod
		configMaps         []*corev1.ConfigMap
		serviceAccountName string
		expectedConfigMaps map[string]*corev1.ConfigMap
		expectedToken      string
	}{
		{
			name: "Basic Pod with service env vars",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{
									Name:  "TEST_ENV",
									Value: "test",
								},
							},
						},
					},
				},
			},
			configMaps:         []*corev1.ConfigMap{},
			serviceAccountName: "default",
			expectedConfigMaps: map[string]*corev1.ConfigMap{},
			expectedToken:      "",
		},
		{
			name: "Pod with config map and token",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Env: []corev1.EnvVar{
								{
									Name:  "TEST_ENV",
									Value: "test",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								Projected: &corev1.ProjectedVolumeSource{
									Sources: []corev1.VolumeProjection{
										{
											ConfigMap: &corev1.ConfigMapProjection{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: "test-configmap",
												},
											},
										},
										{
											ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
												Audience: "test-audience",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-configmap",
						Namespace: "default",
					},
				},
			},
			serviceAccountName: "default",
			expectedConfigMaps: map[string]*corev1.ConfigMap{
				"test-configmap": {
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-configmap",
						Namespace: "default",
					},
				},
			},
			expectedToken: "test-token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var err error

			// Set up the fake Kubernetes client
			fakeClient := fake.NewSimpleClientset()

			// Add ConfigMaps to the fake client if present
			for _, cm := range tc.configMaps {
				_, err = fakeClient.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			if tc.serviceAccountName == "" {
				tc.serviceAccountName = "default"
			}

			_, err = fakeClient.CoreV1().ServiceAccounts(tc.pod.Namespace).Create(ctx, &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.serviceAccountName,
					Namespace: tc.pod.Namespace,
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			if tc.pod.Spec.ServiceAccountName == "" {
				tc.pod.Spec.ServiceAccountName = tc.serviceAccountName
			}

			// Mock Virtualization Client
			vzClient := clientmocks.NewVzClientInterface(t)

			// Mock token generation
			if tc.expectedToken != "" {
				fakeClient.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, &authv1.TokenRequest{
						Status: authv1.TokenRequestStatus{Token: tc.expectedToken},
					}, nil
				})
			}

			providerConfig := provider.MacOSVZProviderConfig{
				Platform:  defaultPlatform,
				K8sClient: fakeClient,
			}

			p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
			require.NoError(t, err)

			// Set up the expected pod
			expectedPod := tc.pod.DeepCopy()

			// Mock Virtualization Client's CreateVirtualizationGroup method
			vzClient.On("CreateVirtualizationGroup", mock.Anything, expectedPod, tc.expectedToken, tc.expectedConfigMaps, mock.MatchedBy(func(store resource.RegistryCredentialStore) bool {
				_, ok := store.ForImage("nginx:latest")
				return !ok
			})).Return(nil)

			// Call the provider's CreatePod function
			err = p.CreatePod(ctx, tc.pod)
			assert.NoError(t, err)

			// Verify the expectations
			vzClient.AssertExpectations(t)

			// Check that the object reference was correctly populated
			calledCtx, ok := vzClient.Calls[0].Arguments[0].(context.Context)
			assert.True(t, ok)
			objRef, ok := event.GetObjectRef(calledCtx)
			assert.True(t, ok)
			assert.Equal(t, *objRef, corev1.ObjectReference{
				Namespace: tc.pod.Namespace,
				Name:      tc.pod.Name,
				UID:       tc.pod.UID,
			})
		})
	}
}

func TestCreatePod_ImagePullSecrets(t *testing.T) {
	authJSON := base64.StdEncoding.EncodeToString([]byte("jsonuser:jsonpass"))

	tests := []struct {
		name         string
		secretType   corev1.SecretType
		secretData   map[string][]byte
		createSecret bool
		expectErr    bool
		expectedHost string
		expectedCred resource.RegistryCredentials
		podImage     string
	}{
		{
			name:         "DockerConfigJSON",
			secretType:   corev1.SecretTypeDockerConfigJson,
			secretData:   map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://registry.example.com":{"auth":"` + authJSON + `"}}}`)},
			createSecret: true,
			expectedHost: "registry.example.com",
			expectedCred: resource.RegistryCredentials{Username: "jsonuser", Password: "jsonpass", Server: "registry.example.com"},
			podImage:     "registry.example.com/app:1",
		},
		{
			name:         "InvalidSecretData",
			secretType:   corev1.SecretTypeDockerConfigJson,
			secretData:   map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
			createSecret: true,
			expectErr:    true,
			podImage:     "registry.example.com/app:3",
		},
		{
			name:         "MissingSecret",
			createSecret: false,
			expectErr:    true,
			podImage:     "registry.example.com/app:4",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fakeClient := fake.NewSimpleClientset()

			_, err := fakeClient.CoreV1().ServiceAccounts("default").Create(ctx, &corev1.ServiceAccount{
				ObjectMeta:       metav1.ObjectMeta{Name: "default", Namespace: "default"},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull-secret"}},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			if tc.createSecret {
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "default"},
					Type:       tc.secretType,
					Data:       tc.secretData,
				}
				_, err := fakeClient.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			vzClient := clientmocks.NewVzClientInterface(t)

			providerConfig := provider.MacOSVZProviderConfig{
				Platform:      defaultPlatform,
				K8sClient:     fakeClient,
				EventRecorder: event.LogEventRecorder{},
			}

			p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
			require.NoError(t, err)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					ServiceAccountName: "default",
					ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "pull-secret"}},
					Containers: []corev1.Container{
						{
							Name:  "macos",
							Image: tc.podImage,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resourceapi.MustParse("1"),
									corev1.ResourceMemory: resourceapi.MustParse("256Mi"),
								},
							},
						},
					},
				},
			}

			expectedPod := pod.DeepCopy()

			if !tc.expectErr {
				vzClient.On("CreateVirtualizationGroup", mock.Anything, expectedPod, "", mock.Anything, mock.MatchedBy(func(store resource.RegistryCredentialStore) bool {
					cred, ok := store.ForImage(tc.podImage)
					if !ok {
						return false
					}
					return cred.Username == tc.expectedCred.Username && cred.Password == tc.expectedCred.Password
				})).Return(nil)
			}

			err = p.CreatePod(ctx, pod)
			if tc.expectErr {
				require.Error(t, err)
				assert.True(t, errdefs.IsInvalidInput(err))
				vzClient.AssertNotCalled(t, "CreateVirtualizationGroup", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				return
			}

			require.NoError(t, err)
			vzClient.AssertExpectations(t)
		})
	}
}

func TestUpdatePod(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	assert.Error(t, p.UpdatePod(ctx, &corev1.Pod{}), "UpdatePod should return an error")
	vzClient.AssertExpectations(t)
}

func TestDeletePod(t *testing.T) {
	tests := []struct {
		name                         string
		pod                          *corev1.Pod
		vzClientDeleteError          error
		executeContainerCommandError error
		expectedPreStopExecuted      bool
		expectK8sPodDeletion         bool
	}{
		{
			name: "Basic pod deletion without pre-stop",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			vzClientDeleteError:          nil,
			executeContainerCommandError: nil,
			expectedPreStopExecuted:      false,
			expectK8sPodDeletion:         true,
		},
		{
			name: "Pod with pre-stop hook and grace period - success",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:                       "test-pod",
					Namespace:                  "default",
					DeletionGracePeriodSeconds: func(i int64) *int64 { return &i }(10),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"echo", "PreStop"},
									},
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			vzClientDeleteError:          nil,
			executeContainerCommandError: nil,
			expectedPreStopExecuted:      true,
			expectK8sPodDeletion:         true,
		},
		{
			name: "Pod with pre-stop hook and grace period - fail",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:                       "test-pod",
					Namespace:                  "default",
					DeletionGracePeriodSeconds: func(i int64) *int64 { return &i }(10),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"echo", "PreStop"},
									},
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			vzClientDeleteError:          nil,
			executeContainerCommandError: assert.AnError,
			expectedPreStopExecuted:      true,
			expectK8sPodDeletion:         false,
		},
		{
			name: "Pod with pre-stop hook and no grace period - hook skipped",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:                       "test-pod",
					Namespace:                  "default",
					DeletionGracePeriodSeconds: func(i int64) *int64 { return &i }(0),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "test-container",
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"echo", "PreStop"},
									},
								},
							},
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			vzClientDeleteError:          nil,
			executeContainerCommandError: nil,
			expectedPreStopExecuted:      false,
			expectK8sPodDeletion:         true,
		},
		{
			name: "Virtualization group deletion failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			vzClientDeleteError:     assert.AnError,
			expectedPreStopExecuted: false,
			expectK8sPodDeletion:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Mock the Virtualization client
			vzClient := clientmocks.NewVzClientInterface(t)

			// Set up the fake Kubernetes client and pre-create the pod
			fakeClient := fake.NewSimpleClientset(tc.pod)

			// Set up the fake event recorder
			fakeRecorder := record.NewFakeRecorder(1)
			kubeRecorder := event.NewKubeEventRecorder(fakeRecorder)

			// WaitGroup to synchronize the test with the asynchronous goroutine
			var wg sync.WaitGroup
			wg.Add(1)

			// Mock the DeleteVirtualizationGroup function
			vzClient.On("DeleteVirtualizationGroup", mock.Anything, tc.pod.Namespace, tc.pod.Name, mock.Anything).
				Run(func(args mock.Arguments) {
					// Simulate the goroutine completion by signaling the waitgroup
					wg.Done()
				}).
				Return(tc.vzClientDeleteError).Once()

			// Mock the ExecuteContainerCommand for pre-stop hooks if required
			if tc.expectedPreStopExecuted {
				vzClient.On("ExecuteContainerCommand", mock.Anything, tc.pod.Namespace, tc.pod.Name, tc.pod.Spec.Containers[0].Name, tc.pod.Spec.Containers[0].Lifecycle.PreStop.Exec.Command, mock.Anything).
					Return(tc.executeContainerCommandError).
					Once()
			}

			// Set up the provider with fake clients
			p, err := provider.NewMacOSVZProvider(ctx, vzClient, provider.MacOSVZProviderConfig{
				Platform:      defaultPlatform,
				K8sClient:     fakeClient,
				EventRecorder: kubeRecorder,
			})
			require.NoError(t, err)

			// Call the DeletePod function
			err = p.DeletePod(ctx, tc.pod)
			assert.NoError(t, err)

			// Wait for the asynchronous deletion to complete
			wg.Wait()

			// Check if pre-stop hooks were executed
			vzClient.AssertExpectations(t)

			if tc.executeContainerCommandError != nil {
				// expect event recorded
				event := <-fakeRecorder.Events
				assert.Contains(t, event, tc.executeContainerCommandError.Error())
			}

			// Check if the Kubernetes pod was deleted
			if tc.expectK8sPodDeletion {
				_, err := fakeClient.CoreV1().Pods(tc.pod.Namespace).Get(ctx, tc.pod.Name, metav1.GetOptions{})
				assert.Error(t, err) // Expect an error as the pod should be deleted
			} else {
				_, err := fakeClient.CoreV1().Pods(tc.pod.Namespace).Get(ctx, tc.pod.Name, metav1.GetOptions{})
				assert.NoError(t, err) // Pod should still exist if deletion failed
			}
		})
	}
}

func TestGetPod(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}

	// Mock the Virtualization client
	vzClient := clientmocks.NewVzClientInterface(t)

	// Test cases
	t.Run("Basic pod", func(t *testing.T) {
		vg := &client.VirtualizationGroup{
			MacOSVirtualMachine: &resource.MacOSVirtualMachine{},
		}
		vzClient.On("GetVirtualizationGroup", mock.Anything, pod.Namespace, pod.Name).Return(vg, nil).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient, pod)

		pod, err := p.GetPod(ctx, pod.Namespace, pod.Name)
		assert.NoError(t, err)
		assert.NotNil(t, pod.Status) // pod must have status generated
	})

	t.Run("Virtualization group not found", func(t *testing.T) {
		vzClient.On("GetVirtualizationGroup", mock.Anything, pod.Namespace, pod.Name).Return(nil, assert.AnError).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient, pod)

		_, err := p.GetPod(ctx, pod.Namespace, pod.Name)
		assert.Error(t, err)
		assert.Equal(t, assert.AnError, err)
	})

	t.Run("Pod does not exist", func(t *testing.T) {
		vg := &client.VirtualizationGroup{
			MacOSVirtualMachine: &resource.MacOSVirtualMachine{},
		}
		vzClient.On("GetVirtualizationGroup", mock.Anything, pod.Namespace, pod.Name).Return(vg, nil).Once()
		// Virtualization group must clean up
		vzClient.On("DeleteVirtualizationGroup", mock.Anything, pod.Namespace, pod.Name, provider.DefaultDeleteVZGroupGracePeriodSeconds).Return(nil).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient)

		_, err := p.GetPod(ctx, pod.Namespace, pod.Name)
		assert.NoError(t, err)
	})

	vzClient.AssertExpectations(t)
}

func TestGetPodStatus_GetVirtualizationGroupError(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	namespace := "default"
	podName := "test-pod"

	vzClient.On("GetVirtualizationGroup", mock.Anything, namespace, podName).Return(nil, assert.AnError)

	_, err = p.GetPodStatus(ctx, namespace, podName)
	assert.Error(t, err)
	vzClient.AssertExpectations(t)
}

func TestGetPods(t *testing.T) {
	ctx := context.Background()

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-1",
				Namespace: "default",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-2",
				Namespace: "default",
			},
		},
	}

	// Mock the Virtualization client
	vzClient := clientmocks.NewVzClientInterface(t)

	t.Run("Basic pods", func(t *testing.T) {
		vg := &client.VirtualizationGroup{
			MacOSVirtualMachine: &resource.MacOSVirtualMachine{},
		}
		vzClient.On("GetVirtualizationGroupListResult", mock.Anything).
			Return(map[types.NamespacedName]*client.VirtualizationGroup{
				{Namespace: pods[0].Namespace, Name: pods[0].Name}: vg,
				{Namespace: pods[1].Namespace, Name: pods[1].Name}: vg,
			}, nil).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient, pods[0], pods[1])

		result, err := p.GetPods(ctx)
		assert.NoError(t, err)
		assert.Len(t, result, 2)
		// pod must have status generated
		assert.NotNil(t, result[0].Status)
		assert.NotNil(t, result[1].Status)
	})

	t.Run("Virtualization group error", func(t *testing.T) {
		vzClient.On("GetVirtualizationGroupListResult", mock.Anything).Return(nil, assert.AnError).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient)

		_, err := p.GetPods(ctx)
		assert.Error(t, err)
		assert.Equal(t, assert.AnError, err)
	})

	t.Run("Pods do not exist", func(t *testing.T) {
		vg := &client.VirtualizationGroup{
			MacOSVirtualMachine: &resource.MacOSVirtualMachine{},
		}
		vzClient.On("GetVirtualizationGroupListResult", mock.Anything).
			Return(map[types.NamespacedName]*client.VirtualizationGroup{
				{Namespace: pods[0].Namespace, Name: pods[0].Name}: vg,
				{Namespace: pods[1].Namespace, Name: pods[1].Name}: vg,
			}, nil).Once()

		p := setupVZProviderWithPodInformer(t, ctx, vzClient)

		_, err := p.GetPods(ctx)
		assert.Error(t, err)
	})

	vzClient.AssertExpectations(t)
}

func TestGetContainerLogs(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	namespace := "default"
	podName := "test-pod"
	containerName := "test-container"
	opts := api.ContainerLogOpts{
		Follow: true,
	}

	// create empty reader
	reader := io.NopCloser(strings.NewReader(""))

	vzClient.On("GetContainerLogs", mock.Anything, namespace, podName, containerName, opts).Return(reader, assert.AnError)

	r, err := p.GetContainerLogs(ctx, namespace, podName, containerName, opts)
	assert.Equal(t, reader, r)
	assert.Equal(t, assert.AnError, err)
	vzClient.AssertExpectations(t)
}

func TestRunInContainer(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	namespace := "default"
	podName := "test-pod"
	containerName := "test-container"
	command := []string{"echo", "test"}
	exec := node.DiscardingExecIO()

	vzClient.On("ExecuteContainerCommand", mock.Anything, namespace, podName, containerName, command, exec).Return(assert.AnError)

	err = p.RunInContainer(ctx, namespace, podName, containerName, command, exec)
	assert.Equal(t, assert.AnError, err)
	vzClient.AssertExpectations(t)
}

func TestAttachToContainer(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	namespace := "default"
	podName := "test-pod"
	containerName := "test-container"
	exec := node.DiscardingExecIO()

	vzClient.On("AttachToContainer", mock.Anything, namespace, podName, containerName, exec).Return(assert.AnError)

	err = p.AttachToContainer(ctx, namespace, podName, containerName, exec)
	assert.Equal(t, assert.AnError, err)
	vzClient.AssertExpectations(t)
}

// func TestGetStatsSummary(t *testing.T) {
// 	ctx := context.Background()
// 	vzClient := clientmocks.NewVzClientInterface(t)
// 	providerConfig := provider.MacOSVZProviderConfig{
// 		Platform: defaultPlatform,
// 	}
// 	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
// 	require.NoError(t, err)

// 	_, err = p.GetStatsSummary(ctx)
// 	assert.NoError(t, err, "GetStatsSummary should not return an error")
// 	vzClient.AssertExpectations(t)
// }

// func TestGetMetricsResource(t *testing.T) {
// 	ctx := context.Background()
// 	vzClient := clientmocks.NewVzClientInterface(t)
// 	providerConfig := provider.MacOSVZProviderConfig{
// 		Platform: defaultPlatform,
// 	}
// 	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
// 	require.NoError(t, err)

// 	_, err = p.GetMetricsResource(ctx)
// 	assert.Error(t, err, "GetMetricsResource should return an error")
// 	vzClient.AssertExpectations(t)
// }

func TestPortForward(t *testing.T) {
	ctx := context.Background()
	vzClient := clientmocks.NewVzClientInterface(t)
	providerConfig := provider.MacOSVZProviderConfig{
		Platform: defaultPlatform,
	}
	p, err := provider.NewMacOSVZProvider(ctx, vzClient, providerConfig)
	require.NoError(t, err)

	reader, writer := io.Pipe()
	rwc := struct {
		io.Reader
		io.Writer
		io.Closer
	}{Reader: reader, Writer: writer, Closer: writer}
	err = p.PortForward(ctx, "", "", 10, rwc)
	assert.Error(t, err, "PortForward should return an error")
	vzClient.AssertExpectations(t)
}
