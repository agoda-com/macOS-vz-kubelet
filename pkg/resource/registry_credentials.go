package resource

import (
	"fmt"

	"github.com/distribution/reference"

	credentialprovider "k8s.io/kubernetes/pkg/credentialprovider"
)

// RegistryCredentials groups authentication material for a container registry.
type RegistryCredentials struct {
	Server   string
	Username string
	Password string
	// IdentityToken is an OAuth2 refresh token from a registry token service; it
	// authenticates the pull on its own, so a token-only credential is still usable.
	IdentityToken string
}

// IsEmpty reports whether the credential set contains usable authentication data.
func (c RegistryCredentials) IsEmpty() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == ""
}

// String redacts the secret material so the value is safe to log. fmt invokes a field's Stringer
// recursively, so a parent struct formatted with %v/%+v inherits the redaction for free.
func (c RegistryCredentials) String() string {
	return fmt.Sprintf("{Server:%s Username:%s Password:%s IdentityToken:%s}",
		c.Server, c.Username, redactSecret(c.Password), redactSecret(c.IdentityToken))
}

// GoString mirrors String for %#v so the Go-syntax form never exposes the password or token either.
func (c RegistryCredentials) GoString() string {
	return fmt.Sprintf("resource.RegistryCredentials{Server:%q, Username:%q, Password:%q, IdentityToken:%q}",
		c.Server, c.Username, redactSecret(c.Password), redactSecret(c.IdentityToken))
}

// redactSecret masks a non-empty secret but leaves an empty one empty, so an absent field stays
// visibly absent rather than masquerading as present.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "<redacted>"
}

// RegistryCredentialStore wraps a Kubernetes docker keyring and exposes helpers that the
// resource managers can consume without depending on kubernetes internals.
type RegistryCredentialStore struct {
	keyring credentialprovider.DockerKeyring
	// tokenKeyring mirrors keyring registry-for-registry, carrying each registry's identity token in
	// the Password slot, so a token correlates to its matched registry without reimplementing the
	// keyring's registry matching. May be nil (no identity tokens).
	tokenKeyring credentialprovider.DockerKeyring
}

// NewRegistryCredentialStore backs the store with keyring. The optional tokenKeyring carries
// per-registry identity tokens aligned with keyring's registry keys.
func NewRegistryCredentialStore(keyring credentialprovider.DockerKeyring, tokenKeyring ...credentialprovider.DockerKeyring) RegistryCredentialStore {
	store := RegistryCredentialStore{keyring: keyring}
	if len(tokenKeyring) > 0 {
		store.tokenKeyring = tokenKeyring[0]
	}
	return store
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

	// tokens aligns with authConfigs by index: tokenKeyring holds the identical registry keys, so
	// its Lookup returns the same matches in the same order. tokens[i] is the identity token for
	// authConfigs[i]'s registry.
	var tokens []credentialprovider.TrackedAuthConfig
	if s.tokenKeyring != nil {
		tokens, _ = s.tokenKeyring.Lookup(imageRef)
	}

	for i, tracked := range authConfigs {
		auth := tracked.AuthConfig
		// Token rides the Password slot of the aligned tokenKeyring entry. Resolve before the IsEmpty
		// skip so a token-only entry (empty user/pass) is not dropped.
		identityToken := auth.IdentityToken
		if identityToken == "" && i < len(tokens) {
			identityToken = tokens[i].Password
		}
		creds := RegistryCredentials{
			Server:        auth.ServerAddress,
			Username:      auth.Username,
			Password:      auth.Password,
			IdentityToken: identityToken,
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
