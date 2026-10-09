package mcp_test

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

	"github.com/kagent-dev/kagent/go/adk/pkg/models"
	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestStandaloneCommandReportsRefusedTerminationOnStderr(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "standalone", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "command result"}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	file := filepath.Join(t.TempDir(), "binding.json")
	binding, err := json.Marshal(adk.MCPCLIConfig{Name: "standalone", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL, Headers: map[string]string{"Authorization": "Bearer static"}}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, binding, 0600))
	command := exec.Command(binary, "--binding-file", file, "read")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Run(), "termination evidence must not rewrite the successful command result")
	require.True(t, json.Valid(stdout.Bytes()), "stdout must remain the raw command envelope")
	require.Contains(t, stdout.String(), "command result")
	require.Contains(t, stderr.String(), "auth_refused")
	require.NotContains(t, stderr.String(), "Bearer static")
}

func TestRunnerProviderAuthorityAuthenticatesOwnedShutdown(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "true")
	for _, exchange := range []bool{false, true} {
		for _, command := range []bool{false, true} {
			name := map[bool]string{false: "propagate", true: "exchange"}[exchange] + "/" + map[bool]string{false: "native", true: "command"}[command]
			t.Run(name, func(t *testing.T) {
				var exchanges atomic.Int32
				var authenticatedStreams atomic.Int32
				expected := "Bearer caller"
				if exchange {
					expected = "Bearer delegated"
					var issuer *httptest.Server
					issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.URL.Path == "/token" {
							exchanges.Add(1)
							_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "delegated", "token_type": "Bearer", "expires_in": 3600})
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "token_endpoint": issuer.URL + "/token"})
					}))
					defer issuer.Close()
					t.Setenv("KAGENT_STS_WELL_KNOWN_URI", issuer.URL+"/.well-known/oauth-authorization-server")
				} else {
					t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
				}
				var mu sync.Mutex
				var calledSession, terminatedSession, deleteAuthority string
				var callAuthority string
				var callSessions []string
				server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "owned", Version: "1"}, nil)
				server.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					mu.Lock()
					calledSession = r.Session.ID()
					callSessions = append(callSessions, calledSession)
					mu.Unlock()
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "authenticated action"}}}, nil
				})
				handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						if r.Header.Get("Authorization") == expected {
							authenticatedStreams.Add(1)
						}
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					var method string
					if r.Method == http.MethodPost {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if err := r.Body.Close(); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var message struct {
							Method string `json:"method"`
						}
						if err := json.Unmarshal(body, &message); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						method = message.Method
					}
					mu.Lock()
					if r.Method == http.MethodDelete && calledSession != "" && r.Header.Get("Mcp-Session-Id") == calledSession {
						terminatedSession, deleteAuthority = r.Header.Get("Mcp-Session-Id"), r.Header.Get("Authorization")
						if deleteAuthority != expected {
							mu.Unlock()
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
					}
					// Discovery may run before an invocation; actual calls must carry current authority.
					if method == "tools/call" {
						callAuthority = r.Header.Get("Authorization")
						if callAuthority != expected {
							mu.Unlock()
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
					}
					mu.Unlock()
					handler.ServeHTTP(w, r)
				}))
				defer remote.Close()
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var input struct {
						Messages []struct {
							Role string `json:"role"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if input.Messages[len(input.Messages)-1].Role == "tool" {
						_, _ = fmt.Fprint(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":"finished"},"finish_reason":"stop"}]}`)
						return
					}
					toolName, arguments := "read", "{}"
					if command {
						toolName, arguments = "bash", `{"command":"owned read"}`
					}
					_, _ = fmt.Fprintf(w, `{"id":"call","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"owned-call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, toolName, arguments)
				}))
				defer model.Close()
				host, stop := context.WithCancel(t.Context())
				defer stop()
				sessions := session.InMemoryService()
				agentConfig := &adk.AgentConfig{Model: &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: model.URL}}
				binding := adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"read"}}
				if command {
					agentConfig.CLITools = []adk.MCPCLIConfig{{Name: "owned", HTTP: binding}}
				} else {
					agentConfig.HttpTools = []adk.HttpMcpServerConfig{binding}
				}
				config, err := runtimeRunner.CreateRunnerConfig(host, agentConfig, sessions, "provider-shutdown", nil, nil)
				require.NoError(t, err)
				runner, err := adkrunner.New(config)
				require.NoError(t, err)
				_, err = sessions.Create(t.Context(), &session.CreateRequest{AppName: "provider-shutdown", UserID: "alice", SessionID: "conversation"})
				require.NoError(t, err)
				for range 2 {
					invocation, end := context.WithCancel(context.WithValue(t.Context(), models.BearerTokenKey, "caller"))
					var text strings.Builder
					for event, err := range runner.Run(invocation, "alice", "conversation", genai.NewContentFromText("read", "user"), adkagent.RunConfig{}) {
						require.NoError(t, err)
						if event.Content != nil {
							for _, part := range event.Content.Parts {
								text.WriteString(part.Text)
							}
						}
					}
					require.Contains(t, text.String(), "finished")
					end()
				}
				mu.Lock()
				require.Len(t, callSessions, 2)
				require.Equal(t, callSessions[0], callSessions[1], "fresh same-subject invocations retain provider-scoped custody")
				mu.Unlock()
				mu.Lock()
				called, auth, terminated := calledSession, callAuthority, terminatedSession
				mu.Unlock()
				require.NotEmpty(t, called)
				require.Equal(t, expected, auth)
				require.Empty(t, terminated, "invocation end must preserve the host-owned relationship")
				stop()
				require.Eventually(t, func() bool {
					mu.Lock()
					defer mu.Unlock()
					return terminatedSession == calledSession && deleteAuthority == expected
				}, 5*time.Second, 10*time.Millisecond, "provider authority must authenticate owned DELETE")
				wantExchanges := int32(0)
				if exchange {
					wantExchanges = 1
				}
				require.Equal(t, wantExchanges, exchanges.Load(), "cleanup cannot exchange fresh authority")
				if !command {
					require.Positive(t, authenticatedStreams.Load(), "provider custody preserves authenticated native standalone streams")
				}
			})
		}
	}
}
