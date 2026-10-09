package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRunnerKeepsConversationBranchIsolationAndAuthoritySeparate(t *testing.T) {
	for _, cli := range []bool{false, true} {
		t.Run(fmt.Sprintf("cli=%t", cli), func(t *testing.T) {
			t.Setenv("KAGENT_PROPAGATE_TOKEN", "true")
			var mu sync.Mutex
			owners := map[string]bool{}
			peerServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "scope", Version: "1"}, nil)
			for _, name := range []string{"create", "read", "close"} {
				peerServer.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					mu.Lock()
					defer mu.Unlock()
					id := r.Session.ID()
					if r.Params.Name == "create" {
						owners[id] = true
					} else if !owners[id] {
						return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "foreign ownership"}}}, nil
					} else if r.Params.Name == "close" {
						delete(owners, id)
					}
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "owned tab"}}}, nil
				})
			}
			peer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return peerServer }, nil))
			t.Cleanup(peer.Close)
			run, _ := newScopedBrowserRunnerFixture(t, cli, peer.URL, true)
			callContext := func(token string, scope fixtureScope) context.Context {
				ctx, _ := a2asrv.NewCallContext(t.Context(), a2asrv.NewServiceParams(map[string][]string{"Authorization": {"Bearer " + token}}))
				return context.WithValue(ctx, fixtureScopeKey{}, scope)
			}
			alice := callContext("alice", fixtureScope{})
			require.Contains(t, run(alice, "alice", "conversation", "create"), "owned tab")
			require.Contains(t, run(alice, "alice", "conversation", "read"), "owned tab")
			for _, foreign := range []struct {
				name, conversation, token string
				scope                     fixtureScope
			}{
				{name: "conversation", conversation: "other-conversation", token: "alice"},
				{name: "branch", conversation: "conversation", token: "alice", scope: fixtureScope{branch: "foreign"}},
				{name: "isolation", conversation: "conversation", token: "alice", scope: fixtureScope{isolation: "foreign"}},
				{name: "authority", conversation: "conversation", token: "bob"},
			} {
				ctx := callContext(foreign.token, foreign.scope)
				denied := "foreign ownership"
				if cli {
					denied = "returned isError"
				}
				require.Contains(t, run(ctx, "alice", foreign.conversation, "read"), denied, foreign.name)
				require.Contains(t, run(ctx, "alice", foreign.conversation, "close"), denied, foreign.name)
			}
			mu.Lock()
			require.Len(t, owners, 1, "foreign close attempts cannot remove the owning caller tab")
			mu.Unlock()
		})
	}
}

// This peer intentionally implements only the fixture's wire responses. The
// runtime must use its real SDK, runner, and command processes to negotiate it.
func TestRunnerNegotiatesModernSessionlessMCP(t *testing.T) {
	for _, cli := range []bool{false, true} {
		t.Run(fmt.Sprintf("cli=%t", cli), func(t *testing.T) {
			var mu sync.Mutex
			var calls, deletes int
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				require.Empty(t, r.Header.Get("Mcp-Session-Id"), "no invented modern protocol session")
				if r.Method == http.MethodDelete {
					deletes++
					w.WriteHeader(405)
					return
				}
				if r.Method != http.MethodPost {
					w.WriteHeader(405)
					return
				}
				var input struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Meta map[string]any `json:"_meta"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if len(input.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				var result string
				switch input.Method {
				case "initialize":
					result = `{"protocolVersion":"2026-07-28","capabilities":{"tools":{}},"serverInfo":{"name":"modern","version":"1"}}`
				case "tools/list":
					require.Equal(t, "2026-07-28", input.Params.Meta["io.modelcontextprotocol/protocolVersion"])
					result = `{"tools":[{"name":"read","inputSchema":{"type":"object"}}]}`
				case "tools/call":
					calls++
					require.Equal(t, "2026-07-28", input.Params.Meta["io.modelcontextprotocol/protocolVersion"])
					result = `{"content":[{"type":"text","text":"modern read"}]}`
				default:
					result = `{}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, input.ID, result)
			}))
			t.Cleanup(peer.Close)
			run, cancel := newBrowserRunnerFixture(t, cli, peer.URL)
			require.Contains(t, run("alice", "read"), "modern read")
			require.Contains(t, run("alice", "read"), "modern read")
			cancel()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			require.Equal(t, 2, calls)
			require.Zero(t, deletes, "sessionless SDK relationships have no termination DELETE")
			mu.Unlock()
		})
	}
}

func TestRunnerReportsLostMCPRelationshipWithoutReplay(t *testing.T) {
	for _, testCase := range []struct{ cli, missing bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		cli, missing := testCase.cli, testCase.missing
		t.Run(fmt.Sprintf("cli=%t/missing=%t", cli, missing), func(t *testing.T) {
			var mu sync.Mutex
			var sessions, mutations int
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(405)
					return
				}
				var input struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if len(input.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				var result string
				switch input.Method {
				case "initialize":
					sessions++
					w.Header().Set("Mcp-Session-Id", fmt.Sprintf("owned-%d", sessions))
					result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"ambiguous","version":"1"}}`
				case "tools/list":
					result = `{"tools":[{"name":"mutate","inputSchema":{"type":"object"}},{"name":"read","inputSchema":{"type":"object"}}]}`
				case "tools/call":
					mutations++
					if mutations == 1 {
						if missing {
							w.WriteHeader(http.StatusNotFound)
							return
						}
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					result = `{"content":[{"type":"text","text":"fresh relationship"}]}`
				default:
					result = `{}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, input.ID, result)
			}))
			t.Cleanup(peer.Close)
			run, _ := newBrowserRunnerFixture(t, cli, peer.URL)
			require.Contains(t, run("alice", "mutate"), "state lost")
			mu.Lock()
			require.Equal(t, 1, mutations, "an ambiguous remote action is never replayed")
			firstSessions := sessions
			mu.Unlock()
			require.Contains(t, run("alice", "read"), "fresh relationship", "only subsequent deliberate work establishes new custody")
			mu.Lock()
			require.Greater(t, sessions, firstSessions)
			require.Equal(t, 2, mutations)
			mu.Unlock()
		})
	}
}

func TestRunnerCancellationDoesNotEndConcurrentCaller(t *testing.T) {
	for _, cli := range []bool{false, true} {
		t.Run(fmt.Sprintf("cli=%t", cli), func(t *testing.T) {
			started := make(chan struct{})
			var once sync.Once
			var mu sync.Mutex
			var sessions int
			authorities := []string{}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(405)
					return
				}
				var input struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if len(input.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				var result string
				switch input.Method {
				case "initialize":
					mu.Lock()
					sessions++
					w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", sessions))
					mu.Unlock()
					result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"concurrent","version":"1"}}`
				case "tools/list":
					result = `{"tools":[{"name":"read","inputSchema":{"type":"object"}}]}`
				case "tools/call":
					authority := r.Header.Get("Authorization")
					mu.Lock()
					authorities = append(authorities, authority)
					mu.Unlock()
					if authority == "Bearer alice" {
						once.Do(func() { close(started) })
						<-r.Context().Done()
						return
					}
					result = `{"content":[{"type":"text","text":"other caller remains active"}]}`
				default:
					result = `{}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, input.ID, result)
			}))
			t.Cleanup(peer.Close)
			run, _ := newScopedBrowserRunnerFixture(t, cli, peer.URL, true)
			callContext := func(token string) context.Context {
				ctx, _ := a2asrv.NewCallContext(t.Context(), a2asrv.NewServiceParams(map[string][]string{"Authorization": {"Bearer " + token}}))
				return ctx
			}
			alice, cancelAlice := context.WithCancel(callContext("alice"))
			defer cancelAlice()
			done := make(chan string, 1)
			go func() { done <- run(alice, "alice", "conversation", "read") }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("Alice never dispatched")
			}
			require.Contains(t, run(callContext("bob"), "bob", "conversation", "read"), "other caller remains active")
			cancelAlice()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("canceled runner did not return")
			}
			require.Contains(t, run(callContext("bob-fresh"), "bob", "conversation", "read"), "other caller remains active")
			mu.Lock()
			require.Equal(t, []string{"Bearer alice", "Bearer bob", "Bearer bob-fresh"}, authorities)
			mu.Unlock()
		})
	}
}
