package event

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/events"
)

const (
	UIDField = "uid"

	// Upstream kubelet records "FailedToRetrieveImagePullSecret" when pull secrets cannot be resolved:
	// https://github.com/kubernetes/website/blob/main/content/en/docs/tasks/configure-pod-container/pull-image-private-registry.md
	eventReasonFailedImagePullSecrets = "FailedToRetrieveImagePullSecret"
)

type objectRefKeyType struct{}

var objectRefKey = objectRefKeyType{}

func WithObjectRef(ctx context.Context, objectRef corev1.ObjectReference) context.Context {
	return context.WithValue(ctx, objectRefKey, objectRef)
}

func GetObjectRef(ctx context.Context) (*corev1.ObjectReference, bool) {
	val := ctx.Value(objectRefKey)
	if val == nil {
		return nil, false
	}
	objectRef, ok := val.(corev1.ObjectReference)
	if !ok {
		return nil, false
	}
	return &objectRef, true
}

type KubeEventRecorder struct {
	eventRecorder record.EventRecorder
}

func NewKubeEventRecorder(eventRecorder record.EventRecorder) *KubeEventRecorder {
	return &KubeEventRecorder{
		eventRecorder: eventRecorder,
	}
}

func (r *KubeEventRecorder) PullingImage(ctx context.Context, image, containerName string) {
	r.recordEvent(ctx, containerName, corev1.EventTypeNormal, events.PullingImage, "Pulling image \"%s\"", image)
}

func (r *KubeEventRecorder) PulledImage(ctx context.Context, image, containerName, duration string) {
	r.recordEvent(ctx, containerName, corev1.EventTypeNormal, events.PulledImage, "Successfully pulled image \"%s\" in %s", image, duration)
}

func (r *KubeEventRecorder) FailedToValidateOCI(ctx context.Context, content string) {
	r.recordEvent(ctx, "", corev1.EventTypeWarning, events.FailedToInspectImage, "Failed to validate OCI content: %s", content)
}

func (r *KubeEventRecorder) FailedToPullImage(ctx context.Context, image, containerName string, err error) {
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.FailedToPullImage, "Failed to pull image \"%s\": %v", image, err)
}

func (r *KubeEventRecorder) BackOffPullImage(ctx context.Context, image, containerName string, err error) {
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.BackOffPullImage, "Back-off pulling image \"%s\": %v", image, err)
}

func (r *KubeEventRecorder) CreatedContainer(ctx context.Context, containerName string) {
	r.recordEvent(ctx, containerName, corev1.EventTypeNormal, events.CreatedContainer, "Created container %s", containerName)
}

func (r *KubeEventRecorder) StartedContainer(ctx context.Context, containerName string) {
	r.recordEvent(ctx, containerName, corev1.EventTypeNormal, events.StartedContainer, "Started container %s", containerName)
}

func (r *KubeEventRecorder) FailedToCreateContainer(ctx context.Context, containerName string, err error) {
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.FailedToCreateContainer, "Failed to create container %s: %v", containerName, err)
}

func (r *KubeEventRecorder) FailedToStartContainer(ctx context.Context, containerName string, err error) {
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.FailedToStartContainer, "Failed to start container %s: %v", containerName, err)
}

func (r *KubeEventRecorder) FailedPostStartHook(ctx context.Context, containerName string, cmd []string, err error) {
	cmdStr := fmt.Sprintf("[%s]", strings.Join(cmd, ", "))
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.FailedPostStartHook, "Exec lifecycle hook (%s) for Container \"%s\" failed - error: %v", cmdStr, containerName, err)
}

func (r *KubeEventRecorder) FailedPreStopHook(ctx context.Context, containerName string, cmd []string, err error) {
	cmdStr := fmt.Sprintf("[%s]", strings.Join(cmd, ", "))
	r.recordEvent(ctx, containerName, corev1.EventTypeWarning, events.FailedPreStopHook, "Exec lifecycle hook (%s) for Container \"%s\" failed - error: %v", cmdStr, containerName, err)
}

func (r *KubeEventRecorder) FailedToResolveImagePullSecrets(ctx context.Context, err error) {
	r.recordEvent(ctx, "", corev1.EventTypeWarning, eventReasonFailedImagePullSecrets, "Failed to resolve image pull secrets: %v", err)
}

func (r *KubeEventRecorder) recordEvent(ctx context.Context, containerName, eventType, reason, messageFmt string, args ...interface{}) {
	objectRef, ok := GetObjectRef(ctx)
	if !ok {
		return
	}
	if containerName != "" {
		objectRef.FieldPath = fmt.Sprintf("spec.containers{%s}", containerName)
	}
	r.eventRecorder.Eventf(objectRef, eventType, reason, messageFmt, args...)
}
