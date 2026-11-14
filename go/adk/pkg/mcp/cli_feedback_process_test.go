package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
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
)

func TestCLIProcessFailureGuidance(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)

	for _, scenario := range []struct {
		name       string
		failMethod string
		guidance   string
		calls      int32
	}{
		{name: "Given a reachable provider, when a valid call succeeds, assert that its result is returned", calls: 1},
		{name: "Given failed discovery, when a command runs, assert that feedback identifies no dispatch", failMethod: "tools/list", guidance: "tool call not dispatched"},
		{name: "Given a failed call response, when a command runs, assert that feedback preserves uncertain acceptance", failMethod: "tools/call", guidance: "acceptance is unknown", calls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Int32
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "feedback", Version: "1"}, nil)
			server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fixture result"}}}, nil
			})
			handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read request: %v", err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct{ Method string }
					if err := json.Unmarshal(body, &request); err != nil {
						t.Errorf("decode request: %v", err)
						return
					}
					if request.Method == "tools/call" {
						calls.Add(1)
					}
					if request.Method == scenario.failMethod {
						http.Error(w, "synthetic failure", http.StatusInternalServerError)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer remote.Close()
			binding := filepath.Join(t.TempDir(), "binding.json")
			require.NoError(t, os.WriteFile(binding, []byte(`{"name":"feedback","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
			command := exec.Command(binary, "--binding-file", binding, "read")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err = command.Run()
			require.Equal(t, scenario.calls, calls.Load(), "%s", stderr.String())
			if scenario.failMethod == "" {
				require.NoError(t, err, "%s", stderr.String())
				require.Contains(t, stdout.String(), "fixture result")
				return
			}
			var failure *exec.ExitError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, 1, failure.ExitCode())
			require.Empty(t, stdout.String())
			require.Contains(t, stderr.String(), "feedback")
			require.Contains(t, stderr.String(), scenario.guidance)
		})
	}
}
