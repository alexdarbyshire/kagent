//go:build !unix

package tools

import (
	"context"
	"os/exec"
	"time"
)

// The shipped Go runtime is Unix. Keep the existing shell behavior on other
// platforms while bounding waits for inherited output pipes.
func configureCommandProcess(_ context.Context, cmd *exec.Cmd) func() {
	cmd.WaitDelay = time.Second
	return func() {}
}
