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
)

func TestNativeStandaloneStreamOutlivesRequestTimeout(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "continuation", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var initializations atomic.Int32
	var stallBody atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && stallBody.Load() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{"))
			w.(http.Flusher).Flush()
			<-request.Context().Done()
			return
		}
		if request.Method == http.MethodPost && request.Header.Get("Mcp-Session-Id") == "" {
			initializations.Add(1)
		}
		handler.ServeHTTP(w, request)
	}))
	t.Cleanup(peer.Close)
	owner := NewClientLifecycle(t.Context(), nil)
	t.Cleanup(func() { owner.Close() })
	timeout := 1.0
	sets := CreateToolsets(t.Context(), []adk.HttpMcpServerConfig{{Params: adk.StreamableHTTPConnectionParams{Url: peer.URL, Timeout: &timeout}}}, nil, nil, false, nil, owner)
	require.Len(t, sets, 1)
	ctx := testReadonlyContext{Context: t.Context()}
	tools, err := sets[0].Tools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	before := initializations.Load()
	time.Sleep(1200 * time.Millisecond)
	tools, err = sets[0].Tools(ctx)
	require.NoError(t, err, "a request deadline must not expire the connection-owned event stream")
	require.Len(t, tools, 1)
	require.Equal(t, before, initializations.Load(), "continuation must retain the same MCP relationship")
	stallBody.Store(true)
	started := time.Now()
	_, err = sets[0].Tools(ctx)
	require.Error(t, err, "a stalled POST response must still honor its request deadline")
	require.GreaterOrEqual(t, time.Since(started), 900*time.Millisecond)
	require.Less(t, time.Since(started), 2*time.Second)
}
