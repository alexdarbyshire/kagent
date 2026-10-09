package translator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	runtimea2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCLIRunnerA2ACallerAndSTSIsolationAcrossSharedAgents(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../../adk/cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "true")
	for _, exchange := range []bool{false, true} {
		t.Run(fmt.Sprintf("STS=%v", exchange), func(t *testing.T) {
			var exchanges atomic.Int32
			var stsHTTP *httptest.Server
			stsHTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/.well-known/oauth-authorization-server" {
					_, _ = fmt.Fprintf(w, `{"issuer":%q,"token_endpoint":%q}`, stsHTTP.URL, stsHTTP.URL+"/token")
					return
				}
				if req.URL.Path != "/token" || req.Method != http.MethodPost {
					http.NotFound(w, req)
					return
				}
				exchanges.Add(1)
				subject := req.FormValue("subject_token")
				assert.Contains(t, []string{"incoming-alice", "incoming-bob"}, subject)
				_, _ = fmt.Fprintf(w, `{"access_token":%q,"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":300}`, "exchanged-"+strings.TrimPrefix(subject, "incoming-"))
			}))
			defer stsHTTP.Close()
			stsURL := ""
			if exchange {
				stsURL = stsHTTP.URL + "/.well-known/oauth-authorization-server"
			}
			t.Setenv("KAGENT_STS_WELL_KNOWN_URI", stsURL)
			type recordedCall struct{ scope, name, user, authorization, allowed, order string }
			var mu sync.Mutex
			var recorded []recordedCall
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "auth", Version: "1"}, nil)
			for _, name := range []string{"cli_record", "native_record"} {
				server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: req.Params.Name + " complete"}}}, nil
				})
			}
			handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					data, err := io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
						return
					}
					req.Body = io.NopCloser(bytes.NewReader(data))
					var call struct {
						Method string `json:"method"`
						Params struct {
							Name string `json:"name"`
						} `json:"params"`
					}
					if err := json.Unmarshal(data, &call); err != nil {
						t.Error(err)
						return
					}
					if call.Method == "tools/call" {
						mu.Lock()
						recorded = append(recorded, recordedCall{scope: strings.TrimPrefix(req.URL.Path, "/"), name: call.Params.Name, user: req.Header.Get("X-User-Id"), authorization: req.Header.Get("Authorization"), allowed: req.Header.Get("X-Allowed"), order: req.Header.Get("X-Order")})
						mu.Unlock()
					}
				}
				handler.ServeHTTP(w, req)
			}))
			defer remote.Close()
			modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var request struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				toolResults := 0
				for _, message := range request.Messages {
					if message.Role == "tool" {
						toolResults++
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if toolResults == 0 {
					_, _ = fmt.Fprint(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"cli","type":"function","function":{"name":"bash","arguments":"{\"command\":\"browser cli_record\"}"}},{"id":"native","type":"function","function":{"name":"native_record","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
					return
				}
				assert.Equal(t, 2, toolResults)
				var results strings.Builder
				for _, message := range request.Messages {
					if message.Role == "tool" {
						results.Write(message.Content)
					}
				}
				assert.Contains(t, results.String(), "cli_record complete")
				assert.Contains(t, results.String(), "native_record complete")
				_, _ = fmt.Fprint(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":"complete"},"finish_reason":"stop"}]}`)
			}))
			defer modelHTTP.Close()
			configFor := func(scope string) *adk.AgentConfig {
				connection := adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL + "/" + scope, Headers: map[string]string{"X-Order": "static"}}, AllowedHeaders: []string{"X-Allowed", "X-Order"}}
				cli := connection
				cli.Tools = []string{"cli_record"}
				connection.Tools = []string{"native_record"}
				return &adk.AgentConfig{Name: scope, Model: &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: modelHTTP.URL}, HttpTools: []adk.HttpMcpServerConfig{connection}, CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: cli}}}
			}
			root := configFor("root")
			root.SubAgents = []*adk.AgentConfig{configFor("child")}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runnerConfig, err := runtimeRunner.CreateRunnerConfig(ctx, root, nil, "authentication", nil, nil)
			require.NoError(t, err)
			var wg sync.WaitGroup
			for _, scope := range []string{"root", "child"} {
				selected := runnerConfig
				if scope == "child" {
					selected.Agent = runnerConfig.Agent.SubAgents()[0]
				}
				executor, err := runtimea2a.NewKAgentExecutor(runtimea2a.KAgentExecutorConfig{RunnerConfig: selected, AppName: "authentication", Logger: slog.New(slog.DiscardHandler)})
				require.NoError(t, err)
				for _, user := range []string{"alice", "bob"} {
					wg.Go(func() {
						invocation, call := a2asrv.NewCallContext(ctx, a2asrv.NewServiceParams(map[string][]string{"Authorization": {"Bearer incoming-" + user}, "X-Allowed": {scope + "-" + user}, "X-Order": {"incoming"}}))
						call.User = a2asrv.NewAuthenticatedUser(user, nil)
						message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("record "+user))
						message.ContextID = scope + "-same-session"
						completed := false
						for event, err := range executor.Execute(invocation, &a2asrv.ExecutorContext{TaskID: a2atype.NewTaskID(), ContextID: message.ContextID, Message: message}) {
							if !assert.NoError(t, err) {
								return
							}
							if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && update.Status.State == a2atype.TaskStateCompleted {
								completed = true
							}
						}
						assert.True(t, completed)
					})
				}
			}
			wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, recorded, 8)
			seen := map[string]bool{}
			for _, call := range recorded {
				key := call.scope + "/" + call.user + "/" + call.name
				require.False(t, seen[key], "a tool call must not replay")
				seen[key] = true
				require.Contains(t, []string{"alice", "bob"}, call.user)
				prefix := "incoming-"
				if exchange {
					prefix = "exchanged-"
				}
				// Both presentations preserve the original ADK context, so
				// provider authority overrides propagated caller credentials.
				require.Equal(t, "Bearer "+prefix+call.user, call.authorization, key)
				require.Equal(t, call.scope+"-"+call.user, call.allowed)
				require.Equal(t, "static", call.order)
			}
			if exchange {
				require.EqualValues(t, 4, exchanges.Load())
			} else {
				require.Zero(t, exchanges.Load())
			}
		})
	}
}
