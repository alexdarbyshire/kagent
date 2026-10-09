package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCommandResultAdapterReceivesCurrentCallerAndNeverReplaysFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{"count": int64(9007199254740993)}, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "original"}}}, nil
	})
	remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	t.Cleanup(remote.Close)
	binding := adk.MCPCLIConfig{Name: "fixture", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"read"}}}
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0600))
	owner, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	owner = WithResultTransformer(owner, func(ctx context.Context, data json.RawMessage) (json.RawMessage, error) {
		caller, ok := ctx.(interface{ SessionID() string })
		require.True(t, ok, "original caller methods survive cancellation wrappers")
		require.Contains(t, string(data), "9007199254740993")
		if caller.SessionID() == "bob" {
			return nil, fmt.Errorf("adapter refused Bob; operation not replayed")
		}
		var result map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &result))
		result["adapted"] = json.RawMessage(`true`)
		return json.Marshal(result)
	})
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)})
	command := fmt.Sprintf("%q --binding-file %q read", binary, file)
	result, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, command, directory)
	require.NoError(t, err)
	require.Contains(t, result, `"adapted":true`)
	require.Contains(t, result, "9007199254740993")
	_, err = executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "bob"}, command, directory)
	require.ErrorContains(t, err, "adapter refused Bob")
	require.Equal(t, int32(2), calls.Load())
}
