package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestNativeCLICommandsKeepScopedSessionAndCloseOnActorShutdown(t *testing.T) {
	testRunnerScopedBrowser(t, true)
}

func TestNativeToolsKeepScopedSessionAndCloseOnActorShutdown(t *testing.T) {
	testRunnerScopedBrowser(t, false)
}

func testRunnerScopedBrowser(t *testing.T, cli bool) {
	t.Helper()
	var mu sync.Mutex
	owners := map[string]bool{}
	var terminated atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "browser", Version: "1"}, nil)
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
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			terminated.Add(1)
		}
		handler.ServeHTTP(w, request)
	}))
	defer remote.Close()
	run, cancel := newBrowserRunnerFixture(t, cli, remote.URL)
	require.Contains(t, run("alice", "create"), "owned browser tab")
	require.Contains(t, run("alice", "read"), "owned browser tab", "a new native invocation retains its connection")
	denied := "returned isError"
	if !cli {
		denied = "foreign browser identity"
	}
	require.Contains(t, run("bob", "read"), denied, "same Session ID under a different caller cannot read Alice's tab")
	require.Contains(t, run("bob", "close"), denied, "another caller cannot close Alice's tab")
	require.Contains(t, run("alice", "close"), "owned browser tab")
	mu.Lock()
	require.Empty(t, owners)
	mu.Unlock()
	cancel()
	require.Eventually(t, func() bool { return terminated.Load() >= 2 }, 5*time.Second, 10*time.Millisecond)
}

func newBrowserRunnerFixture(t *testing.T, cli bool, remoteURL string) (func(string, string) string, context.CancelFunc) {
	run, cancel := newScopedBrowserRunnerFixture(t, cli, remoteURL)
	return func(user, command string) string { return run(t.Context(), user, "conversation", command) }, cancel
}

func newScopedBrowserRunnerFixture(t *testing.T, cli bool, remoteURL string, forwardAuthority ...bool) (func(context.Context, string, string, string) string, context.CancelFunc) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	temporary, err := os.MkdirTemp("/tmp", "native-cli-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(temporary)) })
	t.Setenv("TMPDIR", temporary)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var input struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		last := input.Messages[len(input.Messages)-1]
		for _, message := range slices.Backward(input.Messages) {
			var text string
			if json.Unmarshal(message.Content, &text) != nil {
				continue
			}
			if message.Role == "tool" || (message.Role == "user" && (text == "create" || text == "read" || text == "close" || text == "mutate")) {
				last = message
				break
			}
		}
		var content string
		if err := json.Unmarshal(last.Content, &content); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if last.Role == "tool" {
			_, _ = fmt.Fprintf(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, content)
			return
		}
		toolName := content
		if cli {
			toolName = "bash"
		}
		arguments, err := json.Marshal(struct {
			Command string `json:"command"`
		}{Command: "browser " + content})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if !cli {
			arguments = []byte(`{}`)
		}
		_, _ = fmt.Fprintf(w, `{"id":"call","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"bash-call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, toolName, string(arguments))
	}))
	t.Cleanup(model.Close)
	owner, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	sessions := session.InMemoryService()
	agentConfig := &adk.AgentConfig{
		Model:    &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: model.URL},
		CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remoteURL}, Tools: []string{"create", "read", "close", "mutate"}}}},
	}
	if len(forwardAuthority) > 0 && forwardAuthority[0] {
		agentConfig.CLITools[0].HTTP.AllowedHeaders = []string{"Authorization"}
	}
	if !cli {
		agentConfig.HttpTools = []adk.HttpMcpServerConfig{agentConfig.CLITools[0].HTTP}
		agentConfig.CLITools = nil
	}
	config, err := runtimeRunner.CreateRunnerConfig(owner, agentConfig, sessions, "native-cli", nil, nil)
	require.NoError(t, err)
	config.Agent = &fixtureScopeAgent{Agent: config.Agent}
	runner, err := adkrunner.New(config)
	require.NoError(t, err)
	for _, user := range []string{"alice", "bob"} {
		_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "native-cli", UserID: user, SessionID: "conversation"})
		require.NoError(t, err)
	}
	_, err = sessions.Create(t.Context(), &session.CreateRequest{AppName: "native-cli", UserID: "alice", SessionID: "other-conversation"})
	require.NoError(t, err)
	run := func(ctx context.Context, user, conversation, command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		var answer strings.Builder
		for event, err := range runner.Run(ctx, user, conversation, genai.NewContentFromText(command, "user"), adkagent.RunConfig{}) {
			if err != nil {
				return "runner error: " + err.Error()
			}
			if event.Content != nil {
				for _, part := range event.Content.Parts {
					answer.WriteString(part.Text)
				}
			}
		}
		return answer.String()
	}
	return run, cancel
}

type fixtureScopeKey struct{}
type fixtureScope struct{ branch, isolation string }
type fixtureScopeAgent struct{ adkagent.Agent }

func (f *fixtureScopeAgent) Run(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	scope, ok := ctx.Value(fixtureScopeKey{}).(fixtureScope)
	if !ok {
		return f.Agent.Run(ctx)
	}
	ctx = ctx.WithICDelta(&adkagent.InvocationContextDelta{Branch: &scope.branch, IsolationScope: &scope.isolation})
	return func(yield func(*session.Event, error) bool) {
		for event, err := range f.Agent.Run(ctx) {
			if event != nil {
				event.IsolationScope = scope.isolation
			}
			if !yield(event, err) {
				return
			}
		}
	}
}
