package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/adk/pkg/mcp"
	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestRetainedCLIRecoversAfterForbiddenWithoutReplay(t *testing.T) {
	var denied, calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "archive", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"denied":{"type":"boolean"}}}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "searched once"}}}, nil
	})
	run := forbiddenCLICommands(t, server, &denied)
	result, err := run("search --denied true")
	require.Error(t, err)
	require.Contains(t, result, "acceptance is unknown")
	require.EqualValues(t, 1, denied.Load())
	require.Zero(t, calls.Load(), "authorization rejected the operation before backend dispatch")
	result, err = run("search --denied false")
	require.Error(t, err)
	require.Contains(t, result, "MCP relationship state lost")
	require.Contains(t, result, "prior resource cleanup is unconfirmed")
	require.Contains(t, result, "command not dispatched")
	require.Zero(t, calls.Load(), "the state-loss report must precede any new tool dispatch")
	result, err = run("search --denied false")
	require.NoError(t, err, "%s", result)
	require.Contains(t, result, "searched once")
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 1, denied.Load(), "the denied action must not be replayed")
}

func TestRetainedBrowserReportsLostContinuityBeforeNewDispatch(t *testing.T) {
	var denied, reads atomic.Int32
	var mu sync.Mutex
	owners := map[string]bool{}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "browser", Version: "1"}, nil)
	for _, name := range []string{"open", "search"} {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","properties":{"denied":{"type":"boolean"}}}`)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if request.Params.Name == "open" {
				owners[request.Session.ID()] = true
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "opened owned tab"}}}, nil
			}
			reads.Add(1)
			if !owners[request.Session.ID()] {
				return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "previous tab unavailable in fresh context"}}}, nil
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "read owned tab"}}}, nil
		})
	}
	run := forbiddenCLICommands(t, server, &denied)
	result, err := run("open")
	require.NoError(t, err, "%s", result)
	require.Contains(t, result, "opened owned tab")
	result, err = run("search --denied true")
	require.Error(t, err)
	require.Contains(t, result, "acceptance is unknown")
	result, err = run("search")
	require.Error(t, err)
	require.Contains(t, result, "MCP relationship state lost")
	require.Contains(t, result, "command not dispatched")
	require.Zero(t, reads.Load(), "a browser read cannot silently run in a replacement context")
	result, err = run("search")
	require.Error(t, err)
	require.Contains(t, result, "previous tab unavailable in fresh context")
	require.EqualValues(t, 1, reads.Load())
	require.EqualValues(t, 1, denied.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, owners, 1, "a fresh context does not prove cleanup of the old tab")
}

func forbiddenCLICommands(t *testing.T, server *mcpsdk.Server, denied *atomic.Int32) func(string) (string, error) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				t.Error(readErr)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var message struct {
				Method string `json:"method"`
				Params struct {
					Arguments struct {
						Denied bool `json:"denied"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if decodeErr := json.Unmarshal(body, &message); decodeErr != nil {
				t.Error(decodeErr)
				return
			}
			if message.Method == "tools/call" && message.Params.Arguments.Denied {
				denied.Add(1)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		handler.ServeHTTP(w, request)
	}))
	t.Cleanup(remote.Close)
	binding := adk.MCPCLIConfig{Name: "archive", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"search", "open"}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	owner, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: mcp.PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)})
	base := fmt.Sprintf("%q --binding-file %q", binary, file)
	return func(command string) (string, error) {
		return executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, base+" "+command, directory)
	}
}
