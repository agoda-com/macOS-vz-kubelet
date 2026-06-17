package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/agoda-com/macOS-vz-kubelet/internal/disk"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

const (
	// AnnotationDigest is the annotation key for the digest of the uncompressed content.
	AnnotationDigest = "com.agoda.macosvz.content.digest"

	// AnnotationUncompressedSize is the annotation key for the size of the uncompressed content.
	AnnotationUncompressedSize = "com.agoda.macosvz.content.uncompressed-size"

	// AnnotationUncompressedDigest is the annotation key for the digest of the uncompressed content.
	AnnotationUncompressedDigest = "com.agoda.macosvz.content.uncompressed-digest"
)

var (
	// ErrStoreClosed is returned when the store is already closed.
	ErrStoreClosed = errors.New("store already closed")

	// ErrDuplicateName is returned when a name is duplicated.
	ErrDuplicateName = errors.New("duplicate name")
)

// Store holds information about bundled content and provides functionalities to manage OCI images.
type Store struct {
	workingDir     string
	ignoreExisting bool
	eventRecorder  event.EventRecorder

	closed          int32                             // if the store is closed - 0: false, 1: true.
	digestToPath    *xsync.Map[digest.Digest, string] // map[digest.Digest]string
	mediaTypeToPath *xsync.Map[string, string]        // map[string]string
	nameToStatus    *xsync.Map[string, *nameStatus]   // map[string]*nameStatus
	tmpFiles        *xsync.Map[string, bool]          // map[string]bool

	memoryStore *memory.Store
}

// nameStatus contains a flag indicating if a name exists,
// and a RWMutex protecting it.
type nameStatus struct {
	sync.RWMutex
	exists bool
}

// New initializes and returns a new Store with the given working directory and the ignoreExisting flag.
func New(workingDir string, ignoreExisting bool, eventRecorder event.EventRecorder) (*Store, error) {
	workingDirAbs, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path for %s: %w", workingDir, err)
	}

	return &Store{
		workingDir:      workingDirAbs,
		ignoreExisting:  ignoreExisting,
		eventRecorder:   eventRecorder,
		digestToPath:    xsync.NewMap[digest.Digest, string](),
		mediaTypeToPath: xsync.NewMap[string, string](),
		nameToStatus:    xsync.NewMap[string, *nameStatus](),
		tmpFiles:        xsync.NewMap[string, bool](),
		memoryStore:     memory.New(),
	}, nil
}

// Close closes the Store, removing any temporary files and marking the store as closed.
func (s *Store) Close(ctx context.Context) (err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Close")
	ctx = span.WithField(ctx, "workingDir", s.workingDir)
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return ErrStoreClosed
	}
	s.setClosed()

	var errs []error
	var files []string
	s.tmpFiles.Range(func(path string, _ bool) bool {
		files = append(files, path)
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
		}
		return true
	})
	span.WithField(ctx, "files", files)

	return errors.Join(errs...)
}

// Fetch retrieves content by its Descriptor, either from the store or the fallback memory store.
func (s *Store) Fetch(ctx context.Context, target ocispec.Descriptor) (fp io.ReadCloser, err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Fetch")
	ctx = span.WithFields(ctx, log.Fields{
		"workingDir":       s.workingDir,
		"target.MediaType": target.MediaType,
		"target.Digest":    target.Digest,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return nil, ErrStoreClosed
	}

	// check if the content exists in the store
	path, exists := s.digestToPath.Load(target.Digest)
	if exists {
		fp, err = os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s: %s: %w", target.Digest, target.MediaType, errdef.ErrNotFound)
			}
			return nil, err
		}

		return fp, nil
	}

	// if the content does not exist in the store,
	// then fall back to the fallback storage.
	return s.memoryStore.Fetch(ctx, target)
}

// Push saves content to the store, ensuring it adheres to expected media types and is not duplicated.
func (s *Store) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) (err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Push")
	ctx = span.WithFields(ctx, log.Fields{
		"workingDir":         s.workingDir,
		"expected.MediaType": expected.MediaType,
		"expected.Digest":    expected.Digest,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()
	logger := log.G(ctx)

	if s.isClosedSet() {
		return ErrStoreClosed
	}

	name := expected.Annotations[ocispec.AnnotationTitle]
	if name == "" {
		return s.memoryStore.Push(ctx, expected, content)
	}

	// check the status of the name
	status := s.status(name)
	status.Lock()
	defer status.Unlock()
	if status.exists {
		return fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	if !IsMediaTypeSupported(expected.MediaType) {
		return fmt.Errorf("unsupported media type: %s", expected.MediaType)
	}

	logger.Debugf("Pulling OCI content: %s", name)
	outputFilePath := filepath.Join(s.workingDir, name)
	if err = s.processContentByType(ctx, expected, content, outputFilePath); err != nil {
		logger.WithError(err).Debugf("Failed to process content: %s", name)
		return err
	}
	logger.Debugf("Successfully pulled OCI content: %s", name)

	// update the name status as existed
	status.exists = true

	return nil
}

// Exists checks whether content exists in the store or on disk, validating it if necessary.
func (s *Store) Exists(ctx context.Context, target ocispec.Descriptor) (ok bool, err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Exists")
	ctx = span.WithFields(ctx, log.Fields{
		"workingDir":       s.workingDir,
		"target.MediaType": target.MediaType,
		"target.Digest":    target.Digest,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return false, ErrStoreClosed
	}

	// check if the content exists in the store
	_, exists := s.mediaTypeToPath.Load(target.MediaType)
	if exists {
		return true, nil
	}

	// check if the content exists on the disk and validate if it does
	name := target.Annotations[ocispec.AnnotationTitle]
	filePath := filepath.Join(s.workingDir, name)

	// if the content exists on the disk and is not ignored, validate it
	if _, err := os.Stat(filePath); err == nil && !s.ignoreExisting && name != "" {
		var isCompressed bool
		d := target.Digest
		if uncompressedDigest := target.Annotations[AnnotationUncompressedDigest]; uncompressedDigest != "" {
			d = digest.Digest(uncompressedDigest)
			isCompressed = true
		}

		ctx = span.WithFields(ctx, log.Fields{
			"name":         name,
			"filePath":     filePath,
			"isCompressed": isCompressed,
			"digest":       d,
		})

		// Validate local file with output path with digest
		err = disk.ValidateFileWithDigest(ctx, filePath, d)
		if err == nil {
			s.mediaTypeToPath.Store(target.MediaType, filePath)
			return true, nil
		}
		s.eventRecorder.FailedToValidateOCI(ctx, name)
	}

	// if the content does not exist in the store,
	// then fall back to the fallback storage.
	return s.memoryStore.Exists(ctx, target)
}

// Resolve attempts to resolve a reference to a Descriptor.
func (s *Store) Resolve(ctx context.Context, reference string) (d ocispec.Descriptor, err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Resolve")
	ctx = span.WithFields(ctx, log.Fields{
		"workingDir": s.workingDir,
		"reference":  reference,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	if reference == "" {
		return ocispec.Descriptor{}, errdef.ErrMissingReference
	}

	return s.memoryStore.Resolve(ctx, reference)
}

// Tag assigns a reference to a Descriptor if the content exists.
func (s *Store) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) (err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.Tag")
	ctx = span.WithFields(ctx, log.Fields{
		"workingDir":     s.workingDir,
		"reference":      reference,
		"desc.MediaType": desc.MediaType,
		"desc.Digest":    desc.Digest,
	})
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return ErrStoreClosed
	}

	if reference == "" {
		return errdef.ErrMissingReference
	}

	exists, err := s.Exists(ctx, desc)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s: %s: %w", desc.Digest, desc.MediaType, errdef.ErrNotFound)
	}

	return s.memoryStore.Tag(ctx, desc, reference)
}

// Predecessors returns the nodes directly pointing to the current node.
// Predecessors returns nil without error if the node does not exists in the
// store.
func (s *Store) Predecessors(ctx context.Context, node ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	return nil, nil
}

// Add saves the content to the store, ensuring it adheres to expected media types and is not duplicated.
func (s *Store) Add(ctx context.Context, mediaType string, path string) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	if !IsMediaTypeSupported(mediaType) {
		return ocispec.Descriptor{}, fmt.Errorf("unsupported media type: %s", mediaType)
	}

	// check the status of the name
	mt := MediaType(mediaType)
	name := mt.Title()
	status := s.status(name)
	status.Lock()
	defer status.Unlock()

	if status.exists {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	if path == "" {
		path = name
	}
	path = s.absPath(path)

	_, err := os.Stat(path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to stat %s: %w", path, err)
	}

	desc, err := s.descriptorFromStorageFile(ctx, mt, path)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to create descriptor for %s: %w", name, err)
	}

	s.mediaTypeToPath.Store(mediaType, path)
	// update the name status as existed
	status.exists = true

	return desc, nil
}

// Set saves the Store's configuration.
func (s *Store) Set(ctx context.Context, cfg Config) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	// check the status of the name
	name := MediaTypeConfigV1.Title()
	status := s.status(name)
	status.Lock()
	defer status.Unlock()

	if status.exists {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %w", name, ErrDuplicateName)
	}

	desc, err := s.descriptorFromConfig(ctx, cfg)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to create descriptor for %s: %w", MediaTypeConfigV1.Title(), err)
	}

	// update the name status as existed
	status.exists = true

	return desc, nil
}

// GetManifestConfigDescriptor retrieves the descriptor for the Store's Manifest configuration.
func (s *Store) GetManifestConfigDescriptor(ctx context.Context) (ocispec.Descriptor, error) {
	if s.isClosedSet() {
		return ocispec.Descriptor{}, ErrStoreClosed
	}

	configDesc := ocispec.DescriptorEmptyJSON
	configDesc.Platform = &ocispec.Platform{
		Architecture: "arm64",
		OS:           "darwin",
	}

	if ok, err := s.memoryStore.Exists(ctx, configDesc); !ok || err != nil {
		_ = s.memoryStore.Push(ctx, configDesc, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data))
	}

	return configDesc, nil
}

// GetFilePathForMediaType returns the file path for the given media type.
func (s *Store) GetFilePathForMediaType(ctx context.Context, mediaType MediaType) (path string, err error) {
	_, span := trace.StartSpan(ctx, "OCI.GetFilePathForMediaType")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return path, ErrStoreClosed
	}

	path, ok := s.mediaTypeToPath.Load(string(mediaType))
	if !ok {
		return "", fmt.Errorf("media type %s not found", mediaType)
	}

	return path, nil
}

// GetConfig retrieves the Store's configuration.
func (s *Store) GetConfig(ctx context.Context) (cfg Config, err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.GetConfig")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if s.isClosedSet() {
		return cfg, ErrStoreClosed
	}

	path, err := s.GetFilePathForMediaType(ctx, MediaTypeConfigV1)
	if err != nil {
		return Config{}, fmt.Errorf("failed to get file path for media type %s: %w", MediaTypeConfigV1, err)
	}

	fp, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, fp.Close())
	}()

	config := &Config{}
	if err := json.NewDecoder(fp).Decode(config); err != nil {
		return Config{}, fmt.Errorf("failed to decode bundle: %w", err)
	}

	return *config, nil
}

// absPath returns the absolute path of the path.
func (s *Store) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.workingDir, path)
}

// tempFile creates a temp file with the file name format "macosvz_file_randomString",
// and returns the pointer to the temp file.
func (s *Store) tempFile() (*os.File, error) {
	tmp, err := os.CreateTemp(os.TempDir(), "macosvz_file_*")
	if err != nil {
		return nil, err
	}

	s.tmpFiles.Store(tmp.Name(), true)
	return tmp, nil
}

// status returns the nameStatus for the given name.
func (s *Store) status(name string) *nameStatus {
	status, _ := s.nameToStatus.LoadOrStore(name, &nameStatus{})
	return status
}

// isClosedSet returns true if the `closed` flag is set, otherwise returns false.
func (s *Store) isClosedSet() bool {
	return atomic.LoadInt32(&s.closed) == 1
}

// setClosed sets the `closed` flag.
func (s *Store) setClosed() {
	atomic.StoreInt32(&s.closed, 1)
}
