package envcfg_test

import (
	"testing"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"

	"github.com/stretchr/testify/assert"
)

// defaultPostStart is the post-start timeout used when the env var is unset,
// empty, or invalid. Mirrors the documented default for PostStartTimeoutChecked.
const defaultPostStart = 10 * time.Second

// Lock the env var name: it is part of the public contract (README env table,
// deployment plists) and the single source for both timeout resolvers.
func TestPostStartTimeoutEnv_Name(t *testing.T) {
	assert.Equal(t, "VZ_POSTSTART_TIMEOUT", envcfg.PostStartTimeoutEnv,
		"the post-start timeout env var name is a public contract")
}

// PostStartTimeoutChecked: unset/valid -> err nil; invalid/non-positive -> err
// non-nil AND the returned duration is still the 10s default (so the startup
// warn path stays truthful: the message reports the default that is applied).
func TestPostStartTimeoutChecked(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "Unset -> default, no error", set: false, want: defaultPostStart, wantErr: false},
		{name: "Valid 30s -> value, no error", set: true, value: "30s", want: 30 * time.Second, wantErr: false},
		{name: "Empty -> default, no error", set: true, value: "", want: defaultPostStart, wantErr: false},
		{name: "Unparseable -> default, error", set: true, value: "abc", want: defaultPostStart, wantErr: true},
		{name: "Zero -> default, error", set: true, value: "0s", want: defaultPostStart, wantErr: true},
		{name: "Negative -> default, error", set: true, value: "-5s", want: defaultPostStart, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envcfg.PostStartTimeoutEnv, tt.value)
			} else {
				t.Setenv(envcfg.PostStartTimeoutEnv, "")
			}
			got, err := envcfg.PostStartTimeoutChecked()
			assert.Equal(t, tt.want, got, "resolved post-start timeout (default applied even on error)")
			if tt.wantErr {
				assert.Error(t, err, "invalid/non-positive value must return an error")
			} else {
				assert.NoError(t, err, "unset/valid value must not return an error")
			}
		})
	}
}
