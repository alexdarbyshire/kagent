package mcp

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCLICommandsRetainBrowserOwnershipAcrossInvocations(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, protocol := range []string{"HTTP", "SSE"} {
		t.Run(protocol, func(t *testing.T) {
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "browser", Version: "1"}, nil)
			var mu sync.Mutex
			owners := map[string]bool{}
			for _, name := range []string{"create", "read", "close"} {
				server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					mu.Lock()
					defer mu.Unlock()
					id := request.Session.ID()
					if request.Params.Name == "create" {
						owners[id] = true
					} else if !owners[id] {
						return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "foreign browser identity"}}}, nil
					} else if request.Params.Name == "close" {
						delete(owners, id)
					}
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "owned browser tab"}}}, nil
				})
			}
			var handler http.Handler = mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
			if protocol == "SSE" {
				handler = mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
			}
			remote := httptest.NewServer(handler)
			defer remote.Close()
			binding := adk.MCPCLIConfig{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"create", "read", "close"}}}
			if protocol == "SSE" {
				binding.SSE = &adk.SseMcpServerConfig{Params: adk.SseConnectionParams{Url: remote.URL}, Tools: binding.HTTP.Tools}
				binding.HTTP = adk.HttpMcpServerConfig{}
			}
			owner, cancel := context.WithCancel(t.Context())
			defer cancel()
			data, err := json.Marshal(binding)
			require.NoError(t, err)
			directory := t.TempDir()
			file := filepath.Join(directory, "binding.json")
			require.NoError(t, os.WriteFile(file, data, 0600))
			executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)})
			for _, name := range []string{"create", "read", "close"} {
				// Each execution has a new invocation socket, like separate model calls.
				result, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, fmt.Sprintf("%q --binding-file %q %s", binary, file, name), directory)
				require.NoError(t, err, "%s", result)
				require.Contains(t, result, "owned browser tab")
			}
			mu.Lock()
			defer mu.Unlock()
			require.Empty(t, owners)
		})
	}
}

func TestIntegratedCLIUsesCommandFilesAndPreservesWireNumbers(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "wire", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "record", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"count":{"type":"integer"}},"required":["text","count"],"additionalProperties":false}`)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		require.Contains(t, string(request.Params.Arguments), "9007199254740993")
		return &mcpsdk.CallToolResult{StructuredContent: map[string]any{"count": int64(9007199254740993)}}, nil
	})
	remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer remote.Close()
	binding := adk.MCPCLIConfig{Name: "wire", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"record"}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "arguments.json"), []byte(`{"text":"file","count":9007199254740993}`), 0600))
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{binding}, false, nil)})
	base := fmt.Sprintf("%q --binding-file %q", binary, file)
	for _, command := range []string{
		base + " record --input-file arguments.json",
		`printf '%s' '{"text":"stdin","count":9007199254740993}' | ` + base + " record --input-file -",
		base + ` record --text '--input-file' --count 9007199254740993`,
	} {
		result, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, command, directory)
		require.NoError(t, err)
		require.Contains(t, result, "9007199254740993")
	}
	for _, command := range []string{base + " excluded", base + " record --text ok --count bad", base + " record --input-file arguments.json --text mixed"} {
		_, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, command, directory)
		require.Error(t, err)
	}
	require.EqualValues(t, 3, calls.Load(), "invalid or excluded commands must never dispatch")
}

func TestIntegratedCLIReportsAmbiguousMutationWithoutReplay(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mutation", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "mutate", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var message struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(body, &message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "tools/call" {
				// The action happened, but the connection died before its result arrived.
				calls.Add(1)
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				if err := connection.Close(); err != nil {
					t.Error(err)
				}
				return
			}
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	binding := adk.MCPCLIConfig{Name: "mutation", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL, Headers: map[string]string{"Idempotency-Key": "fixture"}}, Tools: []string{"mutate"}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{binding}, false, nil)})
	command := fmt.Sprintf(`if %q --binding-file %q mutate >result.json 2>diagnostic.txt; then exit 1; else cat diagnostic.txt; fi`, binary, file)
	result, err := executor.ExecuteCommand(cliSessionContext{Context: t.Context(), id: "alice"}, command, directory)
	require.NoError(t, err)
	require.Contains(t, result, "not retried")
	require.EqualValues(t, 1, calls.Load())
}

func TestIntegratedCLICancellationReleasesInvocationAndDoesNotReplay(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	called := make(chan struct{}, 1)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "delay", Version: "1"}, nil)
	release := make(chan struct{})
	server.AddTool(&mcpsdk.Tool{Name: "delay", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		called <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &mcpsdk.CallToolResult{}, nil
		}
	})
	remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer func() { close(release); remote.Close() }()
	binding := adk.MCPCLIConfig{Name: "delay", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"delay"}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	owner, stopOwner := context.WithCancel(t.Context())
	defer stopOwner()
	prepare := PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)
	opened := make(chan string, 1)
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: func(invocation, lifetime context.Context) ([]string, func(), error) {
		environment, cleanup, err := prepare(invocation, lifetime)
		if err == nil {
			opened <- strings.TrimPrefix(environment[0], cliInvocationSocket+"=")
		}
		return environment, cleanup, err
	}})
	invocation, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := executor.ExecuteCommand(cliSessionContext{Context: invocation, id: "alice"}, fmt.Sprintf("%q --binding-file %q delay", binary, file), directory)
		done <- err
	}()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP command did not dispatch")
	}
	socket := <-opened
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "canceled")
	case <-time.After(5 * time.Second):
		t.Fatal("canceled command did not return")
	}
	_, err = os.Stat(filepath.Dir(socket))
	require.True(t, os.IsNotExist(err), "canceled invocation must remove its socket")
	command := exec.Command(binary, "--binding-file", file, "delay")
	command.Env = append(command.Environ(), cliInvocationSocket+"="+socket)
	output, err = command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "not retried")
	require.EqualValues(t, 1, calls.Load())
}

func TestIntegratedCLICancellationStopsIncompleteInitialization(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	started := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Error(err)
			return
		}
		started <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-release:
		}
		finished <- struct{}{}
	}))
	defer func() { close(release); remote.Close() }()
	binding := adk.MCPCLIConfig{Name: "pending", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	directory := t.TempDir()
	file := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	owner, stopOwner := context.WithCancel(t.Context())
	defer stopOwner()
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{binding}, false, nil)})
	invocation, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := executor.ExecuteCommand(cliSessionContext{Context: invocation, id: "alice"}, fmt.Sprintf("%q --binding-file %q --help", binary, file), directory)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("initialization did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "canceled")
	case <-time.After(5 * time.Second):
		t.Fatal("canceled command did not return")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled initialization stayed alive until actor shutdown")
	}
}
