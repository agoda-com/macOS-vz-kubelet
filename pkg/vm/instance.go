package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/netutil"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/Code-Hex/vz/v3"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
)

const (
	IPAddressLookupTimeout = 60 * time.Second
)

// VirtualMachineInstance represents a virtual machine instance.
type VirtualMachineInstance struct {
	IPAddress string

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time

	macAddr string
	config  *config.VirtualMachineConfiguration

	ipRetrievalCancelFunc context.CancelFunc

	*vz.VirtualMachine
}

// NewVirtualMachineInstance creates a new virtual machine instance.
func NewVirtualMachineInstance(ctx context.Context, config *config.VirtualMachineConfiguration) (i *VirtualMachineInstance, err error) {
	ctx, span := trace.StartSpan(ctx, "VirtualMachineInstance.NewVirtualMachineInstance")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	vm, err := vz.NewVirtualMachine(config.VirtualMachineConfiguration)
	if err != nil {
		return nil, err
	}

	instance := &VirtualMachineInstance{
		CreatedAt: time.Now(),

		macAddr: netutil.NormalizeMACAddress(config.MACAddress.String()),
		config:  config,

		VirtualMachine: vm,
	}

	// Start listening to state changes
	go instance.handleStateChanges(ctx)

	return instance, nil
}

// handleStateChanges handles state changes for the virtual machine instance.
func (i *VirtualMachineInstance) handleStateChanges(ctx context.Context) {
	logger := log.G(ctx)

	for {
		select {
		case state, ok := <-i.StateChangedNotify():
			if !ok {
				return
			}
			switch state {
			case vz.VirtualMachineStateRunning:
				currentTime := time.Now()
				i.StartedAt = &currentTime
				logger.Debug("Virtual machine instance has started")
				continue
			case vz.VirtualMachineStateStopped:
				// The virtual machine instance has finished
				currentTime := time.Now()
				i.FinishedAt = &currentTime
				logger.Debug("Virtual machine instance has finished")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// Start starts the virtual machine instance and retrieves the IP address.
func (i *VirtualMachineInstance) Start(ctx context.Context, opts ...vz.VirtualMachineStartOption) (err error) {
	ctx, span := trace.StartSpan(ctx, "VirtualMachineInstance.Start")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if err := i.VirtualMachine.Start(opts...); err != nil {
		return err
	}

	ctx, i.ipRetrievalCancelFunc = context.WithTimeout(ctx, IPAddressLookupTimeout)
	defer i.ipRetrievalCancelFunc()
	err = i.retrieveIPAddress(ctx)
	if err != nil {
		// kill the virtual machine instance if we failed to retrieve the IP address
		_ = i.VirtualMachine.Stop()
		return fmt.Errorf("failed to retrieve IP address: %w", err)
	}

	return nil
}

// retrieveIPAddress retrieves the IP address of the virtual machine instance.
func (i *VirtualMachineInstance) retrieveIPAddress(ctx context.Context) (err error) {
	ctx, span := trace.StartSpan(ctx, "VirtualMachineInstance.retrieveIPAddress")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if i.config.NetworkInterface != "" {
		// Try to capture the IP using TCP dump method
		ip, err := netutil.CaptureIPWithTcpDump(ctx, i.config.NetworkInterface, i.macAddr)
		if err == nil {
			i.IPAddress = ip
			return nil
		}

		log.G(ctx).WithError(err).Warn("Unable to capture IP using tcpdump")
	}

	// Look the IP address up in the ARP table, or in the DHCP leases (see netutil.RetrieveVMIP)
	ip, source, err := netutil.RetrieveVMIP(ctx, i.macAddr)
	if err != nil {
		return fmt.Errorf("failed to retrieve IP address: %w", err)
	}
	log.G(ctx).WithField("ip", ip).WithField("source", source).WithField("mac", i.macAddr).Info("found the VM's IP address")
	i.IPAddress = ip
	return nil
}

// Stop stops the virtual machine instance and removes the overlay files if they exist.
func (i *VirtualMachineInstance) Stop(ctx context.Context) (err error) {
	ctx, span := trace.StartSpan(ctx, "VirtualMachineInstance.Stop")
	logger := log.G(ctx)
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if i.IPAddress == "" && i.ipRetrievalCancelFunc != nil {
		// cancel the IP retrieval context if it is still running
		i.ipRetrievalCancelFunc()
	}

	if i.State() != vz.VirtualMachineStateStopped {
		// force stop VM if it is not stopped already
		logger.Debug("Force stopping VM")
		err = i.VirtualMachine.Stop()
	}

	overlayBlockStoragePath, overlayAuxiliaryStoragePath, ok := i.config.GetOverlays()
	logger.Debugf("Overlay block storage path: %s, overlay auxiliary storage path: %s", overlayBlockStoragePath, overlayAuxiliaryStoragePath)
	if ok {
		logger.Debugf("Removing overlay files: %s, %s", overlayBlockStoragePath, overlayAuxiliaryStoragePath)
		if rmErr := os.Remove(overlayBlockStoragePath); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		if rmErr := os.Remove(overlayAuxiliaryStoragePath); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}

	return err
}
