package resource_test

import (
	"fmt"
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

// fixedKeyring is a minimal DockerKeyring that always returns the same ordered auth configs for any
// image. The exported BasicDockerKeyring cannot inject an identity token (its DockerConfigEntry only
// models username/password), so the token-correlation path is exercised through hand-built keyrings.
type fixedKeyring struct {
	auths []credentialprovider.AuthConfig
}

func (k fixedKeyring) Lookup(image string) ([]credentialprovider.TrackedAuthConfig, bool) {
	if len(k.auths) == 0 {
		return nil, false
	}
	tracked := make([]credentialprovider.TrackedAuthConfig, 0, len(k.auths))
	for i := range k.auths {
		tracked = append(tracked, *credentialprovider.NewTrackedAuthConfig(&k.auths[i], nil))
	}
	return tracked, true
}

// tokenKeyringFor builds a token keyring whose entries carry the identity tokens in the Password
// slot, aligned by index with the primary keyring's auth configs (the provider's contract).
func tokenKeyringFor(tokens ...string) fixedKeyring {
	auths := make([]credentialprovider.AuthConfig, 0, len(tokens))
	for _, token := range tokens {
		auths = append(auths, credentialprovider.AuthConfig{Password: token})
	}
	return fixedKeyring{auths: auths}
}

func TestRegistryCredentialStorePopulatesIdentityToken(t *testing.T) {
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{{ServerAddress: "ghcr.io", Username: "user", Password: "pass"}}},
		tokenKeyringFor("refresh-token-fixture-value"),
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Equal(t, "user", creds.Username)
	require.Equal(t, "pass", creds.Password)
	require.Equal(t, "refresh-token-fixture-value", creds.IdentityToken)
	require.Equal(t, "ghcr.io", creds.Server)
}

func TestRegistryCredentialStoreIdentityTokenOnlyNotSkipped(t *testing.T) {
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{{ServerAddress: "ghcr.io"}}},
		tokenKeyringFor("refresh-token-fixture-value"),
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Empty(t, creds.Username)
	require.Empty(t, creds.Password)
	require.Equal(t, "refresh-token-fixture-value", creds.IdentityToken)
	require.Equal(t, "ghcr.io", creds.Server)
}

func TestRegistryCredentialStoreDoesNotTreatEmailAsToken(t *testing.T) {
	// A real .dockerconfigjson carries an email but no identity token; the email must never be
	// surfaced as the identity token (the regression the side-map design exists to prevent).
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{{ServerAddress: "ghcr.io", Username: "user", Password: "pass", Email: "me@example.com"}}},
		tokenKeyringFor(""),
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Empty(t, creds.IdentityToken)
	require.Equal(t, "pass", creds.Password)
}

func TestRegistryCredentialStoreNilTokenKeyringYieldsNoToken(t *testing.T) {
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{{ServerAddress: "ghcr.io", Username: "user", Password: "pass"}}},
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Empty(t, creds.IdentityToken)
}

func TestRegistryCredentialStoreCorrelatesTokenToSelectedEntry(t *testing.T) {
	// The first (most-specific) match is tokenless; the token belongs to a later entry. The token
	// keyring is index-aligned, so the selected entry's token (empty here) must win - the later
	// entry's token must not bleed across.
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{
			{ServerAddress: "ghcr.io", Username: "specific", Password: "pass"},
			{ServerAddress: "ghcr.io", Username: "broad", Password: "pass"},
		}},
		tokenKeyringFor("", "broad-token-fixture-value"),
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Equal(t, "specific", creds.Username)
	require.Empty(t, creds.IdentityToken)
}

func TestRegistryCredentialStorePrefersAuthConfigIdentityToken(t *testing.T) {
	// A real IdentityToken on the AuthConfig (e.g. a future credential-provider plugin) wins over the
	// side-map token.
	store := resource.NewRegistryCredentialStore(
		fixedKeyring{auths: []credentialprovider.AuthConfig{{ServerAddress: "ghcr.io", IdentityToken: "real-token"}}},
		tokenKeyringFor("side-map-token"),
	)

	creds, ok := store.ForImage("ghcr.io/agoda/macos:latest")
	require.True(t, ok)
	require.Equal(t, "real-token", creds.IdentityToken)
}

func TestRegistryCredentialsIsEmpty(t *testing.T) {
	tests := []struct {
		name  string
		creds resource.RegistryCredentials
		want  bool
	}{
		{name: "all empty", creds: resource.RegistryCredentials{}, want: true},
		{name: "username only", creds: resource.RegistryCredentials{Username: "user"}, want: false},
		{name: "password only", creds: resource.RegistryCredentials{Password: "pass"}, want: false},
		{name: "identity token only", creds: resource.RegistryCredentials{IdentityToken: "refresh-token-fixture-value"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.creds.IsEmpty())
		})
	}
}

func TestRegistryCredentialsRedactsSecrets(t *testing.T) {
	const (
		secretPassword = "password-redaction-sentinel"
		secretToken    = "identity-token-redaction-sentinel"
	)
	creds := resource.RegistryCredentials{
		Server:        "registry.example.com",
		Username:      "user",
		Password:      secretPassword,
		IdentityToken: secretToken,
	}

	// %+v and %v call String(); %#v calls GoString(). A parent struct that holds the
	// credentials by value invokes those recursively, so all three must stay redacted.
	type holder struct {
		Name  string
		Creds resource.RegistryCredentials
	}
	parent := holder{Name: "params", Creds: creds}

	for _, rendered := range []string{
		fmt.Sprintf("%+v", creds),
		fmt.Sprintf("%v", creds),
		creds.String(),
		fmt.Sprintf("%+v", parent),
		fmt.Sprintf("%v", parent),
		fmt.Sprintf("%#v", parent),
	} {
		require.NotContains(t, rendered, secretPassword, "password leaked in %q", rendered)
		require.NotContains(t, rendered, secretToken, "identity token leaked in %q", rendered)
		require.Contains(t, rendered, "registry.example.com", "server should be visible in %q", rendered)
		require.Contains(t, rendered, "user", "username should be visible in %q", rendered)
	}
}

func TestRegistryCredentialsStringOmitsEmptySecrets(t *testing.T) {
	creds := resource.RegistryCredentials{Server: "registry.example.com", Username: "user"}
	rendered := creds.String()
	require.NotContains(t, rendered, "<redacted>", "empty secrets should not render a redaction marker: %q", rendered)
	require.Contains(t, rendered, "registry.example.com")
	require.Contains(t, rendered, "user")
}
