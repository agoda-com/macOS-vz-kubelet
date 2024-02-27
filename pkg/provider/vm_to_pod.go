package provider

import (
	"context"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *MacOSVZProvider) virtualMachineToPod(ctx context.Context, vm *vm.VirtualMachineInstance) (*v1.Pod, error) {
	pod, err := p.podsL.Pods(vm.Namespace).Get(vm.Name)
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
	pod, err := p.podsL.Pods(vm.Namespace).Get(vm.Name)
	if err != nil {
		return nil, err
	}

	state := vm.State()

	vmCreationTime := vm.CreationTime
	lastUpdateTime := vmCreationTime

	vmStartTime := vm.StartTime
	if vmStartTime.After(lastUpdateTime) {
		lastUpdateTime = vmStartTime
	}

	containerStatuses := make([]v1.ContainerStatus, 0, len(pod.Spec.Containers))
	containersList := pod.Spec.Containers

	for _, c := range containersList {
		started := true
		containerStatus := v1.ContainerStatus{
			Name:    c.Name,
			State:   *getContainerStateFromVMState(state, vmStartTime),
			Ready:   state == vz.VirtualMachineStateRunning,
			Started: &started,
			Image:   c.Image,
			ImageID: "",
			// ContainerID: util.GetContainerID(id, c.Name),
		}

		// Add to containerStatuses
		containerStatuses = append(containerStatuses, containerStatus)
	}

	phase := v1.PodUnknown
	reason := ""
	message := ""

	switch state {
	case vz.VirtualMachineStateStarting:
		phase = v1.PodPending
		reason = "Starting"
		message = "VM is starting"

	case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStopping:
		if vm.IPAddress == "" {
			phase = v1.PodPending
			reason = "Starting"
			message = "VM is starting"
		} else {
			phase = v1.PodRunning
			reason = "Running"
			message = "VM is running"
		}

	case vz.VirtualMachineStateStopped:
		phase = v1.PodSucceeded
		reason = "Stopped"
		message = "VM is stopped"

	case vz.VirtualMachineStateError:
		phase = v1.PodFailed
		reason = "Error"
		message = "VM is in an error state"
	}

	return &v1.PodStatus{
		Phase:             phase,
		Reason:            reason,
		Message:           message,
		Conditions:        getPodConditionsFromVMState(state, pod.CreationTimestamp.Time, vmCreationTime, lastUpdateTime),
		PodIP:             vm.IPAddress,
		ContainerStatuses: containerStatuses,
	}, nil
}

func getContainerStateFromVMState(state vz.VirtualMachineState, startTime time.Time) *v1.ContainerState {
	switch state {
	case vz.VirtualMachineStateStarting:
		return &v1.ContainerState{Waiting: &v1.ContainerStateWaiting{
			Reason:  "Starting",
			Message: "VM is starting",
		}}

	case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStopping:
		return &v1.ContainerState{Running: &v1.ContainerStateRunning{
			StartedAt: metav1.Time{Time: startTime},
		}}

	case vz.VirtualMachineStateStopped:
		return &v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
			ExitCode:   0,
			Reason:     "Stopped",
			Message:    "VM is stopped",
			StartedAt:  metav1.Time{Time: startTime},
			FinishedAt: metav1.Time{Time: time.Now()},
		}}

	case vz.VirtualMachineStateError:
		return &v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
			ExitCode:   1,
			Reason:     "Error",
			Message:    "VM is in an error state",
			StartedAt:  metav1.Time{Time: startTime},
			FinishedAt: metav1.Time{Time: time.Now()},
		}}
	}

	return nil
}

func getPodConditionsFromVMState(state vz.VirtualMachineState, scheduledTime, creationTime, lastUpdateTime time.Time) []v1.PodCondition {
	readyConditionStatus := v1.ConditionFalse
	readyConditionTime := creationTime

	switch state {
	case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStopping:
		readyConditionStatus = v1.ConditionTrue
		readyConditionTime = lastUpdateTime

	case vz.VirtualMachineStateError:
		readyConditionStatus = v1.ConditionFalse
		readyConditionTime = lastUpdateTime
	}

	return []v1.PodCondition{
		{
			Type:               v1.PodReady,
			Status:             readyConditionStatus,
			LastTransitionTime: metav1.Time{Time: readyConditionTime},
		},
		{
			Type:               v1.PodInitialized,
			Status:             v1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: creationTime},
		},
		{
			Type:               v1.PodScheduled,
			Status:             v1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: scheduledTime},
		},
	}
}
