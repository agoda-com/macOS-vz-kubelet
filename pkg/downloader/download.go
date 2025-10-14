package downloader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	DefaultMinRetryDelay = 2 * time.Second  // Default minimum delay between retries.
	DefaultMaxDelay      = 60 * time.Second // Default maximum delay between retries.
	DefaultMaxAttempts   = 5                // Default maximum number of retry attempts.
	DefaultFactor        = 1.6              // Default factor to increase the delay between retries.
	DefaultJitter        = 0.2              // Default jitter to add to delays.
)

// Params contains the parameters for downloading an OCI image.
type Params struct {
	Ref             string
	StorePath       string
	IgnoreExisiting bool
	Credentials     resource.RegistryCredentials

	MinRetryDelay time.Duration
	MaxDelay      time.Duration
	MaxAttempts   int
}

// Download downloads an OCI image and returns a Config.
// It uses ORAS to pull the image and store it within the local store.
// It retries the download operation with exponential backoff.
func Download(ctx context.Context, params Params, eventRecorder event.EventRecorder) (cfg config.MacPlatformConfigurationOptions, err error) {
	ctx, span := trace.StartSpan(ctx, "Downloader.Download")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if params.MinRetryDelay == 0 {
		params.MinRetryDelay = DefaultMinRetryDelay
	}
	if params.MaxDelay == 0 {
		params.MaxDelay = DefaultMaxDelay
	}
	if params.MaxAttempts == 0 {
		params.MaxAttempts = DefaultMaxAttempts
	}

	store, err := oci.New(filepath.Join(params.StorePath, "blobs", convertToPath(params.Ref)), params.IgnoreExisiting, eventRecorder)
	if err != nil {
		return cfg, fmt.Errorf("failed to initialize store: %w", err)
	}
	defer func() {
		err = errors.Join(err, store.Close(ctx))
	}()

	err = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: params.MinRetryDelay, // Base delay to start with
		Factor:   DefaultFactor,        // Factor to increase the delay between retries
		Jitter:   DefaultJitter,        // Randomization factor to avoid thundering herd problem
		Steps:    params.MaxAttempts,   // Maximum number of retry attempts
		Cap:      params.MaxDelay,      // Maximum delay between retries
	}, func(ctx context.Context) (done bool, _ error) { // never use condition error
		_, err = pull(ctx, params.Ref, store, params.Credentials)
		if err != nil {
			// log error, but do not return it to continue retrying
			eventRecorder.FailedToPullImage(ctx, params.Ref, "", err)
		}
		return err == nil, nil
	})

	if err != nil {
		return cfg, err
	}

	blkStoragePath, err := store.GetFilePathForMediaType(ctx, oci.MediaTypeDiskImage)
	if err != nil {
		return cfg, fmt.Errorf("failed to get file path for media type %s: %w", oci.MediaTypeDiskImage, err)
	}
	auxStoragePath, err := store.GetFilePathForMediaType(ctx, oci.MediaTypeAuxImage)
	if err != nil {
		return cfg, fmt.Errorf("failed to get file path for media type %s: %w", oci.MediaTypeAuxImage, err)
	}
	c, err := store.GetConfig(ctx)
	if err != nil {
		return cfg, fmt.Errorf("failed to get config: %w", err)
	}
	return config.MacPlatformConfigurationOptions{
		BlockStoragePath:      blkStoragePath,
		AuxiliaryStoragePath:  auxStoragePath,
		HardwareModelData:     c.HardwareModelData,
		MachineIdentifierData: c.MachineIdData,
	}, nil
}

// pull pulls an OCI image from a remote repository and stores it in the local store.
// It returns the descriptor of the downloaded content.
func pull(ctx context.Context, ref string, store *oci.Store, creds resource.RegistryCredentials) (desc *ocispec.Descriptor, err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.pull")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("failed to create repository from reference %s: %w", ref, err)
	}
	// Determine if the repository is using plain HTTP based on if it's localhost or a local IP
	repo.PlainHTTP = isLocalhostOrLocalIP(repo.Reference.Registry)

	if !creds.IsEmpty() {
		repo.Client = &auth.Client{
			Credential: auth.StaticCredential(repo.Reference.Registry, toORASCredential(creds)),
		}
	}

	ctx = auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPull)
	descOras, err := oras.Copy(ctx, repo, repo.Reference.Reference, store, repo.Reference.Reference, oras.DefaultCopyOptions)
	if err != nil {
		return nil, err
	}

	return &descOras, nil
}

func toORASCredential(creds resource.RegistryCredentials) auth.Credential {
	return auth.Credential{
		Username: creds.Username,
		Password: creds.Password,
	}
}

// convertToPath converts an OCI image reference to a path format by replacing the colon with a slash.
func convertToPath(s string) string {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return s
	}
	return strings.Join(parts, "/")
}

// isLocalhostOrLocalIP returns true if the host is localhost or a local IP address.
func isLocalhostOrLocalIP(host string) bool {
	host = strings.Split(host, ":")[0] // stripping port if present
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
