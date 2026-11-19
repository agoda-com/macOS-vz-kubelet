package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDurationEnv(t *testing.T) {
	const key = "VZ_TEST_DURATION"
	const defaultValue = time.Minute

	t.Run("unset uses the default", func(t *testing.T) {
		// t.Setenv registers a restore of the original value; unsetting it here is what the test is about.
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))

		d, err := parseDurationEnv(key, defaultValue)
		require.NoError(t, err)
		assert.Equal(t, defaultValue, d)
	})

	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr string
	}{
		{name: "minutes", value: "5m", want: 5 * time.Minute},
		{name: "seconds", value: "90s", want: 90 * time.Second},
		{name: "zero parses (rejected later)", value: "0s", want: 0},
		{name: "empty is invalid", value: "", wantErr: `invalid duration "" for VZ_TEST_DURATION`},
		{name: "no unit is invalid", value: "300", wantErr: `invalid duration "300" for VZ_TEST_DURATION`},
		{name: "words are invalid", value: "5min", wantErr: `invalid duration "5min" for VZ_TEST_DURATION`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(key, tt.value)

			d, err := parseDurationEnv(key, defaultValue)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

func TestValidateNodeStatusUpdateInterval(t *testing.T) {
	tests := []struct {
		name    string
		value   time.Duration
		wantErr bool
	}{
		{name: "default", value: time.Minute},
		{name: "kubelet's five minutes", value: 5 * time.Minute},
		{name: "one nanosecond", value: time.Nanosecond},
		{name: "zero", value: 0, wantErr: true},
		{name: "negative", value: -time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNodeStatusUpdateInterval(tt.value)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "must be positive")
				return
			}
			assert.NoError(t, err)
		})
	}
}
