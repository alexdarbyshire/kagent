package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

func TestManagedShutdownClosesStatelessSDKSubscriptions(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "subscriptions", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true})
	var active atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	defer remote.CloseClientConnections()
	owner := NewClientLifecycle(t.Context(), nil)
	toolsets := CreateToolsets(t.Context(), []*configv1.ADKHTTPMCPServerConfig{{Params: &configv1.ADKHTTPConnectionParams{Url: remote.URL}}}, nil, nil, false, nil, owner)
	require.Len(t, toolsets, 1)
	tools, err := toolsets[0].Tools(testReadonlyContext{Context: t.Context()})
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Eventually(t, func() bool { return active.Load() > 0 }, time.Second, time.Millisecond)
	owner.Close()
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, time.Millisecond, "SDK subscription POSTs must stop with their native toolset")
}
