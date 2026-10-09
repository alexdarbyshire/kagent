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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/adk/pkg/auth"
	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

type cliSessionContext struct {
	context.Context
	id string
}

func (c cliSessionContext) SessionID() string      { return c.id }
func (c cliSessionContext) AppName() string        { return "cli-test" }
func (c cliSessionContext) UserID() string         { return c.id }
func (c cliSessionContext) AgentName() string      { return "cli-test" }
func (c cliSessionContext) Branch() string         { return "" }
func (c cliSessionContext) IsolationScope() string { return "" }

func TestCLIExecutableInvocationHeadersAndIsolation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	var providerCalls [2]atomic.Int32
	var observedMu sync.Mutex
	observed := map[string][]http.Header{}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "headers", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "record", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "recorded"}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		observedMu.Lock()
		observed[request.Header.Get("X-User-Id")] = append(observed[request.Header.Get("X-User-Id")], request.Header.Clone())
		observedMu.Unlock()
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	binding := adk.MCPCLIConfig{Name: "browser", RequiresInvocationHeaders: true, HTTP: adk.HttpMcpServerConfig{
		Params:         adk.StreamableHTTPConnectionParams{Url: remote.URL, Headers: map[string]string{"X-Order": "static"}},
		AllowedHeaders: []string{"Authorization", "X-Order", "X-Allowed"},
	}}
	data, err := json.Marshal(binding)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "binding.json")
	require.NoError(t, os.WriteFile(file, data, 0o600))
	provider := func(ctx context.Context) map[string]string {
		session, ok := ctx.(interface{ SessionID() string })
		if !assert.True(t, ok, "original ADK context must retain SessionID()") {
			return nil
		}
		id := session.SessionID()
		index := 0
		if id == "bob" {
			index = 1
		}
		sequence := providerCalls[index].Add(1)
		return map[string]string{"Authorization": "Bearer exchanged-" + id, "X-Order": "dynamic", "X-Dynamic": fmt.Sprintf("%s-%d", id, sequence)}
	}
	prepare := PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{binding}, true, provider)
	var socketsMu sync.Mutex
	var sockets []string
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: func(original, lifetime context.Context) ([]string, func(), error) {
		environment, cleanup, err := prepare(original, lifetime)
		if err == nil {
			socketsMu.Lock()
			sockets = append(sockets, strings.TrimPrefix(environment[0], cliInvocationSocket+"="))
			socketsMu.Unlock()
		}
		return environment, cleanup, err
	}})
	var wg sync.WaitGroup
	for _, id := range []string{"alice", "bob"} {
		wg.Go(func() {
			for _, allowed := range []string{id, id + "-next"} {
				ctx, _ := a2asrv.NewCallContext(t.Context(), a2asrv.NewServiceParams(map[string][]string{
					"Authorization": {"Bearer incoming-" + id}, "X-Order": {"allowed"}, "X-Allowed": {allowed}, "X-Ignored": {"must-not-forward"},
				}))
				ctx = auth.WithUserID(ctx, id)
				ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled}))
				result, err := executor.ExecuteCommand(cliSessionContext{Context: ctx, id: id}, fmt.Sprintf("%q --binding-file %q record", binary, file), t.TempDir())
				assert.NoError(t, err)
				assert.Contains(t, result, "recorded")
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 4, calls.Load())
	observedMu.Lock()
	defer observedMu.Unlock()
	for index, id := range []string{"alice", "bob"} {
		headers := observed[id]
		require.GreaterOrEqual(t, len(headers), 3)
		require.GreaterOrEqual(t, providerCalls[index].Load(), int32(len(headers)), "every remote request resolves current dynamic authority; scope selection also resolves opaque authority identity")
		seen := map[string]bool{}
		seenAllowed := map[string]bool{}
		for _, header := range headers {
			require.Equal(t, "Bearer exchanged-"+id, header.Get("Authorization"))
			require.Equal(t, "static", header.Get("X-Order"))
			require.Contains(t, []string{id, id + "-next"}, header.Get("X-Allowed"))
			seenAllowed[header.Get("X-Allowed")] = true
			require.Empty(t, header.Get("X-Ignored"))
			require.Contains(t, header.Get("Traceparent"), "01000000000000000000000000000000")
			require.False(t, seen[header.Get("X-Dynamic")])
			seen[header.Get("X-Dynamic")] = true
		}
		require.True(t, seenAllowed[id+"-next"], "same-session follow-up must use its fresh invocation headers")
	}
	require.Len(t, sockets, 4)
	require.NotEqual(t, sockets[0], sockets[1])
	for _, socket := range sockets {
		_, err := os.Stat(filepath.Dir(socket))
		require.True(t, os.IsNotExist(err), "invocation directory removed after successful Bash")
	}
	output, err = exec.Command(binary, "--binding-file", file, "record").CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "requires a live invocation")
	require.EqualValues(t, 4, calls.Load())
}

func TestCLIInvocationRejectsOtherBindingAndClosesOnCancellation(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer remote.Close()
	ctx, cancel := context.WithCancel(t.Context())
	binding := adk.MCPCLIConfig{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}
	environment, cleanup, err := PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{binding}, false, nil)(ctx, ctx)
	require.NoError(t, err)
	defer cleanup()
	socket := strings.TrimPrefix(environment[0], cliInvocationSocket+"=")
	info, err := os.Stat(socket)
	require.NoError(t, err)
	require.EqualValues(t, 0o600, info.Mode().Perm())
	info, err = os.Stat(filepath.Dir(socket))
	require.NoError(t, err)
	require.EqualValues(t, 0o700, info.Mode().Perm())
	t.Setenv(cliInvocationSocket, socket)
	binding.Name = "foreign"
	transport, err := cliInvocationTransport(http.DefaultTransport, binding)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, remote.URL, nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.ErrorContains(t, err, "status 404")
	require.Zero(t, requests.Load())
	cancel()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Dir(socket)); return os.IsNotExist(err) }, time.Second, time.Millisecond)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	require.Zero(t, requests.Load())
}

func TestCLIInvocationShellFailureAndCancellationCleanup(t *testing.T) {
	for _, cancelCall := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", cancelCall), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opened := make(chan string, 1)
			prepare := PrepareCLIEnvironment(t.Context(), nil, false, nil)
			executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: func(original, lifetime context.Context) ([]string, func(), error) {
				environment, cleanup, err := prepare(original, lifetime)
				if err == nil {
					opened <- strings.TrimPrefix(environment[0], cliInvocationSocket+"=")
				}
				return environment, cleanup, err
			}})
			command := "false"
			if cancelCall {
				command = "sleep 100"
			}
			done := make(chan error, 1)
			go func() { _, err := executor.ExecuteCommand(ctx, command, t.TempDir()); done <- err }()
			var socket string
			select {
			case socket = <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("command never opened invocation socket")
			}
			if cancelCall {
				cancel()
			}
			select {
			case err := <-done:
				require.Error(t, err)
				if cancelCall {
					require.ErrorContains(t, err, "canceled")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("command did not release its invocation")
			}
			_, err := os.Stat(filepath.Dir(socket))
			require.True(t, os.IsNotExist(err), "failed/canceled Bash must remove its private invocation files")
		})
	}
}

func TestCLIInvocationSocketSetupFailureStopsShell(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(directory, "missing"))
	executor := tools.NewCommandExecutor(tools.ExecutionConfig{PrepareEnvironment: PrepareCLIEnvironment(t.Context(), nil, false, nil)})
	marker := filepath.Join(directory, "unexpected-execution")
	_, err := executor.ExecuteCommand(t.Context(), fmt.Sprintf("touch %q", marker), directory)
	require.ErrorContains(t, err, "create MCP invocation directory")
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err), "handoff setup failure must stop execution")
}

func TestCLIInvocationSocketFollowsActorLifetime(t *testing.T) {
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	environment, cleanup, err := PrepareCLIEnvironment(owner, nil, false, nil)(t.Context(), t.Context())
	require.NoError(t, err)
	defer cleanup()
	socket := strings.TrimPrefix(environment[0], cliInvocationSocket+"=")
	cancel()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Dir(socket))
		return os.IsNotExist(err)
	}, time.Second, time.Millisecond, "actor shutdown must remove a live invocation socket")
	environment, cleanup, err = PrepareCLIEnvironment(owner, nil, false, nil)(t.Context(), t.Context())
	require.ErrorContains(t, err, "runtime stopped")
	require.Nil(t, environment)
	require.Nil(t, cleanup)
}
