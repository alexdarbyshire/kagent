//go:build unix

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// Descendants belong to this execution even when Bash exits before they do.
// Kill the group on cancellation and when the execution owner returns.
func configureCommandProcess(ctx context.Context, cmd *exec.Cmd) func() {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return func() {
		if cmd.Process != nil {
			if err := cmd.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				logging.FromContext(ctx).ErrorContext(ctx, "failed to clean up command process group", "pid", cmd.Process.Pid, "error", err)
			}
		}
	}
}
