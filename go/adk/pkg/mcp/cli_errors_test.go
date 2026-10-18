package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIExitCodePreservesJoinedOperationalFailures(t *testing.T) {
	cause := errors.New("invalid command")
	usage := cliUsageErrorf("arguments: %w", cause)
	closeErr := errors.New("returned close failure")
	cleanupErr := errors.New("failed to remove incomplete output")
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"usage", usage, 2},
		{"wrapped usage", fmt.Errorf("command: %w", usage), 2},
		{"joined usage", errors.Join(usage, cliUsageErrorf("invalid file syntax")), 2},
		{"close failure", closeErr, 1},
		{"usage and close", errors.Join(usage, closeErr), 1},
		{"usage and cleanup", errors.Join(usage, cleanupErr), 1},
		{"write and usage", errors.Join(io.ErrShortWrite, usage), 1},
		{"nested joins", fmt.Errorf("finalize: %w", errors.Join(usage, errors.Join(cleanupErr, closeErr))), 1},
		{"cancellation", errors.Join(usage, context.Canceled), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CLIExitCode(tc.err))
		})
	}
	combined := errors.Join(usage, closeErr, cleanupErr)
	require.ErrorIs(t, combined, cause)
	require.ErrorIs(t, combined, closeErr)
	require.ErrorIs(t, combined, cleanupErr)
	require.Contains(t, combined.Error(), "invalid command")
	require.Contains(t, combined.Error(), "returned close failure")
	require.Contains(t, combined.Error(), "failed to remove incomplete output")
}
