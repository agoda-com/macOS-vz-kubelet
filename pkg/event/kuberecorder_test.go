package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

func TestKubeEventRecorder(t *testing.T) {
	tests := []struct {
		name   string
		action func(ctx context.Context, recorder *event.KubeEventRecorder)
	}{
		{
			name: "PullingImage",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.PullingImage(ctx, "nginx:latest", "nginx-container")
			},
		},
		{
			name: "PulledImage",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.PulledImage(ctx, "nginx:latest", "nginx-container", "5s")
			},
		},
		{
			name: "FailedToValidateOCI",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedToValidateOCI(ctx, "invalid content")
			},
		},
		{
			name: "FailedToPullImage",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedToPullImage(ctx, "nginx:latest", "nginx-container", errors.New("network error"))
			},
		},
		{
			name: "BackOffPullImage",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.BackOffPullImage(ctx, "nginx:latest", "nginx-container", errors.New("network error"))
			},
		},
		{
			name: "CreatedContainer",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.CreatedContainer(ctx, "nginx-container")
			},
		},
		{
			name: "StartedContainer",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.StartedContainer(ctx, "nginx-container")
			},
		},
		{
			name: "FailedToCreateContainer",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedToCreateContainer(ctx, "nginx-container", errors.New("insufficient resources"))
			},
		},
		{
			name: "FailedToStartContainer",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedToStartContainer(ctx, "nginx-container", errors.New("container failed"))
			},
		},
		{
			name: "FailedPostStartHook",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedPostStartHook(ctx, "nginx-container", []string{"echo", "hello"}, errors.New("hook failed"))
			},
		},
		{
			name: "FailedPreStopHook",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedPreStopHook(ctx, "nginx-container", []string{"echo", "hello"}, errors.New("hook failed"))
			},
		},
		{
			name: "FailedToResolveImagePullSecrets",
			action: func(ctx context.Context, recorder *event.KubeEventRecorder) {
				recorder.FailedToResolveImagePullSecrets(ctx, errors.New("secret missing"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup fake recorder and event recorder
			fakeRecorder := record.NewFakeRecorder(1)
			eventRecorder := event.NewKubeEventRecorder(fakeRecorder)

			// Create a sample object reference (e.g., a Pod)
			objectRef := corev1.ObjectReference{
				Kind:      "Pod",
				Name:      "test-pod",
				Namespace: "default",
				UID:       "12345",
			}

			// Create a context with the object reference
			ctx := event.WithObjectRef(context.Background(), objectRef)

			// Execute the action for the test case
			tt.action(ctx, eventRecorder)

			// Verify the event was recorded
			select {
			case <-fakeRecorder.Events:
			default:
				t.Errorf("expected an event to be recorded, but none was found")
			}
		})
	}
}

func TestKubeEventRecorder_NoObjectRef(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(0)
	eventRecorder := event.NewKubeEventRecorder(fakeRecorder)

	ctx := context.Background()
	eventRecorder.PullingImage(ctx, "nginx:latest", "nginx-container")

	select {
	case <-fakeRecorder.Events:
		t.Errorf("expected no event to be recorded, but one was found")
	default:
	}
}
