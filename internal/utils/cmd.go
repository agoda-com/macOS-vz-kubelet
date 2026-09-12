package utils

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// BuildExportEnvCommand returns a shell command that exports the given environment variable.
// The command is formatted as "export NAME=VALUE" where VALUE is escaped using heredoc syntax if it contains newlines.
func BuildExportEnvCommand(env corev1.EnvVar) string {
	var value string
	switch {
	case strings.Count(env.Value, "\n") == 0:
		value = strconv.Quote(env.Value)
	default:
		// support multineline env variables
		value = "$(cat <<'ESCAPE_EOF'\n" + env.Value + "\nESCAPE_EOF\n)"
	}
	return fmt.Sprintf("export %s=%s\n", env.Name, value)
}

// BuildExecCommandString serializes a Kubernetes command argv for an SSH exec
// request. SSH carries a command string rather than an argv, so every argument
// is POSIX shell-quoted before the remote login shell parses it.
func BuildExecCommandString(cmd []string, env []corev1.EnvVar) (string, error) {
	if len(cmd) == 0 || cmd[0] == "" {
		return "", fmt.Errorf("command argv is empty")
	}

	cmdStr := ""
	for _, e := range env {
		cmdStr += BuildExportEnvCommand(e)
	}

	cmdStr += "exec"
	for _, arg := range cmd {
		cmdStr += " " + shellQuote(arg)
	}

	return cmdStr, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
