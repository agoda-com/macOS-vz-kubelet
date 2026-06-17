package envcfg_test

import (
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
)

const defaultTimeout = 10 * time.Second

// ResolveDuration: empty/whitespace/invalid/non-positive fall back to def (err
// surfaced for the invalid cases so the caller can warn); a valid positive
// duration is returned as-is with no error.
func TestResolveDuration(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		def      time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{name: "Empty falls back to default", envValue: "", def: defaultTimeout, want: defaultTimeout, wantErr: false},
		{name: "Whitespace falls back to default", envValue: "   ", def: defaultTimeout, want: defaultTimeout, wantErr: false},
		{name: "Tab and newline whitespace falls back", envValue: "\t\n", def: defaultTimeout, want: defaultTimeout, wantErr: false},
		{name: "Valid seconds", envValue: "5s", def: defaultTimeout, want: 5 * time.Second, wantErr: false},
		{name: "Valid milliseconds", envValue: "500ms", def: defaultTimeout, want: 500 * time.Millisecond, wantErr: false},
		{name: "Valid surrounded by whitespace", envValue: "  3s  ", def: defaultTimeout, want: 3 * time.Second, wantErr: false},
		{name: "Zero is invalid, falls back with error", envValue: "0", def: defaultTimeout, want: defaultTimeout, wantErr: true},
		{name: "Zero seconds is invalid, falls back with error", envValue: "0s", def: defaultTimeout, want: defaultTimeout, wantErr: true},
		{name: "Negative is invalid, falls back with error", envValue: "-3s", def: defaultTimeout, want: defaultTimeout, wantErr: true},
		{name: "Garbage is invalid, falls back with error", envValue: "garbage", def: defaultTimeout, want: defaultTimeout, wantErr: true},
		{name: "Bare number without unit is invalid, falls back with error", envValue: "5", def: defaultTimeout, want: defaultTimeout, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := envcfg.ResolveDuration(tt.envValue, tt.def)
			assert.Equal(t, tt.want, got, "resolved duration")
			if tt.wantErr {
				assert.Error(t, err, "invalid input must surface an error so the caller can warn")
			} else {
				assert.NoError(t, err, "valid/empty input must not surface an error")
			}
		})
	}
}

// The default value is returned verbatim regardless of which default is passed,
// so the caller's chosen default is honored (not a hardcoded internal one).
func TestResolveDuration_DefaultPassthrough(t *testing.T) {
	got, err := envcfg.ResolveDuration("", 7*time.Second)
	assert.NoError(t, err)
	assert.Equal(t, 7*time.Second, got)

	got, err = envcfg.ResolveDuration("nope", 42*time.Second)
	assert.Error(t, err)
	assert.Equal(t, 42*time.Second, got)
}
