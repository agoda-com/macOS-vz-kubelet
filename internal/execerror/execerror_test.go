package execerror_test

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"
	utilexec "k8s.io/utils/exec"

	"github.com/agoda-com/macOS-vz-kubelet/internal/execerror"
)

// exitStatusError mimics ssh.ExitError: carries a remote exit code via ExitStatus().
type exitStatusError struct {
	code int
}

func (e exitStatusError) Error() string   { return fmt.Sprintf("process exited with status %d", e.code) }
func (e exitStatusError) ExitStatus() int { return e.code }

func TestAsCodeExitError(t *testing.T) {
	plain := errors.New("dial failed")
	missing := &ssh.ExitMissingError{}

	tests := []struct {
		name        string
		in          error
		wantNil     bool
		wantExit    bool // expect a utilexec.ExitError
		wantCode    int
		wantSameErr error // identity check when not converted
	}{
		{name: "nil", in: nil, wantNil: true},
		{name: "plain non-exit error", in: plain, wantSameErr: plain},
		{name: "ssh exit-missing has no status", in: missing, wantSameErr: missing},
		{name: "exit 1", in: exitStatusError{1}, wantExit: true, wantCode: 1},
		{name: "exit 2", in: exitStatusError{2}, wantExit: true, wantCode: 2},
		{name: "exit 7", in: exitStatusError{7}, wantExit: true, wantCode: 7},
		{name: "exit 42", in: exitStatusError{42}, wantExit: true, wantCode: 42},
		{name: "exit 127", in: exitStatusError{127}, wantExit: true, wantCode: 127},
		{name: "exit 255", in: exitStatusError{255}, wantExit: true, wantCode: 255},
		{name: "negative code", in: exitStatusError{-1}, wantExit: true, wantCode: -1},
		{name: "wrapped exit", in: fmt.Errorf("ssh wait: %w", exitStatusError{1}), wantExit: true, wantCode: 1},
		{name: "exit 0", in: exitStatusError{0}, wantExit: true, wantCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := execerror.AsCodeExitError(tt.in)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %v", got)
				}
				return
			}

			// Direct type-assert, as vk ServeExec does.
			ee, ok := got.(utilexec.ExitError)
			if tt.wantExit {
				if !ok {
					t.Fatalf("got %T, want a utilexec.ExitError", got)
				}
				if !ee.Exited() {
					t.Fatalf("Exited() = false, want true")
				}
				if ee.ExitStatus() != tt.wantCode {
					t.Fatalf("ExitStatus() = %d, want %d", ee.ExitStatus(), tt.wantCode)
				}
				return
			}

			// Non-exit error: returned unchanged, NOT a utilexec.ExitError.
			if ok {
				t.Fatalf("non-exit error became a utilexec.ExitError: %v", got)
			}
			if tt.wantSameErr != nil && !errors.Is(got, tt.wantSameErr) {
				t.Fatalf("returned error identity changed: got %v, want %v", got, tt.wantSameErr)
			}
		})
	}
}
