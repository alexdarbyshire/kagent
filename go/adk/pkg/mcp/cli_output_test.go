package mcp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIOutputFileLimitAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  bool
		cancel bool
	}{
		{name: "exact limit retained"}, {name: "over limit removed", extra: true}, {name: "cancelled removed", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "response.json")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			require.NoError(t, err)
			output := &cliOutputFile{file: file}
			data := bytes.Repeat([]byte("x"), cliOutputLimit)
			n, err := output.Write(data)
			require.NoError(t, err)
			require.Equal(t, len(data), n)
			if tc.extra {
				n, err = output.Write([]byte("\n"))
				require.ErrorContains(t, err, "32 MiB")
				require.Zero(t, n)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			err = output.finish(ctx)
			if tc.extra || tc.cancel {
				require.Error(t, err)
				_, err = os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				saved, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, data, saved)
			}
		})
	}
}

func TestCLIOutputFileCleanupPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, err)
	output := &cliOutputFile{file: file, err: errors.New("injected write failure")}
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0600))
	require.ErrorContains(t, output.finish(t.Context()), "replacement was not removed")
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(saved))
}
