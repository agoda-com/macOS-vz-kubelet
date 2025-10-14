package resourcemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Code-Hex/vz/v3"
	vmdata "github.com/agoda-com/macOS-vz-kubelet/internal/data/vm"
	vzio "github.com/agoda-com/macOS-vz-kubelet/internal/io"
	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// MaxVirtualMachines is the maximum number of virtual machines that can be created.
	// This is a kernel level limitation by Apple and is enforced within Virtualization.framework.
	MaxVirtualMachines = 2
)

// VirtualMachineParams encapsulates the parameters required for creating a virtual machine.
type VirtualMachineParams struct {
	UID              string
	Image            string
	Namespace        string
	Name             string
	ContainerName    string
	CPU              uint
	MemorySize       uint64
	Mounts           []volumes.Mount
	Env              []corev1.EnvVar
	PostStartAction  *resource.ExecAction
	IgnoreImageCache bool
	RegistryCreds    resource.RegistryCredentials
}

// MacOSClient manages the lifecycle of macOS virtual machines.
type MacOSClient struct {
	downloadManager *downloader.Manager
	data            vmdata.VirtualMachineData

	eventRecorder              event.EventRecorder
	networkInterfaceIdentifier string
}

// NewMacOSClient initializes a new MacOSClient instance.
func NewMacOSClient(ctx context.Context, eventRecorder event.EventRecorder, networkInterfaceIdentifier, cachePath string) *MacOSClient {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.NewMacOSClient")
	_ = span.WithFields(ctx, log.Fields{
		"networkInterfaceIdentifier": networkInterfaceIdentifier,
		"cachePath":                  cachePath,
	})
	defer span.End()

	return &MacOSClient{
		eventRecorder:              eventRecorder,
		networkInterfaceIdentifier: networkInterfaceIdentifier,
		downloadManager:            downloader.NewManager(eventRecorder, cachePath),
	}
}

// CreateVirtualMachine creates a new virtual machine with the specified parameters.
func (c *MacOSClient) CreateVirtualMachine(ctx context.Context, params VirtualMachineParams) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.CreateVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	_, loaded := c.data.GetOrCreateVirtualMachineInfo(params.Namespace, params.Name, vmdata.VirtualMachineInfo{
		Ref:      params.Image,
		Resource: resource.NewMacOSVirtualMachine(params.Env),
	})
	if loaded {
		return errdefs.AsInvalidInput(fmt.Errorf("virtual machine already exists"))
	}

	c.eventRecorder.PullingImage(ctx, params.Image, params.ContainerName)

	// Start the asynchronous creation of the virtual machine
	go c.handleVirtualMachineCreation(ctx, params)

	return nil
}

func (c *MacOSClient) handleVirtualMachineCreation(ctx context.Context, params VirtualMachineParams) {
	var err error
	ctx, span := trace.StartSpan(ctx, "MacOSClient.handleVirtualMachineCreation")
	defer func() {
		c.finalizeVirtualMachineInfo(ctx, params, err)
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Debugf("Creating virtual machine with params: %+v", params)

	// Manage download
	downloadCtx, cancel := context.WithCancel(ctx) // create a new context to manage the download
	defer cancel()
	_, updated := c.data.UpdateVirtualMachineInfo(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = cancel
		return i
	})
	if !updated {
		logger.Debug("virtual machine info expired")
		return
	}

	cfg, duration, err := c.downloadManager.Download(downloadCtx, params.Image, params.IgnoreImageCache, params.RegistryCreds)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			// Only log the error if it's not due to context cancellation
			// to avoid spamming the cluster events with canceled downloads.
			c.eventRecorder.BackOffPullImage(ctx, params.Image, params.ContainerName, err)
		}
		return
	}

	// Log the successful image pull event
	c.eventRecorder.PulledImage(ctx, params.Image, params.ContainerName, duration.String())
	logger.Debug(cfg)

	// Wait until resources are available to proceed with the virtual machine creation
	if err = c.waitForCreationProceed(ctx); err != nil {
		return
	}

	c.data.IncrementAllocatedToRunCounter()

	// Create the virtual machine instance
	vm, err := c.createVirtualMachineInstance(ctx, cfg, params)
	if err != nil {
		c.data.DecrementAllocatedToRunCounter()
		return
	}

	// Start the virtual machine
	err = vm.Start(ctx)
	if err != nil {
		c.eventRecorder.FailedToStartContainer(ctx, params.ContainerName, err)
		c.data.DecrementAllocatedToRunCounter()
		return
	}

	c.eventRecorder.StartedContainer(ctx, params.ContainerName)

	if params.PostStartAction == nil {
		// No post-start action specified, return early
		return
	}

	// Execute the post-start action
	err = c.execPostStartAction(ctx, params.Namespace, params.Name, *params.PostStartAction)
	if err != nil {
		c.eventRecorder.FailedPostStartHook(ctx, params.ContainerName, params.PostStartAction.Command, err)
	}
}

// finalizeVirtualMachineInfo updates the virtual machine info with the final result of the creation process.
func (c *MacOSClient) finalizeVirtualMachineInfo(ctx context.Context, params VirtualMachineParams, err error) {
	_, updated := c.data.UpdateVirtualMachineInfo(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = nil // indicate that download is no longer in progress
		if err != nil {
			i.Resource.SetError(err)
		}
		return i
	})
	if !updated {
		log.G(ctx).Debug("virtual machine info expired")
	}
}

// waitForCreationProceed blocks until it's safe to proceed with the virtual machine creation.
func (c *MacOSClient) waitForCreationProceed(ctx context.Context) error {
	// Since kubelet limits number of pods based on the Virtualization.framework limits already,
	// it's safe to assume that the reason for not being able to create a new VM is that we have
	// some of them in Terminating state (graceful shutdown) and we need to wait for them to finish.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if c.canProceedWithVirtualMachineCreation() {
			return nil
		}

		log.G(ctx).Debug("waiting for resources to be available")
		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// createVirtualMachineInstance creates a new virtual machine instance with the specified parameters.
func (c *MacOSClient) createVirtualMachineInstance(ctx context.Context, cfg config.MacPlatformConfigurationOptions, params VirtualMachineParams) (*vm.VirtualMachineInstance, error) {
	vm, err := setupVM(ctx, cfg, params.UID, params.CPU, params.MemorySize, c.networkInterfaceIdentifier, params.Mounts)
	if err != nil {
		c.eventRecorder.FailedToCreateContainer(ctx, params.ContainerName, err)
		return nil, err
	}

	c.data.UpdateVirtualMachineInfo(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.Resource.SetInstance(vm)
		return i
	})
	c.eventRecorder.CreatedContainer(ctx, params.ContainerName)

	return vm, nil
}

// execPostStartAction executes the post-start action inside the virtual machine.
func (c *MacOSClient) execPostStartAction(ctx context.Context, namespace, name string, action resource.ExecAction) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.execPostStart")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace": namespace,
		"name":      name,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Debugf("Executing post-start action: %+v", action)
	logger.Info("Virtual machine is running, executing post-start command")

	ctx, cancel := context.WithTimeout(ctx, action.TimeoutDuration)
	defer cancel() // Ensure context is cancelled to avoid leaking resources

	err = c.ExecInVirtualMachine(ctx, namespace, name, action.Command, node.DiscardingExecIO())
	if ctx.Err() != nil {
		// Ensure context errors are getting priority to be reported
		return ctx.Err()
	}
	return err
}

// DeleteVirtualMachine stops and deletes the specified virtual machine.
func (c *MacOSClient) DeleteVirtualMachine(ctx context.Context, namespace string, name string, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.DeleteVirtualMachine")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace":   namespace,
		"name":        name,
		"gracePeriod": gracePeriod,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, ok := c.data.GetVirtualMachineInfo(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return nil
	}
	defer c.data.RemoveVirtualMachineInfo(namespace, name)

	if info.DownloadCancelFunc != nil {
		info.DownloadCancelFunc()
	}

	if instance := info.Resource.Instance(); instance != nil {
		err = c.stopVirtualMachine(ctx, instance, namespace, name, gracePeriod)
		if err == nil {
			c.data.DecrementAllocatedToRunCounter()
		}
	}

	return err
}

// stopVirtualMachine stops the virtual machine instance.
func (c *MacOSClient) stopVirtualMachine(ctx context.Context, instance *vm.VirtualMachineInstance, namespace, name string, gracePeriod int64) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.stopVirtualMachine")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace":   namespace,
		"name":        name,
		"gracePeriod": gracePeriod,
	})
	logger := log.G(ctx)
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	// Attempt to send a graceful shutdown request, which unfortunately
	// is unsupported by Virtualization.framework as of today.
	if instance.State() == vz.VirtualMachineStateRunning && gracePeriod > 0 {
		logger.Info("Stopping virtual machine gracefully")

		stopCtx, cancel := context.WithTimeout(ctx, time.Duration(gracePeriod)*time.Second)
		defer cancel()
		if err := c.gracefulShutdown(stopCtx, instance, namespace, name); err != nil {
			logger.WithError(err).Warn("Failed to gracefully shutdown VM, will force stop it instead")
		}
	}

	return instance.Stop(ctx)
}

// gracefulShutdown attempts to gracefully shutdown the virtual machine.
func (c *MacOSClient) gracefulShutdown(ctx context.Context, instance *vm.VirtualMachineInstance, namespace, name string) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.gracefulShutdown")
	ctx = span.WithFields(ctx, log.Fields{
		"namespace": namespace,
		"name":      name,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	gracefulShutdownCmd := []string{
		"sh", "-c",
		// Disable network interface and shutdown the VM in the background so ssh connection
		// is not interrupted. This will not work if sudo requires a password.
		"sudo -n true && ((nohup sudo ipconfig set en0 none; sudo shutdown -h now) > /dev/null 2>&1 & disown)",
	}

	err = c.ExecInVirtualMachine(ctx, namespace, name, gracefulShutdownCmd, node.DiscardingExecIO())
	if err != nil {
		return err
	}

	// Check instance.FinishedAt until it's not nil or context is done
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if instance.FinishedAt != nil {
				return nil
			}
		}
	}
}

// GetVirtualMachine retrieves the specified virtual machine.
func (c *MacOSClient) GetVirtualMachine(ctx context.Context, namespace string, name string) (i resource.MacOSVirtualMachine, err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.GetVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, err := c.getVirtualMachineInfo(ctx, namespace, name)
	if err != nil {
		return resource.MacOSVirtualMachine{}, err
	}

	return info.Resource, nil
}

// GetVirtualMachineListResult retrieves all virtual machines managed by the client.
func (c *MacOSClient) GetVirtualMachineListResult(ctx context.Context) (map[types.NamespacedName]resource.MacOSVirtualMachine, error) {
	_, span := trace.StartSpan(ctx, "MacOSClient.GetVirtualMachineListResult")
	defer span.End()

	vms := make(map[types.NamespacedName]resource.MacOSVirtualMachine)

	infos := c.data.ListVirtualMachines()
	// simplify the map down to just the resource
	for key, info := range infos {
		vms[key] = info.Resource
	}

	return vms, nil
}

// ExecInVirtualMachine executes a command inside a specified virtual machine.
func (c *MacOSClient) ExecInVirtualMachine(ctx context.Context, namespace, name string, cmd []string, attach api.AttachIO) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.ExecInVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	info, err := c.getVirtualMachineInfo(ctx, namespace, name)
	if err != nil {
		return err
	}

	client, err := establishVirtualMachineSshConn(ctx, info.Resource)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close SSH client")
		}
	}()

	go func() {
		// Make sure connection is closed when context is done
		<-ctx.Done()
		_ = client.Close()
	}()

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() {
		_ = session.Close()
	}()

	// We establish stdinPipe here instead of directly assigning attach.Stdin() to the session
	// because we need to monitor any interruptions to stdin in order to properly close the session.
	// For example, if the interactive terminal is closed without exiting the session, the session
	// would be left hanging.
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer func() {
		_ = stdinPipe.Close()
	}()

	macOSSession := vzssh.NewMacOSSession(session, attach, stdinPipe)
	if err = macOSSession.SetupSessionIO(ctx); err != nil {
		return fmt.Errorf("failed to setup session IO: %w", err)
	}

	return macOSSession.ExecuteCommand(ctx, info.Resource.Env(), cmd)
}

// GetVirtualMachineStats retrieves the stats of the specified virtual machine.
func (c *MacOSClient) GetVirtualMachineStats(ctx context.Context, namespace, name string) (stats.ContainerStats, error) {
	// Combined script for collecting all required stats
	cmd := []string{
		`cpuUsageNanoCores=$(top -l 1 | awk '/CPU usage/ {print ($3+$5)*10000000}' | sed 's/%//g')`,
		`cpuUsageNanoCores=$(printf "%.0f" "$cpuUsageNanoCores")`,

		`cpuUsageCoreNanoSeconds=$(echo "$(sysctl -n hw.ncpu) * $(( $(date +%s) - $(sysctl -n kern.boottime | awk -F'[ ,]' '{print $4}') )) * 1000000000" | bc -l)`,
		`cpuUsageCoreNanoSeconds=$(printf "%.0f" "$cpuUsageCoreNanoSeconds")`,

		`memoryUsageBytes=$(vm_stat | awk '/Pages active/ {active=$3} /Pages wired down/ {wired=$4} END {print (active+wired)*4096}')`,
		`memoryRssBytes=$(vm_stat | awk '/Pages active/ {print $3*4096}')`,
		`memoryWorkingSetBytes=$(vm_stat | awk '/Pages active/ {active=$3} /Pages speculative/ {speculative=$4} END {print (active-speculative)*4096}')`,

		`echo "{\"cpuUsageNanoCores\": $cpuUsageNanoCores, \"cpuUsageCoreNanoSeconds\": $cpuUsageCoreNanoSeconds, \"memoryUsageBytes\": $memoryUsageBytes, \"memoryRssBytes\": $memoryRssBytes, \"memoryWorkingSetBytes\": $memoryWorkingSetBytes}"`,
	}

	// Capture command output
	stdout := &bytes.Buffer{}
	buf := vzio.NewBufferWriteCloser(stdout)
	attach := node.NewExecIO(false, nil, buf, buf, nil)

	// Execute the script in the VM
	if err := c.ExecInVirtualMachine(ctx, namespace, name, cmd, attach); err != nil {
		return stats.ContainerStats{}, fmt.Errorf("error executing script: %w", err)
	}

	// Parse JSON output
	statsData, err := parseStatsJSON(stdout.Bytes())
	if err != nil {
		return stats.ContainerStats{}, fmt.Errorf("error parsing JSON output: %w", err)
	}

	// Prepare stats.ContainerStats
	time := metav1.NewTime(time.Now())
	return stats.ContainerStats{
		CPU: &stats.CPUStats{
			Time:                 time,
			UsageNanoCores:       statsData.CPUUsageNanoCores,
			UsageCoreNanoSeconds: statsData.CPUUsageCoreNanoSeconds,
		},
		Memory: &stats.MemoryStats{
			Time:            time,
			UsageBytes:      statsData.MemoryUsageBytes,
			WorkingSetBytes: statsData.MemoryWorkingSetBytes,
			RSSBytes:        statsData.MemoryRSSBytes,
		},
	}, nil
}

type vmStatsData struct {
	CPUUsageNanoCores       json.Number `json:"cpuUsageNanoCores"`
	CPUUsageCoreNanoSeconds json.Number `json:"cpuUsageCoreNanoSeconds"`
	MemoryUsageBytes        json.Number `json:"memoryUsageBytes"`
	MemoryRSSBytes          json.Number `json:"memoryRssBytes"`
	MemoryWorkingSetBytes   json.Number `json:"memoryWorkingSetBytes"`
}

type parsedVMStatsData struct {
	CPUUsageNanoCores       *uint64 `json:"cpuUsageNanoCores"`
	CPUUsageCoreNanoSeconds *uint64 `json:"cpuUsageCoreNanoSeconds"`
	MemoryUsageBytes        *uint64 `json:"memoryUsageBytes"`
	MemoryRSSBytes          *uint64 `json:"memoryRssBytes"`
	MemoryWorkingSetBytes   *uint64 `json:"memoryWorkingSetBytes"`
}

func parseStatsJSON(data []byte) (*parsedVMStatsData, error) {
	// Unmarshal into intermediate structure
	var statsData vmStatsData
	if err := json.Unmarshal(data, &statsData); err != nil {
		return nil, err
	}

	// Conversion function for json.Number to *uint64
	convert := func(num json.Number) (*uint64, error) {
		val, err := num.Int64()
		if err != nil {
			return nil, err
		}
		uval := uint64(val)
		return &uval, nil
	}

	// Populate the final ParsedVMStatsData struct
	parsedData := &parsedVMStatsData{}
	var err error

	if parsedData.CPUUsageNanoCores, err = convert(statsData.CPUUsageNanoCores); err != nil {
		return nil, fmt.Errorf("cpuUsageNanoCores: %w", err)
	}
	if parsedData.CPUUsageCoreNanoSeconds, err = convert(statsData.CPUUsageCoreNanoSeconds); err != nil {
		return nil, fmt.Errorf("cpuUsageCoreNanoSeconds: %w", err)
	}
	if parsedData.MemoryUsageBytes, err = convert(statsData.MemoryUsageBytes); err != nil {
		return nil, fmt.Errorf("memoryUsageBytes: %w", err)
	}
	if parsedData.MemoryRSSBytes, err = convert(statsData.MemoryRSSBytes); err != nil {
		return nil, fmt.Errorf("memoryRssBytes: %w", err)
	}
	if parsedData.MemoryWorkingSetBytes, err = convert(statsData.MemoryWorkingSetBytes); err != nil {
		return nil, fmt.Errorf("memoryWorkingSetBytes: %w", err)
	}

	return parsedData, nil
}

// getVirtualMachineInfo retrieves the virtual machine information.
func (c *MacOSClient) getVirtualMachineInfo(ctx context.Context, namespace, name string) (vmdata.VirtualMachineInfo, error) {
	info, ok := c.data.GetVirtualMachineInfo(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return vmdata.VirtualMachineInfo{}, errdefs.NotFound("virtual machine not found")
	}
	return info, nil
}

// canProceedWithVirtualMachineCreation determines if it's safe to proceed with the virtual machine creation.
// It checks whether the current number of added virtual machines has not exceeded the limit.
func (c *MacOSClient) canProceedWithVirtualMachineCreation() bool {
	// the check happens when new VM info is added
	return c.data.AllocatedToRunCount() < MaxVirtualMachines
}

// setupVM creates a new virtual machine instance with the given parameters.
func setupVM(ctx context.Context, cfg config.MacPlatformConfigurationOptions, uid string, cpu uint, memorySize uint64, networkInterfaceIdentifier string, mounts []volumes.Mount) (*vm.VirtualMachineInstance, error) {
	log.G(ctx).Debugf("Creating virtual machine with CPU: %d, memory: %d, network interface: %s, mounts: %+v", cpu, memorySize, networkInterfaceIdentifier, mounts)
	platformConfig, err := config.NewPlatformConfiguration(ctx, cfg, true, uid)
	if err != nil {
		return nil, fmt.Errorf("failed to create platform configuration: %w", err)
	}

	vmConfig, err := config.NewVirtualMachineConfiguration(ctx, platformConfig, cpu, memorySize, networkInterfaceIdentifier, mounts)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine configuration: %w", err)
	}

	vmInstance, err := vm.NewVirtualMachineInstance(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine instance: %w", err)
	}

	return vmInstance, nil
}

// establishVirtualMachineSshConn establishes an SSH connection to the specified virtual machine.
func establishVirtualMachineSshConn(ctx context.Context, vm resource.MacOSVirtualMachine) (*ssh.Client, error) {
	ipAddr := vm.IPAddress()
	if ipAddr == "" {
		return nil, errdefs.InvalidInputf("virtual machine does not have an IP address")
	}

	sshUser, sshPassword, err := getSSHCredentials()
	if err != nil {
		return nil, err
	}

	// Setup SSH client configuration
	config := &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(sshPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// Establish SSH connection with keepalive
	conn, err := vzssh.DialContext(ctx, "tcp", vm.IPAddress()+":22", config)
	if err != nil {
		return nil, err
	}

	go vzssh.SendKeepalive(ctx, conn)

	return conn, nil
}

// getSSHCredentials retrieves SSH credentials from environment variables.
func getSSHCredentials() (string, string, error) {
	sshUser := os.Getenv("VZ_SSH_USER")
	sshPassword := os.Getenv("VZ_SSH_PASSWORD")
	if sshUser == "" || sshPassword == "" {
		return "", "", errdefs.InvalidInputf("VZ_SSH_USER and VZ_SSH_PASSWORD env variables are required")
	}
	return sshUser, sshPassword, nil
}
