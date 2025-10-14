package resource_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	credentialprovider "k8s.io/kubernetes/pkg/credentialprovider"
)

func TestRegistryCredentialStoreReturnsPodFirst(t *testing.T) {
	keyring := &credentialprovider.BasicDockerKeyring{}
	podCfg := credentialprovider.DockerConfig{
		"index.docker.io/v1/": {
			Username: "pod-user",
			Password: "pod-pass",
		},
	}
	saCfg := credentialprovider.DockerConfig{
		"index.docker.io/v1/": {
			Username: "sa-user",
			Password: "sa-pass",
		},
	}

	keyring.Add(&credentialprovider.CredentialSource{
		Secret: &credentialprovider.SecretCoordinates{
			Namespace: "ns",
			Name:      "pod-secret",
		},
	}, podCfg)
	keyring.Add(&credentialprovider.CredentialSource{
		ServiceAccount: &credentialprovider.ServiceAccountCoordinates{
			Namespace: "ns",
			Name:      "default",
		},
	}, saCfg)

	store := resource.NewRegistryCredentialStore(keyring)

	creds, ok := store.ForImage("nginx:latest")
	require.True(t, ok)
	require.Equal(t, "pod-user", creds.Username)
	require.Equal(t, "pod-pass", creds.Password)
	require.Equal(t, "docker.io", creds.Server)
}

func TestRegistryCredentialStoreHandlesExplicitRegistry(t *testing.T) {
	keyring := &credentialprovider.BasicDockerKeyring{}
	cfg := credentialprovider.DockerConfig{
		"ghcr.io": {
			Username: "user",
			Password: "pass",
		},
	}
	keyring.Add(nil, cfg)

	store := resource.NewRegistryCredentialStore(keyring)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Equal(t, "user", creds.Username)
	require.Equal(t, "pass", creds.Password)
	require.Equal(t, "ghcr.io", creds.Server)
}

func TestRegistryCredentialStoreEmpty(t *testing.T) {
	store := resource.NewRegistryCredentialStore(nil)
	_, ok := store.ForImage("example.com/repo:tag")
	require.False(t, ok)
}
