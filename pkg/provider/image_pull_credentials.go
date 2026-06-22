package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/virtual-kubelet/virtual-kubelet/log"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	credentialprovider "k8s.io/kubernetes/pkg/credentialprovider"
)

var (
	errEmptyDockerConfig = errors.New("docker config has no auth entries")
)

func (p *MacOSVZProvider) resolveImagePullCredentials(ctx context.Context, pod *corev1.Pod) (resource.RegistryCredentialStore, error) {
	keyring := &credentialprovider.BasicDockerKeyring{}
	// tokenKeyring mirrors keyring registry-for-registry, identity token in the Password slot.
	// Identical registry keys => Lookup match lists align by index, so the token correlates to the
	// exact entry ForImage selects without reimplementing the keyring's registry matching.
	tokenKeyring := &credentialprovider.BasicDockerKeyring{}
	hasCredentials := false
	seen := make(map[string]struct{})
	var secretRefs []corev1.LocalObjectReference

	for _, ref := range pod.Spec.ImagePullSecrets {
		if _, ok := seen[ref.Name]; ok {
			continue
		}
		seen[ref.Name] = struct{}{}
		secretRefs = append(secretRefs, ref)
	}

	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	sa, err := p.k8sClient.CoreV1().ServiceAccounts(pod.Namespace).Get(ctx, saName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return resource.RegistryCredentialStore{}, fmt.Errorf("service account %q not found: %w", saName, err)
	case err != nil:
		return resource.RegistryCredentialStore{}, fmt.Errorf("failed to fetch service account %q: %w", saName, err)
	default:
		// continue with resolved service account
	}

	for _, ref := range sa.ImagePullSecrets {
		if _, ok := seen[ref.Name]; ok {
			continue
		}
		seen[ref.Name] = struct{}{}
		secretRefs = append(secretRefs, ref)
	}

	for _, ref := range secretRefs {
		secret, err := p.k8sClient.CoreV1().Secrets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return resource.RegistryCredentialStore{}, fmt.Errorf("imagePullSecret %q not found: %w", ref.Name, err)
			}
			return resource.RegistryCredentialStore{}, fmt.Errorf("failed to fetch imagePullSecret %q: %w", ref.Name, err)
		}

		switch secret.Type {
		case corev1.SecretTypeDockerConfigJson, corev1.SecretTypeDockercfg:
			// supported types
		default:
			warnErr := fmt.Errorf("ignoring imagePullSecret %q: unsupported type %q", ref.Name, secret.Type)
			log.G(ctx).WithError(warnErr).Warn("Unsupported imagePullSecret type")
			p.eventRecorder.FailedToResolveImagePullSecrets(ctx, warnErr)
			continue
		}

		dockerConfig, identityTokens, err := parseImagePullSecret(secret)
		if err != nil {
			return resource.RegistryCredentialStore{}, fmt.Errorf("failed to parse imagePullSecret %q: %w", ref.Name, err)
		}

		source := &credentialprovider.CredentialSource{
			Secret: &credentialprovider.SecretCoordinates{
				UID:       string(secret.UID),
				Namespace: secret.Namespace,
				Name:      secret.Name,
			},
		}
		// Add to BOTH keyrings one registry at a time, sorted. A whole-map Add iterates Go's randomized
		// map order, so two registry keys normalizing to the same keyring key could append in a
		// different order per keyring, misaligning the index correlation. Sorted single-registry Adds
		// keep the collapse/append order identical in both, binding each token to its entry.
		registries := make([]string, 0, len(dockerConfig))
		for registry := range dockerConfig {
			registries = append(registries, registry)
		}
		sort.Strings(registries)
		for _, registry := range registries {
			keyring.Add(source, credentialprovider.DockerConfig{registry: dockerConfig[registry]})
			tokenKeyring.Add(source, credentialprovider.DockerConfig{
				registry: credentialprovider.DockerConfigEntry{Password: identityTokens[registry]},
			})
		}
		hasCredentials = true
	}

	if !hasCredentials {
		return resource.RegistryCredentialStore{}, nil
	}

	return resource.NewRegistryCredentialStore(keyring, tokenKeyring), nil
}

// parseImagePullSecret returns the docker config plus the per-registry identity tokens recovered
// from the raw secret, returned separately since the kubernetes docker config type cannot model
// an identity token.
func parseImagePullSecret(secret *corev1.Secret) (credentialprovider.DockerConfig, map[string]string, error) {
	switch secret.Type {
	case corev1.SecretTypeDockerConfigJson:
		return parseDockerConfigJSON(secret.Data[corev1.DockerConfigJsonKey])
	case corev1.SecretTypeDockercfg:
		if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok && len(data) > 0 {
			return parseDockerConfigJSON(data)
		}
		cfg, err := parseDockerConfig(secret.Data[corev1.DockerConfigKey])
		return cfg, nil, err
	default:
		return nil, nil, fmt.Errorf("unsupported secret type %q", secret.Type)
	}
}

func parseDockerConfigJSON(raw []byte) (credentialprovider.DockerConfig, map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil, errEmptyDockerConfig
	}

	var cfg credentialprovider.DockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("invalid docker config json: %w", err)
	}

	if len(cfg.Auths) == 0 {
		return nil, nil, errEmptyDockerConfig
	}

	return cfg.Auths, identityTokensFromDockerConfigJSON(raw), nil
}

// identityTokensFromDockerConfigJSON extracts the per-registry identity token from a raw
// .dockerconfigjson, which the kubernetes docker config type does not model. Registries
// without an identity token are omitted.
func identityTokensFromDockerConfigJSON(raw []byte) map[string]string {
	var parsed struct {
		Auths map[string]struct {
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}

	tokens := make(map[string]string, len(parsed.Auths))
	for registry, entry := range parsed.Auths {
		if entry.IdentityToken != "" {
			tokens[registry] = entry.IdentityToken
		}
	}
	return tokens
}

func parseDockerConfig(raw []byte) (credentialprovider.DockerConfig, error) {
	if len(raw) == 0 {
		return nil, errEmptyDockerConfig
	}

	var cfg credentialprovider.DockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid dockercfg: %w", err)
	}

	if len(cfg) == 0 {
		return nil, errEmptyDockerConfig
	}

	return cfg, nil
}
