package nativeartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

type discoveryContext struct {
	context.Context
	agent.ReadonlyContext
}

func (c discoveryContext) Deadline() (deadline time.Time, ok bool) { return c.Context.Deadline() }
func (c discoveryContext) Done() <-chan struct{}                   { return c.Context.Done() }
func (c discoveryContext) Err() error                              { return c.Context.Err() }
func (c discoveryContext) Value(key any) any                       { return c.Context.Value(key) }

func TestADKDiscoveryRecoversExpiredGatewaySession(t *testing.T) {
	var expire atomic.Bool
	var rejected atomic.Int32
	var sessionMu sync.Mutex
	var firstSession string
	seen := map[string]bool{}
	server := mcp.NewServer(&mcp.Implementation{Name: "gateway-peer", Version: "test"}, &mcp.ServerOptions{GetSessionID: uuid.NewString})
	server.AddTool(&mcp.Tool{Name: "identity", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("Mcp-Session-Id")
		sessionMu.Lock()
		if sid != "" {
			seen[sid] = true
		}
		if sid != "" && firstSession == "" {
			firstSession = sid
		}
		isFirst := sid != "" && sid == firstSession
		sessionMu.Unlock()
		if expire.Load() && isFirst {
			rejected.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			if _, err := fmt.Fprint(w, "mcp: invalid session ID header"); err != nil {
				t.Error(err)
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer peer.Close()
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: NewHTTPTransport(peer.URL, peer.Client()), Client: NewClient()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := discoveryContext{Context: t.Context()}
	tools, err := ts.Tools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("initial discovery tools=%d err=%v", len(tools), err)
	}
	expire.Store(true)
	tools, err = ts.Tools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("expired discovery tools=%d err=%v", len(tools), err)
	}
	sessionMu.Lock()
	count := len(seen)
	sessionMu.Unlock()
	if count != 2 || rejected.Load() != 1 {
		t.Fatalf("session identities=%d expired requests=%d", count, rejected.Load())
	}
}

func TestDiscoveryRecoveryDoesNotRemapOtherFailuresOrCalls(t *testing.T) {
	for _, tc := range []struct {
		name, method, session, body string
		status                      int
	}{{"tool mutation", "tools/call", uuid.NewString(), "mcp: invalid session ID header", 400}, {"unrelated bad request", "tools/list", uuid.NewString(), "mcp: invalid request parameters", 400}, {"nonUUID", "tools/list", "not-a-uuid", "mcp: invalid session ID header", 400}, {"denied", "tools/list", uuid.NewString(), "mcp: invalid session ID header", 403}, {"body suffix", "tools/list", uuid.NewString(), "mcp: invalid session ID header\n", 400}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("caller auth lost")
				}
				w.WriteHeader(tc.status)
				if _, err := fmt.Fprint(w, tc.body); err != nil {
					t.Error(err)
				}
			}))
			defer peer.Close()
			caller := peer.Client()
			original := caller.Transport
			transport := NewHTTPTransport(peer.URL, caller)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, peer.URL, strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{}}`, tc.method)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Mcp-Session-Id", tc.session)
			req.Header.Set("Authorization", "Bearer test-only")
			response, err := transport.HTTPClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || string(data) != tc.body || calls.Load() != 1 {
				t.Fatalf("status=%d body=%q requests=%d", response.StatusCode, data, calls.Load())
			}
			if caller.Transport != original {
				t.Fatal("caller client modified")
			}
		})
	}
}

func TestExpiredToolCallIsNotReplayed(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "mutation-peer", Version: "test"}, &mcp.ServerOptions{GetSessionID: uuid.NewString})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handler.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var message struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(data, &message); err != nil {
			t.Error(err)
			return
		}
		if message.Method == "tools/call" {
			calls.Add(1)
			w.WriteHeader(400)
			if _, err := fmt.Fprint(w, "mcp: invalid session ID header"); err != nil {
				t.Error(err)
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer peer.Close()
	client := NewClient()
	cs, err := client.Connect(t.Context(), NewHTTPTransport(peer.URL, peer.Client()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cs.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "mutation", Arguments: map[string]any{}})
	if err == nil || errors.Is(err, mcp.ErrSessionMissing) || calls.Load() != 1 {
		t.Fatalf("mutation err=%v calls=%d", err, calls.Load())
	}
}

// An optional local gateway v1.6.0 fixture must use sessionTtl: 1s. Its reaper
// runs every 30 seconds; no live credential or estate endpoint belongs here.
func TestLocalGatewayIdleDiscovery(t *testing.T) {
	endpoint := os.Getenv("NATIVE_MCP_EXPIRY_ENDPOINT")
	if endpoint == "" {
		t.Skip("set local short-TTL gateway endpoint to run the pinned-gateway probe")
	}
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: NewHTTPTransport(endpoint, nil), Client: NewClient()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := discoveryContext{Context: t.Context()}
	before, err := ts.Tools(ctx)
	if err != nil || len(before) == 0 {
		t.Fatalf("initial tools=%d err=%v", len(before), err)
	}
	timer := time.NewTimer(32 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	after, err := ts.Tools(ctx)
	if err != nil || len(after) != len(before) {
		t.Fatalf("after expiry tools=%d expected=%d err=%v", len(after), len(before), err)
	}
}
