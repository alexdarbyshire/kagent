package mcp_test

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
		arguments, err := json.Marshal(struct {
			Command string `json:"command"`
		}{Command: "browser " + content})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"call","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"bash-call","type":"function","function":{"name":"bash","arguments":%q}}]},"finish_reason":"tool_calls"}]}`, string(arguments))
	}))
	defer model.Close()
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	sessions := session.InMemoryService()
	config, err := runtimeRunner.CreateRunnerConfig(owner, &adk.AgentConfig{
		Model:    &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: model.URL},
		CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"create", "read", "close"}}}},
	}, sessions, "native-cli", nil, nil)
	require.NoError(t, err)
	runner, err := adkrunner.New(config)
	require.NoError(t, err)
	for _, user := range []string{"alice", "bob"} {
		_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "native-cli", UserID: user, SessionID: "conversation"})
		require.NoError(t, err)
	}
	run := func(user, command string) string {
		t.Helper()
		var answer strings.Builder
		for event, err := range runner.Run(t.Context(), user, "conversation", genai.NewContentFromText(command, "user"), adkagent.RunConfig{}) {
			require.NoError(t, err)
			if event.Content != nil {
				for _, part := range event.Content.Parts {
					answer.WriteString(part.Text)
				}
			}
		}
		return answer.String()
	}
	require.Contains(t, run("alice", "create"), "owned browser tab")
	require.Contains(t, run("alice", "read"), "owned browser tab", "a new native invocation retains its connection")
	require.Contains(t, run("bob", "read"), "returned isError", "same Session ID under a different caller cannot read Alice's tab")
	require.Contains(t, run("bob", "close"), "returned isError", "another caller cannot close Alice's tab")
	require.Contains(t, run("alice", "close"), "owned browser tab")
	mu.Lock()
	require.Empty(t, owners)
	mu.Unlock()
	cancel()
	require.Eventually(t, func() bool { return terminated.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
}
