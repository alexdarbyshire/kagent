package translator_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	runtimea2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCLIRuntimeScopesRootAndSharedCommands(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build := exec.Command("go", "build", "-o", binary, "../../../adk/cmd/mcp-cli")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	originalPATH := os.Getenv("PATH")
	var rootCalls, childCalls atomic.Int32
	serveMCP := func(scope string, count *atomic.Int32) *httptest.Server {
		s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: scope, Version: "1"}, nil)
		for _, name := range []string{"root_record", "child_record"} {
			s.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				count.Add(1)
				assert.JSONEq(t, fmt.Sprintf(`{"text":%q}`, scope), string(req.Params.Arguments))
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: scope}}}, nil
			})
		}
		return httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return s }, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	}
	rootMCP := serveMCP("root", &rootCalls)
	defer rootMCP.Close()
	childMCP := serveMCP("child", &childCalls)
	defer childMCP.Close()
	var rootTurns, childTurns atomic.Int32
	var pathsMu sync.Mutex
	paths := map[string]string{}
	modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		scope := req.Header.Get("X-Scope")
		counter := &rootTurns
		if scope == "child" {
			counter = &childTurns
		}
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if counter.Add(1) == 1 {
			args, _ := json.Marshal(struct {
				Command string `json:"command"`
			}{"command -v browser; browser " + scope + "_record --text " + scope})
			_, _ = fmt.Fprintf(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"bash","arguments":%q}}]},"finish_reason":"tool_calls"}]}`, string(args))
			return
		}
		for _, message := range request.Messages {
			if message.Role != "tool" {
				continue
			}
			var content string
			if err := json.Unmarshal(message.Content, &content); err != nil {
				t.Error(err)
				continue
			}
			path, _, _ := strings.Cut(content, "\n")
			pathsMu.Lock()
			paths[scope] = path
			pathsMu.Unlock()
		}
		_, _ = fmt.Fprint(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":"complete"},"finish_reason":"stop"}]}`)
	}))
	defer modelHTTP.Close()
	configFor := func(scope, endpoint string) *adk.AgentConfig {
		return &adk.AgentConfig{Name: scope, Model: &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture", Headers: map[string]string{"X-Scope": scope}}, BaseUrl: modelHTTP.URL}, CLITools: []adk.MCPCLIConfig{{Name: "browser", Description: "Scoped recording", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: endpoint}, Tools: []string{scope + "_record"}}}}}
	}
	root := configFor("root", rootMCP.URL)
	root.SubAgents = []*adk.AgentConfig{configFor("child", childMCP.URL)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runnerConfig, err := runtimeRunner.CreateRunnerConfig(ctx, root, nil, "scoped", nil, nil)
	require.NoError(t, err)
	require.Equal(t, originalPATH, os.Getenv("PATH"), "materialization must not mutate process PATH")
	for _, scope := range []string{"root", "child"} {
		selected := runnerConfig
		if scope == "child" {
			selected.Agent = runnerConfig.Agent.SubAgents()[0]
		}
		executor, err := runtimea2a.NewKAgentExecutor(runtimea2a.KAgentExecutorConfig{RunnerConfig: selected, AppName: "scoped", Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, err)
		message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("record "+scope))
		message.ContextID = scope + "-session"
		completed := false
		for event, err := range executor.Execute(ctx, &a2asrv.ExecutorContext{TaskID: a2atype.NewTaskID(), ContextID: message.ContextID, Message: message}) {
			require.NoError(t, err)
			if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && update.Status.State == a2atype.TaskStateCompleted {
				completed = true
			}
		}
		require.True(t, completed)
	}
	pathsMu.Lock()
	rootPath, childPath := paths["root"], paths["child"]
	pathsMu.Unlock()
	require.NotEmpty(t, rootPath)
	require.NotEmpty(t, childPath)
	require.NotEqual(t, filepath.Dir(rootPath), filepath.Dir(childPath))
	for _, scope := range []string{"root", "child"} {
		path, other := rootPath, "child_record"
		if scope == "child" {
			path, other = childPath, "root_record"
		}
		output, err := exec.Command(path, "--help").CombinedOutput()
		require.NoError(t, err, "%s", output)
		require.Contains(t, string(output), scope+"_record")
		require.NotContains(t, string(output), other)
		output, err = exec.Command(path, other, "--text", scope).CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(output), "excluded tool")
		output, err = exec.Command(path, scope+"_record", "--text", scope).CombinedOutput()
		require.NoError(t, err, "%s", output)
		require.True(t, json.Valid(output), "launcher stdout must be JSON")
	}
	require.EqualValues(t, 2, rootCalls.Load())
	require.EqualValues(t, 2, childCalls.Load())
	require.Equal(t, originalPATH, os.Getenv("PATH"))
	privateRoot := filepath.Dir(filepath.Dir(filepath.Dir(rootPath)))
	cancel()
	require.Eventually(t, func() bool { _, err := os.Stat(privateRoot); return os.IsNotExist(err) }, time.Second, 10*time.Millisecond, "runtime cancellation must remove private command tree")
}
