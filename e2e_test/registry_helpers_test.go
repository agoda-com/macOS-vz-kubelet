package e2e_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	docker "github.com/moby/moby/client"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/bcrypt"
	"k8s.io/apimachinery/pkg/util/wait"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

type registryFixture struct {
	Address  string
	Username string
	Password string
}

func ensureLocalRegistry(t *testing.T, dockerCl *docker.Client, username, password string) (registryFixture, error) {
	t.Helper()

	ctx := t.Context()

	const (
		registryImage = "registry:3"
		registryPort  = "5000"
		labelKey      = "macos.vz.kubelet.e2e.registry"
		labelValue    = "true"
	)

	filtersArgs := filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", labelKey, labelValue)))
	containers, err := dockerCl.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filtersArgs,
	})
	if err != nil {
		return registryFixture{}, fmt.Errorf("list registry containers: %w", err)
	}

	if len(containers) > 0 {
		return prepareExistingRegistry(ctx, dockerCl, containers[0], username, password, registryPort)
	}

	if _, _, err := dockerCl.ImageInspectWithRaw(ctx, registryImage); err != nil {
		if !errdefs.IsNotFound(err) {
			return registryFixture{}, fmt.Errorf("inspect registry image: %w", err)
		}

		reader, pullErr := dockerCl.ImagePull(ctx, registryImage, image.PullOptions{})
		if pullErr != nil {
			return registryFixture{}, fmt.Errorf("pull registry image: %w", pullErr)
		}
		defer func() {
			_ = reader.Close()
		}()

		if _, err := io.Copy(io.Discard, reader); err != nil {
			return registryFixture{}, fmt.Errorf("drain registry image pull: %w", err)
		}
	}

	portKey := nat.Port("5000/tcp")
	containerConfig := &container.Config{
		Image: registryImage,
		Env: []string{
			"REGISTRY_AUTH=htpasswd",
			"REGISTRY_AUTH_HTPASSWD_REALM=Registry Realm",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
			"REGISTRY_HTTP_ADDR=0.0.0.0:5000",
		},
		ExposedPorts: nat.PortSet{
			portKey: struct{}{},
		},
		Labels: map[string]string{
			labelKey: labelValue,
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			portKey: []nat.PortBinding{
				{
					HostIP:   "127.0.0.1",
					HostPort: registryPort,
				},
			},
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Target: "/auth",
			},
		},
	}

	created, err := dockerCl.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, registryContainerName)
	if err != nil {
		return registryFixture{}, fmt.Errorf("create registry container: %w", err)
	}

	if err := dockerCl.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return registryFixture{}, fmt.Errorf("start registry container: %w", err)
	}

	if err := refreshRegistryCredentials(ctx, dockerCl, created.ID, username, password); err != nil {
		return registryFixture{}, fmt.Errorf("prime registry credentials: %w", err)
	}

	address := net.JoinHostPort("localhost", registryPort)
	if err := waitForRegistry(ctx, address, username, password); err != nil {
		return registryFixture{}, fmt.Errorf("wait for registry: %w", err)
	}

	return registryFixture{
		Address:  address,
		Username: username,
		Password: password,
	}, nil
}

func prepareExistingRegistry(ctx context.Context, dockerCl *docker.Client, existing types.Container, username, password, defaultPort string) (registryFixture, error) {
	inspect, err := dockerCl.ContainerInspect(ctx, existing.ID)
	if err != nil {
		return registryFixture{}, fmt.Errorf("inspect registry container: %w", err)
	}

	canonicalName := strings.TrimPrefix(inspect.Name, "/")
	if canonicalName != registryContainerName {
		if err := dockerCl.ContainerRename(ctx, existing.ID, registryContainerName); err != nil {
			return registryFixture{}, fmt.Errorf("rename registry container: %w", err)
		}
		inspect, err = dockerCl.ContainerInspect(ctx, existing.ID)
		if err != nil {
			return registryFixture{}, fmt.Errorf("reinspect registry container after rename: %w", err)
		}
	}

	if inspect.State == nil || !inspect.State.Running {
		if err := dockerCl.ContainerStart(ctx, existing.ID, container.StartOptions{}); err != nil {
			return registryFixture{}, fmt.Errorf("start existing registry container: %w", err)
		}
		inspect, err = dockerCl.ContainerInspect(ctx, existing.ID)
		if err != nil {
			return registryFixture{}, fmt.Errorf("reinspect registry container: %w", err)
		}
	}

	if err := refreshRegistryCredentials(ctx, dockerCl, existing.ID, username, password); err != nil {
		return registryFixture{}, fmt.Errorf("refresh registry credentials: %w", err)
	}

	address := net.JoinHostPort("localhost", defaultPort)
	if len(inspect.NetworkSettings.Ports) > 0 {
		if bindings, ok := inspect.NetworkSettings.Ports["5000/tcp"]; ok && len(bindings) > 0 {
			address = net.JoinHostPort(bindings[0].HostIP, bindings[0].HostPort)
		}
	}

	if err := waitForRegistry(ctx, address, username, password); err != nil {
		return registryFixture{}, fmt.Errorf("wait for registry: %w", err)
	}

	return registryFixture{
		Address:  address,
		Username: username,
		Password: password,
	}, nil
}

func pushMacOSImageToRegistry(ctx context.Context, registry registryFixture, imageDir, imageRef string) (err error) {
	store, err := oci.New(imageDir, false, event.LogEventRecorder{})
	if err != nil {
		return fmt.Errorf("init oci store: %w", err)
	}
	defer func() {
		err = errors.Join(err, store.Close(ctx))
	}()

	cfgPath := filepath.Join(imageDir, oci.MediaTypeConfigV1.Title())
	configBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var cfg oci.Config
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	if len(cfg.Storage) == 0 {
		cfg.Storage = []oci.MediaType{
			oci.MediaTypeAuxImage,
			oci.MediaTypeDiskImage,
		}
	}

	seen := make(map[oci.MediaType]struct{}, len(cfg.Storage))
	layerDescs := make([]ocispec.Descriptor, 0, len(cfg.Storage))
	for _, mediaType := range cfg.Storage {
		if _, ok := seen[mediaType]; ok {
			continue
		}
		seen[mediaType] = struct{}{}

		desc, err := store.Add(ctx, string(mediaType), mediaType.Title())
		if err != nil {
			return fmt.Errorf("add %s: %w", mediaType.Title(), err)
		}
		layerDescs = append(layerDescs, desc)
	}

	configDesc, err := store.Set(ctx, cfg)
	if err != nil {
		return fmt.Errorf("set config: %w", err)
	}

	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}
	repo.PlainHTTP = isLocalhostOrLoopback(repo.Reference.Registry)
	repo.Client = &auth.Client{
		Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: registry.Username,
			Password: registry.Password,
		}),
	}

	if repo.Reference.Reference == "" {
		return fmt.Errorf("image reference %q must include a tag or digest", imageRef)
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_0, "", oras.PackManifestOptions{
		ConfigDescriptor: &configDesc,
		Layers:           layerDescs,
	})
	if err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}

	if err := store.Tag(ctx, manifestDesc, repo.Reference.Reference); err != nil {
		return fmt.Errorf("tag manifest: %w", err)
	}

	scopedCtx := auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPush, auth.ActionPull)
	tolerantRepo := &tolerantRepository{Repository: repo}
	if _, err := oras.Copy(scopedCtx, store, repo.Reference.Reference, tolerantRepo, repo.Reference.Reference, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("push image: %w", err)
	}

	return nil
}

type tolerantRepository struct {
	*remote.Repository
}

func (r *tolerantRepository) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	exists, err := r.Repository.Exists(ctx, target)
	if err != nil {
		var respErr *errcode.ErrorResponse
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusBadRequest {
			return false, nil
		}
	}
	return exists, err
}

func buildDockerConfigJSON(registryHost, username, password string) ([]byte, error) {
	authValue := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
	payload := map[string]map[string]map[string]string{
		"auths": {
			registryHost: {
				"username": username,
				"password": password,
				"auth":     authValue,
			},
		},
	}
	return json.Marshal(payload)
}

func waitForRegistry(ctx context.Context, address, username, password string) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
	}
	endpoint := fmt.Sprintf("http://%s/v2/", address)

	return wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false, err
		}
		req.SetBasicAuth(username, password)

		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode < http.StatusInternalServerError {
			return true, nil
		}
		return false, nil
	})
}

func refreshRegistryCredentials(ctx context.Context, dockerCl *docker.Client, containerID, username, password string) error {
	content, err := buildHtpasswdContent(username, password)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "htpasswd",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		return fmt.Errorf("encode htpasswd header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("encode htpasswd content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finalize htpasswd archive: %w", err)
	}

	if err := dockerCl.CopyToContainer(ctx, containerID, "/auth", bytes.NewReader(buf.Bytes()), types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	}); err != nil {
		return fmt.Errorf("copy htpasswd to container: %w", err)
	}

	return nil
}

func buildHtpasswdContent(username, password string) ([]byte, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("generate htpasswd hash: %w", err)
	}

	return []byte(fmt.Sprintf("%s:%s\n", username, string(hash))), nil
}

func isLocalhostOrLoopback(host string) bool {
	trimmed := strings.Split(host, ":")[0]
	if trimmed == "localhost" {
		return true
	}

	ip := net.ParseIP(trimmed)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
