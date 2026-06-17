// Package envcfg parses configuration from environment variables.
// Pure Go (no cgo, no macOS syscalls); the Darwin provider reads os.Getenv and
// passes the raw values here so the parsing/validation stays unit-testable.
package envcfg

import (
	"fmt"
	"strings"
	"time"
)

// ResolveDuration parses a Go duration string, falling back to def when the
// value is unset/blank, unparseable, or non-positive. An error is returned for
// the unparseable/non-positive cases (def is still returned alongside it) so the
// caller can log a warning; an empty value falls back silently with no error.
func ResolveDuration(envValue string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(envValue)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("invalid duration %q: %w", v, err)
	}
	if d <= 0 {
		return def, fmt.Errorf("duration %q must be positive", v)
	}
	return d, nil
}
