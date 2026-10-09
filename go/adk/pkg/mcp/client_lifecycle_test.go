package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

func TestNativeHostShutdownTerminatesOwnedConnection(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "shutdown", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var deletes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Method == http.MethodDelete {
			require.Equal(t, "Bearer static", request.Header.Get("Authorization"))
			require.NotEmpty(t, request.Header.Get("Mcp-Session-Id"))
			deletes.Add(1)
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	toolsets := CreateToolsets(owner, []adk.HttpMcpServerConfig{{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL, Headers: map[string]string{"Authorization": "Bearer static"}}}}, nil, nil, false, nil)
	require.Len(t, toolsets, 1)
	tools, err := toolsets[0].Tools(testReadonlyContext{Context: t.Context()})
	require.NoError(t, err)
	require.Len(t, tools, 1)
	before := deletes.Load() // Metadata discovery has already released its connection.
	cancel()
	require.Eventually(t, func() bool { return deletes.Load() > before }, 2*time.Second, 10*time.Millisecond, "host shutdown must release the native lazy toolset connection")
}

func TestManagedShutdownReportsRemoteEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		want string
	}{
		{"accepted", http.StatusOK, "accepted"},
		{"missing", http.StatusNotFound, "already_missing"},
		{"gone", http.StatusGone, "already_missing"},
		{"unsupported", http.StatusMethodNotAllowed, "unsupported"},
		{"unauthorized", http.StatusUnauthorized, "auth_refused"},
		{"forbidden", http.StatusForbidden, "auth_refused"},
		{"server_failure", http.StatusInternalServerError, "remote_refused"},
		{"redirect", http.StatusTemporaryRedirect, "remote_refused"},
		{"hung", 0, "timed_out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "evidence", Version: "1"}, nil)
			server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{}, nil
			})
			handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
			var redirected atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/other" {
					redirected.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				if request.Method == http.MethodGet {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if request.Method == http.MethodDelete {
					require.Equal(t, "Bearer static", request.Header.Get("Authorization"))
					require.NotEmpty(t, request.Header.Get("Mcp-Session-Id"))
					if test.code == 0 {
						<-request.Context().Done()
						return
					}
					if test.code == http.StatusTemporaryRedirect {
						w.Header().Set("Location", "/other")
					}
					w.WriteHeader(test.code)
					return
				}
				handler.ServeHTTP(w, request)
			}))
			defer remote.Close()
			owner := NewClientLifecycle(t.Context(), nil)
			toolsets := CreateToolsets(t.Context(), []adk.HttpMcpServerConfig{{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL, Headers: map[string]string{"Authorization": "Bearer static"}}}}, nil, nil, false, nil, owner)
			require.Len(t, toolsets, 1)
			_, err := toolsets[0].Tools(testReadonlyContext{Context: t.Context()})
			require.NoError(t, err)
			started := time.Now()
			outcomes := owner.Close()
			require.Less(t, time.Since(started), 1500*time.Millisecond)
			require.Len(t, outcomes, 2) // Classification and native discovery both use SDK connections.
			for _, outcome := range outcomes {
				require.Equal(t, test.want, outcome.Status)
				require.Equal(t, test.code, outcome.HTTPStatus)
				require.True(t, outcome.LocalReleased)
			}
			_, err = toolsets[0].Tools(testReadonlyContext{Context: t.Context()})
			require.Error(t, err, "the released native connection must not remain usable")
			require.Equal(t, outcomes, owner.Close(), "shutdown is idempotent")
			require.Zero(t, redirected.Load(), "termination authority cannot follow a binding redirect")
		})
	}
}

func TestManagedShutdownBoundsManyHungRelationships(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "hung", Version: "1"}, nil)
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var hanging atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Method == http.MethodDelete && hanging.Load() {
			<-request.Context().Done()
			return
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	owner := NewClientLifecycle(t.Context(), nil)
	configs := make([]adk.HttpMcpServerConfig, 6)
	for index := range configs {
		configs[index].Params.Url = remote.URL
	}
	toolsets := CreateToolsets(t.Context(), configs, nil, nil, false, nil, owner)
	require.Len(t, toolsets, len(configs))
	for _, toolset := range toolsets {
		_, err := toolset.Tools(testReadonlyContext{Context: t.Context()})
		require.NoError(t, err)
	}
	hanging.Store(true)
	started := time.Now()
	outcomes := owner.Close()
	require.Less(t, time.Since(started), 1500*time.Millisecond, "total shutdown must not multiply by relationship count")
	require.Len(t, outcomes, 12)
	for _, outcome := range outcomes {
		require.True(t, outcome.LocalReleased)
	}
}

func TestManagedShutdownRejectsInvocationOnlyAuthority(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "forwarded", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var deletes atomic.Int32
	var authorizedPosts atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Method == http.MethodDelete {
			deletes.Add(1)
		}
		if request.Method == http.MethodPost && request.Header.Get("Authorization") == "Bearer forwarded" {
			authorizedPosts.Add(1)
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	owner := NewClientLifecycle(t.Context(), nil)
	toolsets := CreateToolsets(t.Context(), []adk.HttpMcpServerConfig{{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, AllowedHeaders: []string{"Authorization"}}}, nil, nil, false, nil, owner)
	invocation := a2aCtx(map[string][]string{"Authorization": {"Bearer forwarded"}})
	tools, err := toolsets[0].Tools(testReadonlyContext{Context: invocation})
	require.NoError(t, err)
	require.Len(t, tools, 1)
	before := authorizedPosts.Load()
	_, err = tools[0].(nativeRunnableTool).Run(lifecycleToolContext{underlying: invocation}, map[string]any{})
	require.NoError(t, err)
	require.Greater(t, authorizedPosts.Load(), before, "the current tool call must still authenticate without provider custody")
	outcomes := owner.Close()
	require.Len(t, outcomes, 2)
	require.Equal(t, "authority_unavailable", outcomes[len(outcomes)-1].Status)
	for _, outcome := range outcomes {
		require.True(t, outcome.LocalReleased)
	}
	require.Zero(t, deletes.Load(), "context-only authority must not be snapshotted for shutdown")
}

func TestNativeSSEStreamOutlivesInvocationAndClosesWithHost(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sse", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var finished atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer static", request.Header.Get("Authorization"))
		handler.ServeHTTP(w, request)
		if request.Method == http.MethodGet {
			finished.Add(1)
		}
	}))
	defer remote.Close()
	defer remote.CloseClientConnections()
	owner := NewClientLifecycle(t.Context(), nil)
	toolsets := CreateToolsets(t.Context(), nil, []adk.SseMcpServerConfig{{Params: adk.SseConnectionParams{Url: remote.URL, Headers: map[string]string{"Authorization": "Bearer static"}}}}, nil, false, nil, owner)
	require.Len(t, toolsets, 1)
	require.Eventually(t, func() bool { return finished.Load() == 1 }, time.Second, 10*time.Millisecond)
	invocation, end := context.WithCancel(t.Context())
	tools, err := toolsets[0].Tools(testReadonlyContext{Context: invocation})
	require.NoError(t, err)
	require.Len(t, tools, 1)
	end()
	require.EqualValues(t, 1, finished.Load(), "ending discovery invocation must not close the retained SSE stream")
	outcomes := owner.Close()
	require.Eventually(t, func() bool { return finished.Load() == 2 }, time.Second, 10*time.Millisecond)
	for _, outcome := range outcomes {
		require.Equal(t, "unsupported_transport", outcome.Status)
		require.True(t, outcome.LocalReleased)
	}
}

type lifecycleToolContext struct {
	adkagent.Context
	underlying context.Context
}

func (c lifecycleToolContext) Deadline() (time.Time, bool)                        { return c.underlying.Deadline() }
func (c lifecycleToolContext) Done() <-chan struct{}                              { return c.underlying.Done() }
func (c lifecycleToolContext) Err() error                                         { return c.underlying.Err() }
func (c lifecycleToolContext) Value(key any) any                                  { return c.underlying.Value(key) }
func (lifecycleToolContext) SessionID() string                                    { return "test-session" }
func (lifecycleToolContext) AppName() string                                      { return "test-app" }
func (lifecycleToolContext) UserID() string                                       { return "test-user" }
func (lifecycleToolContext) AgentName() string                                    { return "test-agent" }
func (lifecycleToolContext) Branch() string                                       { return "" }
func (lifecycleToolContext) IsolationScope() string                               { return "" }
func (lifecycleToolContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
