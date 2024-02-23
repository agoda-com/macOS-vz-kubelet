package provider

import (
	"context"
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	v1 "k8s.io/api/core/v1"
)

func (p *MacOSVZProvider) virtualMachineToPod(ctx context.Context, vm *vm.VirtualMachineInstance, namespace, name string) (*v1.Pod, error) {
	pod, err := p.podsL.Pods(namespace).Get(name)
	if err != nil {
		return nil, err
	}

	updatedPod := pod.DeepCopy()

	podState, err := p.getPodStatusFromVirtualMachine(ctx, vm)
	if err != nil {
		return nil, err
	}

	updatedPod.Status = *podState

	return updatedPod, nil
}

func (p *MacOSVZProvider) getPodStatusFromVirtualMachine(ctx context.Context, vm *vm.VirtualMachineInstance) (*v1.PodStatus, error) {

	switch vm.State() {
	case vz.VirtualMachineStateStarting:
		// podStatus.Phase = v1.PodPending
		// started := true
		// podStatus.ContainerStatuses = []v1.ContainerStatus{
		// 	{
		// 		Name:    pod.Spec.Containers[0].Name,
		// 		State:   v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "Starting"}},
		// 		Ready:   false,
		// 		Started: &started,
		// 	},
		// }
		return &v1.PodStatus{
			Phase:   v1.PodPending,
			Reason:  "Starting",
			Message: "VM is starting",
		}, nil

	case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStopping:
		return &v1.PodStatus{
			Phase:   v1.PodRunning,
			Reason:  "Running",
			Message: "VM is running",
			// ContainerStatuses: []v1.ContainerStatus{
			// 	{
			// 		Name:    pod.Spec.Containers[0].Name,
			// 		State:   v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: pod.CreationTimestamp}},
			// 		Ready:   true,
			// 		Started: &started,
			// 	},
			// },
		}, nil

	case vz.VirtualMachineStateStopped:
		return &v1.PodStatus{
			Phase:   v1.PodSucceeded,
			Reason:  "Stopped",
			Message: "VM is stopped",
		}, nil

	case vz.VirtualMachineStateError:
		return &v1.PodStatus{
			Phase:   v1.PodFailed,
			Reason:  "Error",
			Message: "VM is in an error state",
		}, nil
	}

	return nil, fmt.Errorf("VM State %s not implemented by MacOS provider", vm.State())
}
