package resourcemanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Code-Hex/vz/v3"
	vmdata "github.com/agoda-com/macOS-vz-kubelet/internal/data/vm"
	"github.com/agoda-com/macOS-vz-kubelet/internal/node"
	"github.com/agoda-com/macOS-vz-kubelet/internal/sshconn"
	"github.com/agoda-com/macOS-vz-kubelet/internal/volumes"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/downloader"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	corev1 "k8s.io/api/core/v1"
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
	vms             vmdata.VirtualMachineData

	eventRecorder              event.EventRecorder
	networkInterfaceIdentifier string
	vmPermits                  chan struct{}
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
		vmPermits:                  make(chan struct{}, MaxVirtualMachines),
		vms:                        vmdata.NewVirtualMachineData(),
	}
}

// CreateVirtualMachine creates a new virtual machine with the specified parameters.
func (c *MacOSClient) CreateVirtualMachine(ctx context.Context, params VirtualMachineParams) (err error) {
	ctx, span := trace.StartSpan(ctx, "MacOSClient.CreateVirtualMachine")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	_, loaded := c.vms.LoadOrStore(params.Namespace, params.Name, vmdata.VirtualMachineInfo{
		Ref:      params.Image,
		Resource: resource.NewMacOSVirtualMachine(params.Env),
		// Persistent SSH connection shared across every exec for this VM; dials
		// lazily on first NewSession. Closed in DeleteVirtualMachine.
		SSHConn: sshconn.New(c.sshDialFunc(params.Namespace, params.Name)),
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
	_, found := c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = cancel
		return i
	})
	if !found {
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
	guard, acquireErr := c.acquirePermit(ctx, types.NamespacedName{Namespace: params.Namespace, Name: params.Name})
	if acquireErr != nil {
		err = acquireErr
		return
	}
	defer guard.Release(ctx)

	// Create the virtual machine instance
	vm, err := c.createVirtualMachineInstance(ctx, cfg, params)
	if err != nil {
		return
	}

	// Start the virtual machine
	err = vm.Start(ctx)
	if err != nil {
		c.eventRecorder.FailedToStartContainer(ctx, params.ContainerName, err)
		return
	}
	guard.Commit()
	c.eventRecorder.StartedContainer(ctx, params.ContainerName)

	if params.PostStartAction == nil {
		// Hookless pods gate Ready on the probe too (universal gating, see the mapper
		// in pkg/provider/virtualizationgroup_to_pod.go buildPodStatus). Assign the
		// function-level err (no block-scope shadow) so a permanent probe failure rides
		// the deferred finalizeVirtualMachineInfo to SetError -> State()=Failed -> PodFailed,
		// letting the controller recreate it - symmetric with the hook path below.
		if err = c.waitForVirtualMachineSSHReady(ctx, params.Namespace, params.Name); err != nil {
			// FailedPostStartProbe is the hookless analog of the hook path's
			// FailedPostStartHook. Suppress it on context.Canceled (pod deleted mid-probe is
			// normal teardown, not a failure; cf. the download path's guard above).
			if !errors.Is(err, context.Canceled) {
				c.eventRecorder.FailedPostStartProbe(ctx, params.ContainerName, err)
				log.G(ctx).WithError(err).Warn("hookless post-start SSH readiness probe failed; pod fails")
			}
			return
		}
		c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
			i.Resource.SetPostStartFinishedAt(time.Now())
			return i
		})
		return
	}

	// Execute the post-start action
	err = c.execPostStartAction(ctx, params.Namespace, params.Name, *params.PostStartAction)
	if err != nil {
		// err propagates via the deferred finalizeVirtualMachineInfo to SetError -> State()=Failed
		// regardless of the guard below; only the cluster event is suppressed on a mid-hook pod
		// delete (ctx cancel), a normal teardown not a hook failure (cf. the context.Canceled
		// guard on the download path above).
		if !errors.Is(err, context.Canceled) {
			c.eventRecorder.FailedPostStartHook(ctx, params.ContainerName, params.PostStartAction.Command, err)
		}
		return
	}
	// Record success so the status mapper flips the macOS container Ready next poll.
	c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.Resource.SetPostStartFinishedAt(time.Now())
		return i
	})
}

// finalizeVirtualMachineInfo updates the virtual machine info with the final result of the creation process.
func (c *MacOSClient) finalizeVirtualMachineInfo(ctx context.Context, params VirtualMachineParams, err error) {
	_, found := c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.DownloadCancelFunc = nil // indicate that download is no longer in progress
		if err != nil {
			i.Resource.SetError(err)
		}
		return i
	})
	if !found {
		log.G(ctx).Debug("virtual machine info expired")
	}
}

// createVirtualMachineInstance creates a new virtual machine instance with the specified parameters.
func (c *MacOSClient) createVirtualMachineInstance(ctx context.Context, cfg config.MacPlatformConfigurationOptions, params VirtualMachineParams) (*vm.VirtualMachineInstance, error) {
	vm, err := setupVM(ctx, cfg, params.UID, params.CPU, params.MemorySize, c.networkInterfaceIdentifier, params.Mounts)
	if err != nil {
		c.eventRecorder.FailedToCreateContainer(ctx, params.ContainerName, err)
		return nil, err
	}

	c.vms.Update(params.Namespace, params.Name, func(i vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		i.Resource.SetInstance(vm)
		return i
	})
	c.eventRecorder.CreatedContainer(ctx, params.ContainerName)

	return vm, nil
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

	info, ok := c.vms.Load(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return nil
	}

	// Delete must execute before releasePermit (defers are LIFO).
	// releasePermit handles the missing-entry case via non-blocking channel drain.
	defer c.vms.Delete(namespace, name)
	defer c.releasePermit(ctx, types.NamespacedName{Namespace: namespace, Name: name})

	if info.DownloadCancelFunc != nil {
		info.DownloadCancelFunc()
	}

	if instance := info.Resource.Instance(); instance != nil {
		err = c.stopVirtualMachine(ctx, instance, namespace, name, gracePeriod)
	}

	// Close the persistent SSH connection after stopVirtualMachine (graceful
	// shutdown SSHes over it).
	if info.SSHConn != nil {
		_ = info.SSHConn.Close()
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

	infos := c.vms.All()
	// simplify the map down to just the resource
	for key, info := range infos {
		vms[key] = info.Resource
	}

	return vms, nil
}

// getVirtualMachineInfo retrieves the virtual machine information.
func (c *MacOSClient) getVirtualMachineInfo(ctx context.Context, namespace, name string) (vmdata.VirtualMachineInfo, error) {
	info, ok := c.vms.Load(namespace, name)
	if !ok {
		log.G(ctx).Debugf("virtual machine not found for namespace %s and name %s", namespace, name)
		return vmdata.VirtualMachineInfo{}, errdefs.NotFound("virtual machine not found")
	}
	return info, nil
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
