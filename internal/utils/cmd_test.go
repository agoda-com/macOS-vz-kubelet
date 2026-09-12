package utils_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/utils"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildExportEnvCommand(t *testing.T) {
	tests := []struct {
		name     string
		env      corev1.EnvVar
		expected string
	}{
		{
			name: "Single line value",
			env: corev1.EnvVar{
				Name:  "SINGLE_LINE",
				Value: "simple_value",
			},
			expected: "export SINGLE_LINE=\"simple_value\"\n",
		},
		{
			name: "Single line value with special characters",
			env: corev1.EnvVar{
				Name:  "SPECIAL_CHARS",
				Value: "value_with_special_chars!@#$%^&*()",
			},
			expected: "export SPECIAL_CHARS=\"value_with_special_chars!@#$%^&*()\"\n",
		},
		{
			name: "Multi-line value",
			env: corev1.EnvVar{
				Name:  "MULTI_LINE",
				Value: "line1\nline2\nline3",
			},
			expected: "export MULTI_LINE=$(cat <<'ESCAPE_EOF'\nline1\nline2\nline3\nESCAPE_EOF\n)\n",
		},
		{
			name: "Empty value",
			env: corev1.EnvVar{
				Name:  "EMPTY_VALUE",
				Value: "",
			},
			expected: "export EMPTY_VALUE=\"\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := utils.BuildExportEnvCommand(tt.env)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildExecCommandStringPreservesArgv(t *testing.T) {
	args := []string{"executable name", "one argument", "it's quoted", "", "$HOME", "*"}
	cmd := append([]string{"sh", "-c", `printf '%s\0' "$0" "$@"`}, args...)

	command, err := utils.BuildExecCommandString(cmd, nil)
	assert.NoError(t, err)

	output, err := exec.CommandContext(t.Context(), "sh", "-c", command).Output()
	assert.NoError(t, err)
	assert.Equal(t, strings.Join(args, "\x00")+"\x00", string(output))
}

func TestBuildExecCommandString(t *testing.T) {
	tests := []struct {
		name        string
		cmd         []string
		env         []corev1.EnvVar
		expected    string
		expectError bool
	}{
		{
			name: "Command with environment variables",
			cmd:  []string{"printf", "%s\\n", "hello world"},
			env: []corev1.EnvVar{
				{Name: "FOO", Value: "bar"},
				{Name: "BAZ", Value: "qux"},
			},
			expected:    "export FOO=\"bar\"\nexport BAZ=\"qux\"\nexec 'printf' '%s\\n' 'hello world'",
			expectError: false,
		},
		{
			name:        "Empty command",
			cmd:         nil,
			env:         []corev1.EnvVar{},
			expected:    "",
			expectError: true,
		},
		{
			name:        "Empty executable",
			cmd:         []string{"", "argument"},
			env:         []corev1.EnvVar{},
			expected:    "",
			expectError: true,
		},
		{
			name:        "Arguments preserve spaces quotes and empty values",
			cmd:         []string{"printf", "%s\\n", "one argument", "it's quoted", ""},
			env:         []corev1.EnvVar{},
			expected:    "exec 'printf' '%s\\n' 'one argument' 'it'\"'\"'s quoted' ''",
			expectError: false,
		},
		{
			name:        "Shell metacharacters remain argument data",
			cmd:         []string{"printf", "%s", "$(touch /tmp/unwanted); *"},
			env:         []corev1.EnvVar{},
			expected:    "exec 'printf' '%s' '$(touch /tmp/unwanted); *'",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := utils.BuildExecCommandString(tt.cmd, tt.env)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}
