package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// virtualizationGroupToPod converts a VirtualizationGroup to a Kubernetes Pod status.
func (p *MacOSVZProvider) virtualizationGroupToPod(ctx context.Context, vg *client.VirtualizationGroup, namespace, name string) (*corev1.Pod, error) {
	pod, err := p.podLister.Pods(namespace).Get(name)
	if err != nil {
		return nil, err
	}

	podState := p.buildPodStatus(ctx, vg, pod)
	updatedPod := pod.DeepCopy()
	updatedPod.Status = *podState

	return updatedPod, nil
}

// buildPodStatus constructs the pod's status from the provided virtualization group.
func (p *MacOSVZProvider) buildPodStatus(_ context.Context, vg *client.VirtualizationGroup, pod *corev1.Pod) *corev1.PodStatus {
	var firstContainerStartTime, lastUpdateTime time.Time

	macOSVM := vg.MacOSVirtualMachine
	groupContainers := vg.Containers

	podIp := macOSVM.IPAddress()
	containerStatuses := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))

	macOSPostStartSatisfied := true
	// postStartFinishedAt is the post-start readiness marker's wall-clock: it stamps the
	// Ready LastTransitionTime, since the gate clears when the SSH probe (plus the exec
	// hook when one exists) finishes, not at VM start. Nil until the probe sets it,
	// leaving those timestamps unchanged.
	var postStartFinishedAt *time.Time

	for i, c := range pod.Spec.Containers {
		// vz: always assume that first container is macOS container
		if i == 0 {
			state := macOSVM.State()
			// Gate readiness on the post-start marker for EVERY macOS container[0], not
			// just hook pods: it is set when the SSH readiness probe succeeds (hookless
			// and hook paths both run it), and again when the optional exec hook finishes.
			// Until then the pod is NotReady/NotStarted even with an IP.
			postStartFinishedAt = macOSVM.PostStartFinishedAt()
			macOSPostStartSatisfied = postStartFinishedAt != nil
			started := podIp != "" && macOSPostStartSatisfied
			ready := state == resource.VirtualMachineStateRunning && macOSPostStartSatisfied

			if startedAt := macOSVM.StartedAt(); startedAt != nil {
				firstContainerStartTime = *startedAt
				lastUpdateTime = firstContainerStartTime
			}
			if finishedAt := macOSVM.FinishedAt(); finishedAt != nil {
				lastUpdateTime = *finishedAt
			}

			containerStatus := corev1.ContainerStatus{
				Name:         c.Name,
				State:        vmToContainerState(macOSVM, pod.CreationTimestamp.Time),
				Ready:        ready,
				Started:      &started,
				RestartCount: 0,
				Image:        c.Image,
				ImageID:      "",
				ContainerID:  utils.GetContainerID(resource.MacOSRuntime, c.Name),
			}

			// Add the container status to the list.
			containerStatuses = append(containerStatuses, containerStatus)
			continue
		}

		container, err := getContainerWithName(c.Name, groupContainers)
		if err != nil {
			continue
		}

		state := container.State.Status
		started := state == resource.ContainerStatusRunning
		ready := state == resource.ContainerStatusRunning

		containerStatus := corev1.ContainerStatus{
			Name:         c.Name,
			State:        containerToContainerState(container, pod.CreationTimestamp.Time),
			Ready:        ready,
			Started:      &started,
			RestartCount: 0,
			Image:        c.Image,
			ImageID:      "",
			ContainerID:  utils.GetContainerID(resource.ContainerRuntime, c.Name),
		}

		startedAt := container.State.StartedAt
		finishedAt := container.State.FinishedAt
		if !startedAt.IsZero() &&
			(startedAt.Before(firstContainerStartTime) || firstContainerStartTime.IsZero()) {
			firstContainerStartTime = startedAt
		}
		if startedAt.After(lastUpdateTime) {
			lastUpdateTime = startedAt
		}
		if !finishedAt.IsZero() &&
			(finishedAt.After(lastUpdateTime) || lastUpdateTime.IsZero()) {
			lastUpdateTime = finishedAt
		}

		// Add the container status to the list.
		containerStatuses = append(containerStatuses, containerStatus)
	}

	var startTime *metav1.Time
	if !firstContainerStartTime.IsZero() {
		startTime = &metav1.Time{Time: firstContainerStartTime}
	}
	return &corev1.PodStatus{
		Phase:             getPodPhaseFromVirtualizationGroup(vg),
		Conditions:        getPodConditionsFromVirtualizationGroup(vg, pod.CreationTimestamp.Time, firstContainerStartTime, lastUpdateTime, macOSPostStartSatisfied, postStartFinishedAt),
		Message:           "",
		Reason:            "",
		HostIP:            p.nodeIPAddress,
		PodIP:             podIp,
		StartTime:         startTime,
		ContainerStatuses: containerStatuses,
	}
}

// vmToContainerState converts the macOS VM state to a Kubernetes container state.
func vmToContainerState(vm resource.VirtualMachine, podCreationTime time.Time) corev1.ContainerState {
	startTime := podCreationTime
	finishTime := podCreationTime
	if startedAt := vm.StartedAt(); startedAt != nil {
		startTime = *startedAt
	}
	if finishedAt := vm.FinishedAt(); finishedAt != nil {
		finishTime = *finishedAt
	}

	switch vm.State() {
	case resource.VirtualMachineStatePreparing:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "Downloading",
				Message: "VM is downloading image from the registry",
			},
		}
	case resource.VirtualMachineStateStarting:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "Starting",
				Message: "VM is starting",
			},
		}
	case resource.VirtualMachineStateRunning:
		return corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(startTime),
			},
		}
	case resource.VirtualMachineStateTerminating, resource.VirtualMachineStateTerminated:
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   0,
				Reason:     "Completed",
				Message:    "VM is stopped",
				StartedAt:  metav1.NewTime(startTime),
				FinishedAt: metav1.NewTime(finishTime),
			},
		}
	case resource.VirtualMachineStateFailed:
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   1,
				Reason:     "Error",
				Message:    fmt.Sprintf("VM has failed: %v", vm.Error()),
				StartedAt:  metav1.NewTime(startTime),
				FinishedAt: metav1.NewTime(finishTime),
			},
		}
	}

	return corev1.ContainerState{}
}

// containerToContainerState converts the container state to a Kubernetes container state.
func containerToContainerState(container resource.Container, podCreationTime time.Time) corev1.ContainerState {
	startTime := podCreationTime
	finishTime := podCreationTime
	if !container.State.StartedAt.IsZero() {
		startTime = container.State.StartedAt
	}
	if !container.State.FinishedAt.IsZero() {
		finishTime = container.State.FinishedAt
	}

	switch container.State.Status {
	case resource.ContainerStatusWaiting:
		if container.State.Error != "" {
			// mimic standard kubernetes behavior for
			// container error during pre-running stage
			return corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "Error",
					Message: container.State.Error,
				},
			}
		}
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating",
			},
		}
	case resource.ContainerStatusCreated:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ContainerCreated",
				Message: "Container has been created",
			},
		}
	case resource.ContainerStatusRunning:
		return corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(startTime),
			},
		}
	case resource.ContainerStatusPaused:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ContainerPaused",
				Message: "Container is paused",
			},
		}
	case resource.ContainerStatusRestarting:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ContainerRestarting",
				Message: "Container is restarting",
			},
		}
	case resource.ContainerStatusOOMKilled:
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   137,
				Reason:     "OOMKilled",
				Message:    "Container was killed due to out of memory",
				StartedAt:  metav1.NewTime(startTime),
				FinishedAt: metav1.NewTime(finishTime),
			},
		}
	case resource.ContainerStatusDead:
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   int32(container.State.ExitCode),
				Reason:     "ContainerDead",
				Message:    container.State.Error,
				StartedAt:  metav1.NewTime(startTime),
				FinishedAt: metav1.NewTime(finishTime),
			},
		}
	default:
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   int32(container.State.ExitCode),
				Reason:     "Unknown",
				Message:    container.State.Error,
				StartedAt:  metav1.NewTime(startTime),
				FinishedAt: metav1.NewTime(finishTime),
			},
		}
	}
}

// getContainerWithName finds and returns a container with the specified name from a list of containers.
func getContainerWithName(name string, list []resource.Container) (resource.Container, error) {
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}

	return resource.Container{}, fmt.Errorf("container %s not found", name)
}

// getPodPhaseFromVirtualizationGroup determines the pod phase based on the state of the virtualization group.
func getPodPhaseFromVirtualizationGroup(vg *client.VirtualizationGroup) corev1.PodPhase {
	// Get the macOS VM and group containers
	macOSVM := vg.MacOSVirtualMachine
	groupContainers := vg.Containers
	hasIP := macOSVM.IPAddress() != ""

	// Determine the pod phase based on the macOS VM state
	switch macOSVM.State() {
	case resource.VirtualMachineStatePreparing, resource.VirtualMachineStateStarting:
		return corev1.PodPending
	case resource.VirtualMachineStateTerminated:
		return corev1.PodSucceeded
	case resource.VirtualMachineStateFailed:
		return corev1.PodFailed
	case resource.VirtualMachineStateTerminating, resource.VirtualMachineStateRunning:
		// If there are no group containers, consider VM as a single source of truth
		if len(groupContainers) == 0 {
			if !hasIP {
				return corev1.PodPending
			}
			return corev1.PodRunning
		}
	}

	// Determine the pod phase based on the container statuses
	allContainersRunning := true
	for _, container := range groupContainers {
		switch container.State.Status {
		case resource.ContainerStatusWaiting, resource.ContainerStatusCreated:
			return corev1.PodPending
		case resource.ContainerStatusRunning:
			// Continue checking other containers
		case resource.ContainerStatusOOMKilled, resource.ContainerStatusUnknown:
			return corev1.PodFailed
		default:
			allContainersRunning = false
		}
	}

	if !hasIP {
		return corev1.PodPending
	}

	if allContainersRunning {
		return corev1.PodRunning
	}

	return corev1.PodUnknown
}

// getPodConditionsFromVirtualizationGroup determines the pod conditions based on the state of the virtualization group.
func getPodConditionsFromVirtualizationGroup(vg *client.VirtualizationGroup, podCreationTime, firstContainerStartTime, lastUpdateTime time.Time, macOSPostStartSatisfied bool, postStartFinishedAt *time.Time) []corev1.PodCondition {
	// Get the macOS VM and group containers
	macOSVM := vg.MacOSVirtualMachine
	groupContainers := vg.Containers

	// Initialize pod conditions
	conditions := []corev1.PodCondition{}

	// Determine PodScheduled condition
	podScheduledCondition := corev1.PodCondition{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: podCreationTime},
	}
	conditions = append(conditions, podScheduledCondition)

	// Determine Initialized condition
	initializedCondition := corev1.PodCondition{
		Type:               corev1.PodInitialized,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.Time{Time: firstContainerStartTime},
	}

	// Determine Ready condition
	readyCondition := corev1.PodCondition{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.Time{Time: lastUpdateTime},
	}

	// Check macOS VM state
	switch macOSVM.State() {
	case resource.VirtualMachineStatePreparing, resource.VirtualMachineStateStarting:
		// Pod is not yet initialized or ready
		initializedCondition.Status = corev1.ConditionFalse
		readyCondition.Status = corev1.ConditionFalse
	case resource.VirtualMachineStateTerminated:
		// Pod is initialized but not ready
		initializedCondition.Status = corev1.ConditionTrue
		readyCondition.Status = corev1.ConditionFalse
	case resource.VirtualMachineStateFailed:
		// Pod is not initialized and not ready
		initializedCondition.Status = corev1.ConditionFalse
		readyCondition.Status = corev1.ConditionFalse
	case resource.VirtualMachineStateTerminating, resource.VirtualMachineStateRunning:
		// Check container states
		allContainersRunning := true
		for _, container := range groupContainers {
			switch container.State.Status {
			case resource.ContainerStatusWaiting, resource.ContainerStatusCreated:
				// Pod is not yet initialized or ready
				initializedCondition.Status = corev1.ConditionFalse
				readyCondition.Status = corev1.ConditionFalse
			case resource.ContainerStatusRunning:
				// Continue checking other containers
			case resource.ContainerStatusOOMKilled, resource.ContainerStatusUnknown:
				// Pod is not initialized and not ready
				initializedCondition.Status = corev1.ConditionFalse
				readyCondition.Status = corev1.ConditionFalse
				allContainersRunning = false
			default:
				allContainersRunning = false
			}
		}

		if allContainersRunning {
			// Pod is initialized and ready
			initializedCondition.Status = corev1.ConditionTrue
			readyCondition.Status = corev1.ConditionTrue
		}

		// post-start gates Ready, not Initialized: an unsatisfied post-start gate keeps the pod NotReady.
		if !macOSPostStartSatisfied {
			readyCondition.Status = corev1.ConditionFalse
		}
	}

	// Stamp the Ready transition with the post-start-finish wall-clock, not the
	// VM-start-derived lastUpdateTime, so it reflects when the gate cleared.
	// postStartFinishedAt is nil until the SSH probe (plus optional hook) finishes,
	// so pods still gating keep their VM-start timestamps.
	if readyCondition.Status == corev1.ConditionTrue && postStartFinishedAt != nil {
		readyCondition.LastTransitionTime = metav1.NewTime(*postStartFinishedAt)
	}

	// Append conditions to the list
	conditions = append(conditions, initializedCondition, readyCondition)

	return conditions
}
