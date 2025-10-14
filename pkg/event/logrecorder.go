package event

import (
	"context"
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/log"
)

type LogEventRecorder struct{}

func (r LogEventRecorder) PullingImage(ctx context.Context, image, _ string) {
	log.G(ctx).Infof("Pulling image \"%s\"", image)
}

func (r LogEventRecorder) PulledImage(ctx context.Context, image, _, duration string) {
	log.G(ctx).Infof("Successfully pulled image \"%s\" in %s", image, duration)
}

func (r LogEventRecorder) FailedToValidateOCI(ctx context.Context, content string) {
	log.G(ctx).Warnf("Failed to validate OCI content: %s", content)
}

func (r LogEventRecorder) FailedToPullImage(ctx context.Context, image, _ string, err error) {
	log.G(ctx).WithError(err).Warnf("Failed to pull image \"%s\"", image)
}

func (r LogEventRecorder) BackOffPullImage(ctx context.Context, image, _ string, err error) {
	log.G(ctx).WithError(err).Errorf("Back-off pulling image \"%s\"", image)
}

func (r LogEventRecorder) CreatedContainer(ctx context.Context, containerName string) {
	log.G(ctx).Infof("Created container %s", containerName)
}

func (r LogEventRecorder) StartedContainer(ctx context.Context, containerName string) {
	log.G(ctx).Infof("Started container %s", containerName)
}

func (r LogEventRecorder) FailedToCreateContainer(ctx context.Context, containerName string, err error) {
	log.G(ctx).WithError(err).Errorf("Failed to create container %s", containerName)
}

func (r LogEventRecorder) FailedToStartContainer(ctx context.Context, containerName string, err error) {
	log.G(ctx).WithError(err).Errorf("Failed to start container %s", containerName)
}

func (r LogEventRecorder) FailedPostStartHook(ctx context.Context, containerName string, cmd []string, err error) {
	cmdStr := fmt.Sprintf("[%s]", strings.Join(cmd, ", "))
	log.G(ctx).WithError(err).Errorf("Exec lifecycle hook (%s) for Container \"%s\" failed - error: %v", cmdStr, containerName, err)
}

func (r LogEventRecorder) FailedPreStopHook(ctx context.Context, containerName string, cmd []string, err error) {
	cmdStr := fmt.Sprintf("[%s]", strings.Join(cmd, ", "))
	log.G(ctx).WithError(err).Errorf("Exec lifecycle hook (%s) for Container \"%s\" failed - error: %v", cmdStr, containerName, err)
}

func (r LogEventRecorder) FailedToResolveImagePullSecrets(ctx context.Context, err error) {
	log.G(ctx).WithError(err).Warn("Failed to resolve image pull secrets")
}
