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
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/adk/pkg/mcp"
	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestRetainedCLIRecoversAfterForbiddenWithoutReplay(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var denied, calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "archive", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"denied":{"type":"boolean"}}}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "searched once"}}}, nil
	})
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
	defer remote.Close()
	binding := adk.MCPCLIConfig{Name: "archive", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"search"}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: mcp.PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)})
	base := fmt.Sprintf("%q --binding-file %q search", binary, file)
	result, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, base+" --denied true", directory)
	require.Error(t, err)
	require.Contains(t, result, "acceptance is unknown")
	require.EqualValues(t, 1, denied.Load())
	require.Zero(t, calls.Load(), "authorization rejected the operation before backend dispatch")
	result, err = executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, base+" --denied false", directory)
	require.NoError(t, err, "%s", result)
	require.Contains(t, result, "searched once")
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 1, denied.Load(), "the denied action must not be replayed")
}
