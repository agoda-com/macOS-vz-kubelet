package downloader

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Manager manages the download of OCI images.
type Manager struct {
	eventRecorder event.EventRecorder
	cachePath     string

	downloads sync.Map // map[string]*state (ref -> state)
}

// state contains the state of a download operation.
type state struct {
	subscribers atomic.Int32
	once        sync.Once
	done        chan struct{}
	span        oteltrace.Span
	cancelFunc  context.CancelFunc

	config   config.MacPlatformConfigurationOptions
	duration time.Duration
	err      error
}

// NewManager creates a new DownloadManager.
func NewManager(eventRecorder event.EventRecorder, cachePath string) *Manager {
	return &Manager{
		eventRecorder: eventRecorder,
		cachePath:     cachePath,
	}
}

// Download ensures that a download operation identified by 'ref' is only initiated once,
// regardless of how many subscribers request it. It uses sync.Once to ensure the job runs
// only once, and manages multiple subscribers using a sync.WaitGroup-like approach.
// Each subscriber either receives the result of the download when it's complete, or an error
// if the download fails or if their context is canceled. The download job is canceled
// when the last subscriber unsubscribes (cancels their context).
//
// Parameters:
//   - ctx: The context controlling the lifecycle of the subscriber's interest in the download.
//   - ref: A unique identifier for the resource being downloaded.
//   - ignoreExisting: A flag indicating whether to force a re-download, even if the resource
//     is already cached.
//
// Returns:
// - config.MacPlatformConfigurationOptions: The result of the download if successful.
// - error: Any error that occurred during the download, or if the subscriber's context is canceled.
func (m *Manager) Download(ctx context.Context, ref string, ignoreExisting bool, creds resource.RegistryCredentials) (cfg config.MacPlatformConfigurationOptions, d time.Duration, err error) {
	ctx, span := trace.StartSpan(ctx, "Manager.Download")
	ctx = span.WithFields(ctx, log.Fields{
		"ref":            ref,
		"ignoreExisting": ignoreExisting,
	})
	defer func() {
		_ = span.WithField(ctx, "duration", d)
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)
	logger.Infof("Requesting to subscribe to download %q", ref)

	value, _ := m.downloads.LoadOrStore(ref, &state{done: make(chan struct{}, 1)})
	state, ok := value.(*state)
	if !ok {
		return cfg, d, fmt.Errorf("invalid state")
	}

	state.subscribers.Add(1) // Increment the number of subscribers
	defer func() {
		if state.subscribers.Add(-1) == 0 {
			m.downloads.Delete(ref) // Delete reference first so that the state is not accessed after it's closed
			state.cancelFunc()      // Cancel download when last subscriber is removed
			logger.Infof("No more subscribers left for %q, cleaning up...", ref)
		}
	}()

	state.once.Do(func() {
		logger.Infof("Initiating download %q per request", ref)
		downloadCreds := creds
		// Use a background context to manage the underlying download
		var downloadCtx context.Context
		downloadCtx, state.cancelFunc = context.WithCancel(context.Background())

		// Create a new span for the download operation and link it to the parent span
		// New detached span is closed in startDownload function
		name := "Manager.startDownload"
		link := oteltrace.LinkFromContext(ctx)
		// nolint: spancheck
		downloadCtx, state.span = otel.Tracer(name).Start(downloadCtx, name, oteltrace.WithLinks(link))
		downloadCtx = log.WithLogger(downloadCtx, log.G(ctx).WithField("method", name))

		// Propagate the object reference to the download context
		// TODO: OCI doesnt have to report to kubernetes directly, remove this eventually
		objRef, ok := event.GetObjectRef(ctx)
		if ok {
			downloadCtx = event.WithObjectRef(downloadCtx, *objRef)
		}

		// Performing download in a go routine to keep listening for context cancellation.
		// Start Download manages its own background context and cancels it when the download is done.
		// nolint: contextcheck
		go m.startDownload(downloadCtx, state, ref, ignoreExisting, downloadCreds)
	})

	// Link the download span to the subscriber's span
	oteltrace.SpanFromContext(ctx).AddLink(oteltrace.Link{SpanContext: state.span.SpanContext()})

	select {
	case <-ctx.Done():
		return cfg, d, context.Canceled
	case <-state.done:
		return state.config, state.duration, state.err
	}
}

// startDownload starts the download operation and manages the state of the download.
func (m *Manager) startDownload(ctx context.Context, state *state, ref string, ignoreExisting bool, creds resource.RegistryCredentials) {
	defer func() {
		close(state.done)
		state.cancelFunc()

		// Set Span status and end it
		if state.err == nil {
			state.span.SetStatus(codes.Ok, "")
		} else {
			state.span.SetStatus(codes.Error, state.err.Error())
		}
		state.span.End()
	}()
	state.span.SetAttributes(attribute.String("ref", ref), attribute.Bool("ignoreExisting", ignoreExisting))
	logger := log.G(ctx)

	logger.Infof("Starting download for %q", ref)
	startTime := time.Now()
	state.config, state.err = Download(ctx, Params{
		Ref:             ref,
		StorePath:       m.cachePath,
		IgnoreExisiting: ignoreExisting,
		Credentials:     creds,
	}, m.eventRecorder)

	state.duration = time.Since(startTime)
	logger.Debugf("Download for %q completed in %v", ref, state.duration)

	if ctx.Err() != nil {
		// prioritize the context error
		state.err = ctx.Err()
	}
}
