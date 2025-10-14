package resourcemanager

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	containerdata "github.com/agoda-com/macOS-vz-kubelet/internal/data/container"
	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	registrytypes "github.com/docker/docker/api/types/registry"
	dockercl "github.com/moby/moby/client"
	"github.com/moby/moby/pkg/stdcopy"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// ContainerNamePrefix is the prefix for container names that helps us identify containers managed by the virtual-kubelet.
	ContainerNamePrefix = "macos-vz"

	DefaultMinRetryDelay = 2 * time.Second  // Default minimum delay between retries.
	DefaultMaxDelay      = 60 * time.Second // Default maximum delay between retries.
	DefaultMaxAttempts   = 5                // Default maximum number of retry attempts.
	DefaultFactor        = 1.6              // Default factor to increase the delay between retries.
	DefaultJitter        = 0.2              // Default jitter to add to delays.
)

// DockerClient manages Docker containers for pods.
type DockerClient struct {
	client        *dockercl.Client
	eventRecorder event.EventRecorder
	data          containerdata.ContainerData
}

// NewDockerClient initializes a new ContainerClient for docker containers.
func NewDockerClient(ctx context.Context, client *dockercl.Client, eventRecorder event.EventRecorder) (c *DockerClient, err error) {
	ctx, span := trace.StartSpan(ctx, "dockerClient.NewDockerClient")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if client == nil {
		return nil, fmt.Errorf("must provide a containers client")
	}

	dockerClient := &DockerClient{
		client:        client,
		eventRecorder: eventRecorder,
	}

	containers, err := getActiveContainers(ctx, client)
	if err != nil {
		return nil, err
	}

	// Cleanup dangling containers
	for _, ids := range containers {
		for _, id := range ids {
			log.G(ctx).Infof("Removing dangling container %s", id)
			_ = client.ContainerRemove(ctx, id, dockercontainer.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	}

	return dockerClient, nil
}

// CreateContainer creates and starts a Docker container for a given pod.
func (c *DockerClient) CreateContainer(ctx context.Context, params ContainerParams) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.CreateContainer")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	_, loaded := c.data.GetOrCreateContainerInfo(params.PodNamespace, params.PodName, params.Name, containerdata.ContainerInfo{})
	if loaded {
		return errdefs.AsInvalidInput(fmt.Errorf("container %s already exists", params.Name))
	}

	// Handle container creation in go routine to avoid blocking the main loop
	go c.handleDockerContainerCreation(ctx, params)

	return nil
}

// handleDockerContainerCreation creates a Docker container and starts it.
func (c *DockerClient) handleDockerContainerCreation(ctx context.Context, params ContainerParams) {
	var containerInfo containerdata.ContainerInfo
	var err error
	ctx, span := trace.StartSpan(ctx, "DockerClient.handleDockerContainerCreation")
	defer func() {
		span.SetStatus(err)
		span.End()
		if err != nil {
			c.data.SetContainerInfo(params.PodNamespace, params.PodName, params.Name, containerInfo.WithError(err))
		}
	}()
	logger := log.G(ctx)
	logger.Debugf("Creating container with params: %+v", params)

	switch params.ImagePullPolicy {
	case corev1.PullAlways:
		logger.Debug("Removing existing image due to pull policy")
		_, _ = c.client.ImageRemove(ctx, params.Image, image.RemoveOptions{
			Force:         false, // Do not remove images that are potentially in use by running containers
			PruneChildren: true,
		})
		fallthrough
	case corev1.PullIfNotPresent:
		c.eventRecorder.PullingImage(ctx, params.Image, params.Name)
		startTime := time.Now()
		err = c.pullImage(ctx, params.Image, params.Name, params.RegistryCreds)
		if err != nil {
			c.eventRecorder.BackOffPullImage(ctx, params.Image, params.Name, err)
			return
		}
		c.eventRecorder.PulledImage(ctx, params.Image, params.Name, time.Since(startTime).String())
	case corev1.PullNever:
		// Do nothing
	}

	config := createDockerContainerConfig(params)
	hostConfig := createDockerHostConfigFromMounts(params.Mounts)
	containerName := getUnderlyingContainerName(params.PodNamespace, params.PodName, params.Name)
	result, err := c.client.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		c.eventRecorder.FailedToCreateContainer(ctx, params.Name, err)
		return
	}

	// Store the container ID for future reference
	containerInfo = containerInfo.WithID(result.ID)
	c.data.SetContainerInfo(params.PodNamespace, params.PodName, params.Name, containerInfo)

	c.eventRecorder.CreatedContainer(ctx, params.Name)
	err = c.client.ContainerStart(ctx, result.ID, dockercontainer.StartOptions{})
	if err != nil {
		c.eventRecorder.FailedToStartContainer(ctx, params.Name, err)
		return
	}
	c.eventRecorder.StartedContainer(ctx, params.Name)

	if params.PostStartAction == nil {
		// No post-start action specified, return early
		return
	}

	// Execute the post-start action
	if err := c.execPostStartAction(ctx, result.ID, params.PodNamespace, params.PodName, params.Name, *params.PostStartAction); err != nil {
		c.eventRecorder.FailedPostStartHook(ctx, params.Name, params.PostStartAction.Command, err)
	}
}

// pullImage pulls the specified Docker image.
func (c *DockerClient) pullImage(ctx context.Context, ref string, containerName string, creds resource.RegistryCredentials) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.pullImage")
	ctx = span.WithFields(ctx, log.Fields{
		"image":         ref,
		"containerName": containerName,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	authHeader, encodeErr := encodeRegistryAuth(creds)
	if encodeErr != nil {
		return fmt.Errorf("failed to encode registry auth: %w", encodeErr)
	}

	err = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: DefaultMinRetryDelay, // Base delay to start with
		Factor:   DefaultFactor,        // Factor to increase the delay between retries
		Jitter:   DefaultJitter,        // Randomization factor to avoid thundering herd problem
		Steps:    DefaultMaxAttempts,   // Maximum number of retry attempts
		Cap:      DefaultMaxDelay,      // Maximum delay between retries
	}, func(ctx context.Context) (done bool, _ error) { // never use condition error
		reader, err := c.client.ImagePull(ctx, ref, image.PullOptions{
			RegistryAuth: authHeader,
		})
		if err != nil {
			c.eventRecorder.FailedToPullImage(ctx, ref, containerName, err)
			return err == nil, nil
		}

		var line string
		bufReader := bufio.NewReader(reader)
		for {
			line, err = bufReader.ReadString('\n')
			log.G(ctx).Debug(line)
			if err != nil {
				break
			}
		}

		return err == io.EOF, reader.Close()
	})

	return err
}

func encodeRegistryAuth(creds resource.RegistryCredentials) (string, error) {
	if creds.IsEmpty() {
		return "", nil
	}

	authConfig := registrytypes.AuthConfig{
		Username:      creds.Username,
		Password:      creds.Password,
		ServerAddress: creds.Server,
	}
	return registrytypes.EncodeAuthConfig(authConfig)
}

// execPostStartAction executes a post-start action in the specified container.
func (c *DockerClient) execPostStartAction(ctx context.Context, containerID, podNs, podName, containerName string, action resource.ExecAction) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.execPostStart")
	ctx = span.WithFields(ctx, log.Fields{
		"containerID":   containerID,
		"namespace":     podNs,
		"name":          podName,
		"containerName": containerName,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Debugf("Executing post-start action: %+v", action)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var data types.ContainerJSON
	for {
		select {
		case <-ticker.C:
			data, err = c.client.ContainerInspect(ctx, containerID)
			if err != nil {
				logger.WithError(err).Warnf("failed to inspect container %s", containerID)
				return err
			}
			if data.State == nil || !data.State.Running {
				continue
			}

			ctx, cancel := context.WithTimeout(ctx, action.TimeoutDuration)
			defer cancel() // Ensure context is cancelled to avoid leaking resources

			logger.Info("Container is running, executing post-start command")
			err = c.ExecInContainer(ctx, podNs, podName, containerName, action.Command, node.DiscardingExecIO())
			if err != nil {
				logger.WithError(err).Warnf("failed to execute post-start command for container %s", containerName)
				return err
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RemoveContainers removes all containers associated with a given pod.
func (c *DockerClient) RemoveContainers(ctx context.Context, podNs, podName string, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.RemoveContainers")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	containerInfoMap, loaded := c.data.RemoveAllContainerInfo(podNs, podName)
	if !loaded {
		return errdefs.NotFound("containers not found")
	}

	var errs []error
	for _, containerInfo := range containerInfoMap {
		if containerInfo.ID == "" {
			// Skip containers that were not created
			continue
		}
		// TODO: don't force and rather implement grace period
		if err := c.client.ContainerRemove(ctx, containerInfo.ID, dockercontainer.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// GetContainers retrieves the container objects for a given pod namespace and name.
func (c *DockerClient) GetContainers(ctx context.Context, podNs, podName string) (containers []resource.Container, err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.GetContainers")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	containerInfoMap, ok := c.data.GetAllContainerInfo(podNs, podName)
	if !ok {
		return nil, errdefs.NotFound("containers not found")
	}

	return c.getContainersWrapped(ctx, containerInfoMap), nil
}

// GetContainersListResult fetches the list of containers for all pods managed by the DockerClient.
func (c *DockerClient) GetContainersListResult(ctx context.Context) (map[k8stypes.NamespacedName][]resource.Container, error) {
	_, span := trace.StartSpan(ctx, "DockerClient.GetContainersListResult")
	defer span.End()

	containerData := c.data.GetAllData()
	result := make(map[k8stypes.NamespacedName][]resource.Container, len(containerData))
	for key, containerInfoMap := range containerData {
		result[key] = c.getContainersWrapped(ctx, containerInfoMap)
	}
	return result, nil
}

// getContainersWrapped retrieves and wraps container details for provided container IDs.
func (c *DockerClient) getContainersWrapped(ctx context.Context, containerInfoMap map[string]containerdata.ContainerInfo) []resource.Container {
	logger := log.G(ctx)
	var containers []resource.Container

	for containerName, containerInfo := range containerInfoMap {
		container := resource.Container{
			ID:   containerInfo.ID,
			Name: containerName,
		}

		if containerInfo.Error != nil {
			container.State.Error = containerInfo.Error.Error()
		} else if container.ID != "" {
			result, err := c.client.ContainerInspect(ctx, containerInfo.ID)
			if err != nil {
				logger.WithError(err).Warnf("failed to inspect container %s", containerInfo.ID)
				container.State.Error = err.Error()
			} else {
				container.State = containerStateFromDockerState(ctx, result.State)
			}
		}

		containers = append(containers, container)
	}

	return containers
}

// GetContainerLogs retrieves the logs for a specific docker container.
func (c *DockerClient) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (in io.ReadCloser, err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.GetContainerLogs")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	tailStr := strconv.Itoa(opts.Tail)

	// Convert SinceSeconds to a time string
	var sinceStr string
	if opts.SinceSeconds > 0 {
		sinceTime := time.Now().Add(-time.Duration(opts.SinceSeconds) * time.Second)
		sinceStr = sinceTime.Format(time.RFC3339)
	} else if !opts.SinceTime.IsZero() {
		sinceStr = opts.SinceTime.Format(time.RFC3339)
	}

	dockerOpts := dockercontainer.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      sinceStr,
		Until:      "", // Assuming no end time
		Timestamps: opts.Timestamps,
		Follow:     opts.Follow,
		Tail:       tailStr,
		Details:    false, // Assuming no details are needed
	}

	stream, err := c.client.ContainerLogs(ctx, getUnderlyingContainerName(namespace, podName, containerName), dockerOpts)
	if err != nil {
		return nil, err
	}

	// Create a pipe to handle the combined stdout and stderr streams
	pr, pw := io.Pipe()

	go func() {
		_, err := stdcopy.StdCopy(pw, pw, stream)
		if err != nil {
			pw.CloseWithError(err)
		}
		if err := pw.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close pipe writer")
		}
	}()

	return pr, nil
}

// ExecInContainer executes a command in a specific container of a pod.
func (c *DockerClient) ExecInContainer(ctx context.Context, namespace, name, containerName string, cmd []string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.ExecInContainer")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	isTTY := attach != nil && attach.TTY()
	consoleSizeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	consoleSize := node.GetConsoleSize(consoleSizeCtx, attach)
	config := execConfigFromAttachIO(cmd, consoleSize, attach)

	id, err := c.client.ContainerExecCreate(ctx, getUnderlyingContainerName(namespace, name, containerName), config)
	if err != nil {
		return err
	}

	if isTTY {
		// Handle terminal resizing if TTY is enabled
		go node.HandleTerminalResizing(ctx, attach, func(size api.TermSize) error {
			return c.client.ContainerExecResize(ctx, id.ID, dockercontainer.ResizeOptions{
				Height: uint(size.Height),
				Width:  uint(size.Width),
			})
		})
	}

	if attach == nil {
		return nil
	}

	hr, err := c.client.ContainerExecAttach(ctx, id.ID, types.ExecStartCheck{
		Tty:         isTTY,
		ConsoleSize: consoleSize,
	})
	if err != nil {
		return err
	}
	defer hr.Close()

	if isTTY {
		// Handle I/O asynchronously if TTY is enabled
		return c.handleContainerIO(hr, attach.Stdin(), attach.Stdout(), attach.Stderr())
	} else {
		// Handle I/O synchronously if TTY is not enabled
		return c.handleContainerIOSync(hr, attach.Stdin(), attach.Stdout(), attach.Stderr())
	}
}

// AttachToContainer attaches to a specific container of a pod.
func (c *DockerClient) AttachToContainer(ctx context.Context, namespace, name, containerName string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.AttachToContainer")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if attach.TTY() {
		// Handle terminal resizing if TTY is enabled
		go node.HandleTerminalResizing(ctx, attach, func(size api.TermSize) error {
			return c.client.ContainerResize(ctx, getUnderlyingContainerName(namespace, name, containerName), dockercontainer.ResizeOptions{
				Height: uint(size.Height),
				Width:  uint(size.Width),
			})
		})
	}

	hr, err := c.client.ContainerAttach(ctx, getUnderlyingContainerName(namespace, name, containerName), dockercontainer.AttachOptions{
		Stream: attach.TTY(),
		Stdin:  attach.Stdin() != nil,
		Stdout: attach.Stdout() != nil,
		Stderr: attach.Stderr() != nil,
		Logs:   true,
	})
	if err != nil {
		return err
	}
	defer hr.Close()

	return c.handleContainerIO(hr, attach.Stdin(), attach.Stdout(), attach.Stderr())
}

// handleContainerIO manages the IO between attached streams and the container's streams.
func (c *DockerClient) handleContainerIO(hr types.HijackedResponse, stdin io.Reader, stdout, stderr io.Writer) error {
	doneCh := make(chan error, 1)

	if stdin != nil {
		go func() {
			_, err := io.Copy(hr.Conn, stdin)
			doneCh <- err
			doneCh <- hr.CloseWrite()
		}()
	}

	if stdout != nil {
		errWriter := stdout
		if stderr != nil {
			errWriter = stderr
		}
		go func() {
			_, err := stdcopy.StdCopy(stdout, errWriter, hr.Reader)
			doneCh <- err
		}()
	}

	return <-doneCh
}

// handleContainerIOSync manages the IO between attached streams and the container's streams synchronously.
func (c *DockerClient) handleContainerIOSync(hr types.HijackedResponse, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdin != nil {
		if _, err := io.Copy(hr.Conn, stdin); err != nil {
			return err
		}
		if err := hr.CloseWrite(); err != nil {
			return err
		}
	}

	if stdout != nil {
		errWriter := stdout
		if stderr != nil {
			errWriter = stderr
		}
		if _, err := stdcopy.StdCopy(stdout, errWriter, hr.Reader); err != nil {
			return err
		}
	}

	return nil
}

// IsContainerPresent checks if a specific container is present within a given pod.
func (c *DockerClient) IsContainerPresent(ctx context.Context, podNs, podName, containerName string) bool {
	_, span := trace.StartSpan(ctx, "DockerClient.IsContainerPresent")
	defer span.End()

	_, ok := c.data.GetContainerInfo(podNs, podName, containerName)
	return ok
}

func (c *DockerClient) GetContainerStats(ctx context.Context, podNs, podName string, containerName string) (s stats.ContainerStats, err error) {
	ctx, span := trace.StartSpan(ctx, "DockerClient.GetContainerStats")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	containerInfo, exists := c.data.GetContainerInfo(podNs, podName, containerName)
	if !exists {
		return s, errdefs.NotFound("container not found")
	}

	// Fetch stats for the container
	statsResp, err := c.client.ContainerStats(ctx, containerInfo.ID, false)
	if err != nil {
		return s, fmt.Errorf("failed to get container stats: %w", err)
	}
	defer func() {
		if closeErr := statsResp.Body.Close(); err != nil {
			err = errors.Join(err, fmt.Errorf("failed to close stats response body: %w", closeErr))
		}
	}()

	// Decode the stats JSON
	var statsData types.StatsJSON
	if err := json.NewDecoder(statsResp.Body).Decode(&statsData); err != nil {
		return s, fmt.Errorf("failed to decode stats: %w", err)
	}

	// Extract CPU usage in nano cores
	cpuDelta := statsData.CPUStats.CPUUsage.TotalUsage - statsData.PreCPUStats.CPUUsage.TotalUsage
	systemDelta := statsData.CPUStats.SystemUsage - statsData.PreCPUStats.SystemUsage
	var cpuUsageNanoCores *uint64
	if systemDelta > 0 && len(statsData.CPUStats.CPUUsage.PercpuUsage) > 0 {
		cpuUsage := uint64(float64(cpuDelta) / float64(systemDelta) * 1e9)
		cpuUsageNanoCores = &cpuUsage
	}

	// Extract CPU usage in core nano seconds
	cpuUsageCoreNanoSeconds := statsData.CPUStats.CPUUsage.TotalUsage

	// Extract memory usage
	memoryUsageBytes := statsData.MemoryStats.Usage
	memoryRSSBytes := statsData.MemoryStats.Stats["rss"]
	memoryWorkingSetBytes := statsData.MemoryStats.Usage - statsData.MemoryStats.Stats["cache"]

	// Prepare stats.ContainerStats
	time := metav1.NewTime(time.Now())
	return stats.ContainerStats{
		Name: containerName,
		CPU: &stats.CPUStats{
			Time:                 time,
			UsageNanoCores:       cpuUsageNanoCores,
			UsageCoreNanoSeconds: &cpuUsageCoreNanoSeconds,
		},
		Memory: &stats.MemoryStats{
			Time:            time,
			UsageBytes:      &memoryUsageBytes,
			WorkingSetBytes: &memoryWorkingSetBytes,
			RSSBytes:        &memoryRSSBytes,
		},
	}, nil
}

// getActiveContainers lists all active containers that match the specified name prefix.
func getActiveContainers(ctx context.Context, client *dockercl.Client) (map[k8stypes.NamespacedName][]string, error) {
	filterArgs := filters.NewArgs(filters.Arg("name", ContainerNamePrefix+"_*"))
	containers, err := client.ContainerList(ctx, dockercontainer.ListOptions{
		All:     true, // Include stopped containers
		Filters: filterArgs,
	})
	if err != nil {
		return nil, err
	}

	containerMap := make(map[k8stypes.NamespacedName][]string)
	for _, container := range containers {
		for _, name := range container.Names {
			nsName, err := extractNamespacedName(name)
			if err != nil {
				log.G(ctx).WithError(err).Warnf("failed to extract namespaced name from container name %s", name)
				continue
			}
			containerMap[nsName] = append(containerMap[nsName], container.ID)
		}
	}
	return containerMap, nil
}

// extractNamespacedName extracts the namespace and name from the underlying container name.
func extractNamespacedName(containerName string) (k8stypes.NamespacedName, error) {
	name := strings.TrimPrefix(containerName, "/"+ContainerNamePrefix+"_")
	parts := strings.SplitN(name, "_", 3)
	if len(parts) != 3 {
		return k8stypes.NamespacedName{}, fmt.Errorf("invalid container name format: %s", name)
	}
	return k8stypes.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}

// createDockerContainerConfig creates a Docker container configuration from Kubernetes container parameters.
func createDockerContainerConfig(params ContainerParams) *dockercontainer.Config {
	env := make([]string, len(params.Env))
	for i, e := range params.Env {
		env[i] = e.Name + "=" + e.Value
	}

	volumes := make(map[string]struct{}, len(params.Mounts))
	for _, m := range params.Mounts {
		volumes[m.ContainerPath] = struct{}{}
	}

	return &dockercontainer.Config{
		Hostname:   params.PodName,
		Env:        env,
		Entrypoint: params.Command,
		Cmd:        params.Args,
		Image:      params.Image,
		Volumes:    volumes,
		WorkingDir: params.WorkingDir,
		Tty:        params.TTY,
		OpenStdin:  params.Stdin,
		StdinOnce:  params.StdinOnce,
	}
}

// createDockerHostConfigFromMounts converts a list of Kubernetes volume mounts to Docker host configurations.
func createDockerHostConfigFromMounts(mounts []volumes.Mount) *dockercontainer.HostConfig {
	binds := make([]string, len(mounts))
	for i, m := range mounts {
		readonly := "rw"
		if m.ReadOnly {
			readonly = "ro"
		}
		binds[i] = fmt.Sprintf("%s:%s:%s", m.HostPath, m.ContainerPath, readonly)
	}

	return &dockercontainer.HostConfig{
		Binds: binds,
	}
}

// getUnderlyingContainerName generates a container name based on the pod's namespace, name, and container name.
func getUnderlyingContainerName(podNs, podName, containerName string) string {
	return fmt.Sprintf("%s_%s_%s_%s", ContainerNamePrefix, podNs, podName, containerName)
}

// containerStateFromDockerState converts Docker container states to internal ContainerState.
func containerStateFromDockerState(ctx context.Context, state *types.ContainerState) resource.ContainerState {
	if state == nil {
		return resource.ContainerState{Status: resource.ContainerStatusUnknown}
	}

	var status resource.ContainerStatus
	switch {
	case state.Running:
		status = resource.ContainerStatusRunning
	case state.Paused:
		status = resource.ContainerStatusPaused
	case state.Restarting:
		status = resource.ContainerStatusRestarting
	case state.OOMKilled:
		status = resource.ContainerStatusOOMKilled
	case state.Dead:
		status = resource.ContainerStatusDead
	case state.Status == "created":
		status = resource.ContainerStatusCreated
	default:
		status = resource.ContainerStatusUnknown
	}

	startedAt, err := time.Parse(time.RFC3339, state.StartedAt)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to parse container start time %s", state.StartedAt)
		startedAt = time.Time{} // Set to zero time if parsing fails
	}

	finishedAt, err := time.Parse(time.RFC3339, state.FinishedAt)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to parse container finish time %s", state.FinishedAt)
		finishedAt = time.Time{} // Set to zero time if parsing fails
	}

	return resource.ContainerState{
		Status:     status,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		ExitCode:   state.ExitCode,
		Error:      state.Error,
	}
}

func execConfigFromAttachIO(cmd []string, consoleSize *[2]uint, attach api.AttachIO) types.ExecConfig {
	config := types.ExecConfig{
		Cmd:         cmd,
		ConsoleSize: consoleSize,
	}

	if attach != nil {
		config.AttachStdin = attach.Stdin() != nil
		config.AttachStdout = attach.Stdout() != nil
		config.AttachStderr = attach.Stderr() != nil
		config.Tty = attach.TTY()
	}

	return config
}
