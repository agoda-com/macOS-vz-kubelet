package resource

import (
	"github.com/distribution/reference"

	credentialprovider "k8s.io/kubernetes/pkg/credentialprovider"
)

// RegistryCredentials groups authentication material for a container registry.
type RegistryCredentials struct {
	Server   string
	Username string
	Password string
}

// IsEmpty reports whether the credential set contains usable authentication data.
func (c RegistryCredentials) IsEmpty() bool {
	return c.Username == "" && c.Password == ""
}

// RegistryCredentialStore wraps a Kubernetes docker keyring and exposes helpers that the
// resource managers can consume without depending on kubernetes internals.
type RegistryCredentialStore struct {
	keyring credentialprovider.DockerKeyring
}

// NewRegistryCredentialStore constructs a credential store backed by the provided keyring.
func NewRegistryCredentialStore(keyring credentialprovider.DockerKeyring) RegistryCredentialStore {
	return RegistryCredentialStore{keyring: keyring}
}

// ForImage resolves credentials for the supplied image reference, if any.
func (s RegistryCredentialStore) ForImage(imageRef string) (RegistryCredentials, bool) {
	if s.keyring == nil {
		return RegistryCredentials{}, false
	}

	authConfigs, ok := s.keyring.Lookup(imageRef)
	if !ok || len(authConfigs) == 0 {
		return RegistryCredentials{}, false
	}

	for _, tracked := range authConfigs {
		auth := tracked.AuthConfig
		creds := RegistryCredentials{
			Server:   auth.ServerAddress,
			Username: auth.Username,
			Password: auth.Password,
		}
		if creds.IsEmpty() {
			continue
		}

		if creds.Server == "" {
			if named, err := reference.ParseNormalizedNamed(imageRef); err == nil {
				creds.Server = reference.Domain(named)
			}
		}
		return creds, true
	}

	return RegistryCredentials{}, false
}
