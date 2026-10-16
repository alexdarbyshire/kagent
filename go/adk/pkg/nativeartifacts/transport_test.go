package nativeartifacts

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Replay a proxy that repeatedly ends an otherwise valid, idle standalone GET.
// Speed up only the server's retry interval; use the real SDK's retry budget.
func TestStatefulHTTPToolsSurviveIdleStandaloneStreamClosures(t *testing.T) {
	for _, jsonResponse := range []bool{true, false} {
		t.Run(fmt.Sprintf("json_response_%t", jsonResponse), func(t *testing.T) { testStatefulHTTPAfterIdle(t, jsonResponse) })
	}
}

func testStatefulHTTPAfterIdle(t *testing.T, jsonResponse bool) {
	t.Helper()
	var gets atomic.Int32
	release := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "stateful-browser", Version: "test"}, nil)
	server.AddTool(&mcp.Tool{Name: "identity", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: req.Session.ID()}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: jsonResponse})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if _, err := fmt.Fprint(w, "retry: 1\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	client := NewClient()
	cs, err := client.Connect(t.Context(), NewHTTPTransport(httpServer.URL, httpServer.Client()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Error(err)
		}
	})
	before, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	ended := make(chan error, 1)
	go func() { ended <- cs.Wait() }()
	select {
	case err := <-ended:
		t.Fatalf("idle side stream destroyed usable stateful POST session: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	after, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	identity := func(result *mcp.CallToolResult) string { return result.Content[0].(*mcp.TextContent).Text }
	if identity(before) == "" || identity(before) != identity(after) {
		t.Fatal("stateful MCP identity changed across idle interval")
	}
	if gets.Load() != 0 {
		t.Fatalf("request-response transport opened %d unnecessary standalone GETs", gets.Load())
	}
}
