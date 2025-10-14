package event

import "context"

type EventRecorder interface {
	PullingImage(ctx context.Context, image, containerName string)
	PulledImage(ctx context.Context, image, containerName string, duration string)
	FailedToValidateOCI(ctx context.Context, content string)
	FailedToPullImage(ctx context.Context, image, containerName string, err error)
	BackOffPullImage(ctx context.Context, image, containerName string, err error)

	CreatedContainer(ctx context.Context, containerName string)
	StartedContainer(ctx context.Context, containerName string)
	FailedToCreateContainer(ctx context.Context, containerName string, err error)
	FailedToStartContainer(ctx context.Context, containerName string, err error)
	FailedPostStartHook(ctx context.Context, containerName string, cmd []string, err error)
	FailedPreStopHook(ctx context.Context, containerName string, cmd []string, err error)
	FailedToResolveImagePullSecrets(ctx context.Context, err error)
}
