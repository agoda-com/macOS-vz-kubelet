package podstatus_test

import (
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/podstatus"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// HasExecPostStartHook is true only for an exec-shaped hook; HTTPGet/TCPSocket/Sleep are
// not run by the provider and must report false (so they never gate readiness).
func TestHasExecPostStartHook(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		want      bool
	}{
		{
			name: "Exec postStart hook gates",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "true"}},
					},
				},
			},
			want: true,
		},
		{
			name: "HTTPGet postStart hook does not gate",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"},
					},
				},
			},
			want: false,
		},
		{
			name: "TCPSocket postStart hook does not gate",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						TCPSocket: &corev1.TCPSocketAction{Host: "127.0.0.1"},
					},
				},
			},
			want: false,
		},
		{
			name: "Sleep postStart hook does not gate",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						Sleep: &corev1.SleepAction{Seconds: 5},
					},
				},
			},
			want: false,
		},
		{
			name: "Nil PostStart does not gate",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{PostStart: nil},
			},
			want: false,
		},
		{
			name:      "Nil Lifecycle does not gate",
			container: corev1.Container{Lifecycle: nil},
			want:      false,
		},
		{
			name: "Non-nil PostStart with nil Exec does not gate",
			container: corev1.Container{
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{Exec: nil},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, podstatus.HasExecPostStartHook(tt.container))
		})
	}
}
