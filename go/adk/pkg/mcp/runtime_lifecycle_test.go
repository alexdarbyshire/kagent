package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

func TestRuntimeShutdownWaitsForRootAndSharedTermination(t *testing.T) {
	if remote := os.Getenv("KAGENT_TEST_SHUTDOWN_REMOTE"); remote != "" {
		runLifecycleSignalProcess(t, remote)
		return
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "shutdown", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	var completed atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Method == http.MethodDelete {
			require.Equal(t, "Bearer delegated", request.Header.Get("Authorization"))
			time.Sleep(700 * time.Millisecond)
			completed.Add(1)
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestRuntimeShutdownWaitsForRootAndSharedTermination$")
	command.Env = append(os.Environ(), "KAGENT_TEST_SHUTDOWN_REMOTE="+remote.URL, "GORACE=atexit_sleep_ms=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Process.Kill() })
	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan())
	require.Equal(t, "READY", scanner.Text())
	started := time.Now()
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	require.NoError(t, command.Wait(), "%s", stderr.String())
	require.EqualValues(t, 2, completed.Load(), "both runtime-owned MCP relationships finish DELETE before process exit")
	require.Less(t, time.Since(started), 1300*time.Millisecond, "root and Shared owners share one shutdown budget")
	require.Equal(t, 2, strings.Count(stderr.String(), "MCP termination outcome"), "%s", stderr.String())
	require.Equal(t, 2, strings.Count(stderr.String(), "status=accepted"), "%s", stderr.String())
}

func TestRuntimeShutdownRejectsNewOwnersAndRelationships(t *testing.T) {
	ctx, shutdown := WithClientLifecycleShutdown(t.Context())
	owner := NewClientLifecycle(ctx, nil)
	_, err := owner.relationship(relationshipKey{binding: "existing"})
	require.NoError(t, err)
	shutdown()
	_, err = owner.relationship(relationshipKey{binding: "new"})
	require.ErrorContains(t, err, "lifecycle stopped")
	late := NewClientLifecycle(ctx, nil)
	_, err = late.relationship(relationshipKey{binding: "late"})
	require.ErrorContains(t, err, "lifecycle stopped")
}

func runLifecycleSignalProcess(t *testing.T, remote string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ctx, shutdown := WithClientLifecycleShutdown(ctx)
	defer func() { cancel(); shutdown() }()
	for _, name := range []string{"root", "shared"} {
		owner := NewClientLifecycle(ctx, nil)
		toolsets := CreateToolsets(ctx, []*configv1.ADKHTTPMCPServerConfig{{Params: &configv1.ADKHTTPConnectionParams{Url: remote + "/" + name, Headers: map[string]string{"Authorization": "Bearer delegated"}}}}, nil, nil, false, nil, owner)
		_, err := toolsets[0].Tools(testReadonlyContext{Context: ctx})
		require.NoError(t, err)
	}
	stop, release := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer release()
	fmt.Println("READY")
	<-stop.Done()
}
