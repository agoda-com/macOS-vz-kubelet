package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

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

		dockerConfig, err := parseImagePullSecret(secret)
		if err != nil {
			return resource.RegistryCredentialStore{}, fmt.Errorf("failed to parse imagePullSecret %q: %w", ref.Name, err)
		}

		keyring.Add(&credentialprovider.CredentialSource{
			Secret: &credentialprovider.SecretCoordinates{
				UID:       string(secret.UID),
				Namespace: secret.Namespace,
				Name:      secret.Name,
			},
		}, dockerConfig)
		hasCredentials = true
	}

	if !hasCredentials {
		return resource.RegistryCredentialStore{}, nil
	}

	return resource.NewRegistryCredentialStore(keyring), nil
}

func parseImagePullSecret(secret *corev1.Secret) (credentialprovider.DockerConfig, error) {
	switch secret.Type {
	case corev1.SecretTypeDockerConfigJson:
		return parseDockerConfigJSON(secret.Data[corev1.DockerConfigJsonKey])
	default:
		if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
			return parseDockerConfigJSON(data)
		}
		return nil, fmt.Errorf("unsupported secret type %q", secret.Type)
	}
}

func parseDockerConfigJSON(raw []byte) (credentialprovider.DockerConfig, error) {
	if len(raw) == 0 {
		return nil, errEmptyDockerConfig
	}

	var cfg credentialprovider.DockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid docker config json: %w", err)
	}

	if len(cfg.Auths) == 0 {
		return nil, errEmptyDockerConfig
	}

	return cfg.Auths, nil
}
