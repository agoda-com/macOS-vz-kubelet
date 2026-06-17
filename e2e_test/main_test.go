package e2e_test

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// imageCacheDirPrefix is shared by MkdirTemp and the startup sweep glob so the create
// and sweep patterns cannot drift.
const imageCacheDirPrefix = "macos-vz-e2e-image-cache-"

// sharedImageCacheDir is the macOS image-store cache reused by every provider in this
// test binary, so the ~137GB image is pulled once, not per pod spawn.
var sharedImageCacheDir string

// TestMain owns the shared image cache dir (~137GB macOS image) and must not leak it.
// Three cleanup layers, so the dir is reclaimed even on early stop or crash:
//
//  1. Startup sweep: remove cache dirs left by a prior death (SIGKILL / crash /
//     `go test -timeout` kill) - the safety net for what layers 2 and 3 cannot catch.
//     Safe because e2e runs are serialized (CI resource_group; single-host manual runs),
//     so no concurrent run owns these dirs.
//  2. Signal handler: remove the dir on a graceful stop (SIGINT/SIGTERM). SIGQUIT is left
//     uncaught so `go test -timeout` still dumps goroutines; SIGKILL cannot be caught.
//     Both are reclaimed by the next run's sweep.
//  3. Normal path: after m.Run, signal.Stop before RemoveAll (else they race), and
//     RemoveAll explicitly because os.Exit skips defers.
func TestMain(m *testing.M) {
	flag.Parse()

	// Layer 1: sweep dirs leaked by a prior death before creating a fresh one.
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), imageCacheDirPrefix+"*")); err == nil {
		for _, d := range matches {
			_ = os.RemoveAll(d)
		}
	}

	dir, err := os.MkdirTemp("", imageCacheDirPrefix+"*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create shared image cache dir: %v\n", err)
		os.Exit(1)
	}
	sharedImageCacheDir = dir

	// Layer 2: clean up on a graceful stop (SIGINT/SIGTERM).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = os.RemoveAll(sharedImageCacheDir)
		os.Exit(1)
	}()

	code := m.Run()

	// Layer 3: stop the handler before RemoveAll (else they race); RemoveAll explicitly
	// because os.Exit skips defers.
	signal.Stop(sigCh)
	_ = os.RemoveAll(sharedImageCacheDir)
	os.Exit(code)
}
