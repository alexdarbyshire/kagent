//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCommandExecutorReturnKillsBackgroundMCPBridge(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	release := make(chan struct{})
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "delayed", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "delay", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		if err := os.WriteFile(filepath.Join(directory, "called"), nil, 0600); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &mcpsdk.CallToolResult{}, nil
		}
	})
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	defer func() {
		close(release)
		httpServer.Close()
	}()
	binding, err := json.Marshal(adk.MCPCLIConfig{Name: "delayed", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: httpServer.URL}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "binding.json"), binding, 0600))
	// Wait for the real MCP dispatch before the parent shell exits successfully.
	command := `./mcp-cli --binding-file binding.json delay >/dev/null 2>&1 & echo $! > bridge.pid; for ((i=0; i<500; i++)); do if [[ -f called ]]; then exit 0; fi; sleep 0.01; done; exit 1`
	_, err = NewCommandExecutor(ExecutionConfig{}).ExecuteCommand(t.Context(), command, directory)
	data, readErr := os.ReadFile(filepath.Join(directory, "bridge.pid"))
	require.NoError(t, readErr)
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, parseErr)
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load(), "execution must dispatch once without replay")
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		_, state, _ := strings.Cut(string(data), ") ")
		return strings.HasPrefix(state, "Z ")
	}, time.Second, 10*time.Millisecond, "bridge survived successful parent-shell exit")
}

func TestCommandExecutorCancellationKillsScriptDescendants(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewCommandExecutor(ExecutionConfig{}).ExecuteCommand(ctx, `bash -c 'sleep 60 & echo $! > child.pid; wait' & echo $! > shell.pid; wait`, directory)
		done <- err
	}()
	var pids []int
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for _, name := range []string{"shell.pid", "child.pid"} {
		var data []byte
		require.Eventually(t, func() bool {
			var err error
			data, err = os.ReadFile(filepath.Join(directory, name))
			return err == nil && len(strings.TrimSpace(string(data))) > 0
		}, 3*time.Second, 10*time.Millisecond)
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		pids = append(pids, pid)
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Bash descendants kept canceled execution alive")
	}
	for _, pid := range pids {
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
			if errors.Is(err, os.ErrNotExist) {
				return true
			}
			_, state, _ := strings.Cut(string(data), ") ")
			return strings.HasPrefix(state, "Z ")
		}, 3*time.Second, 10*time.Millisecond, "descendant %d still running", pid)
	}
}
