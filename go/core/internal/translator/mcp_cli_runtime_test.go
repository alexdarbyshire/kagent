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
	"sync/atomic"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	runtimea2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestCompiledMCPCLIExecutesThroughA2AWithoutSkills(t *testing.T) {
	runCompiledMCPCLIFixture(t, false, false)
}

func TestCompiledMCPCLIExecutesWithSkillsAndNativeBinding(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) { runCompiledMCPCLIFixture(t, true, mixed) })
	}
}

func runCompiledMCPCLIFixture(t *testing.T, withSkills, mixed bool) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build := exec.Command("go", "build", "-o", binary, "../../../adk/cmd/mcp-cli")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	var calls atomic.Int32
	var nativeCalls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "record", Description: "FULL_TOOL_DESCRIPTION_ONLY_FOR_HELP", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"count":{"type":"integer"}},"required":["text","count"],"additionalProperties":false}`)}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		assert.JSONEq(t, `{"text":"hello","count":9007199254740993}`, string(req.Params.Arguments))
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "recorded"}}, StructuredContent: map[string]any{"count": int64(9007199254740993)}}, nil
	})
	server.AddTool(&mcpsdk.Tool{Name: "native_lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		nativeCalls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "native lookup complete"}}}, nil
	})
	mcpHTTP := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	defer mcpHTTP.Close()
	var turns atomic.Int32
	modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
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
		if turns.Add(1) == 1 {
			bashCount := 0
			nativeCount := 0
			for _, tool := range request.Tools {
				if tool.Function.Name == "bash" {
					bashCount++
				}
				if tool.Function.Name == "native_lookup" {
					nativeCount++
				}
				if strings.Contains(tool.Function.Name, "record") {
					t.Errorf("duplicate native MCP tool: %s", tool.Function.Name)
				}
			}
			if bashCount != 1 {
				t.Errorf("expected one Bash tool without skills, got %d", bashCount)
			}
			if mixed && nativeCount != 1 {
				t.Errorf("mixed configuration requires one native tool, got %d", nativeCount)
			}
			serialized, err := json.Marshal(request.Messages)
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(string(serialized), "browser") || !strings.Contains(string(serialized), "--help") {
				t.Errorf("missing CLI discovery: %s", serialized)
			}
			if strings.Contains(string(serialized), "FULL_TOOL_DESCRIPTION_ONLY_FOR_HELP") {
				t.Error("full MCP catalog eagerly included in discovery")
			}
			if bashCount == 0 {
				_, _ = fmt.Fprint(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","content":"missing bash"},"finish_reason":"stop"}]}`)
				return
			}
			nativeCall := ""
			if mixed {
				nativeCall = `,{"id":"native_call","type":"function","function":{"name":"native_lookup","arguments":"{}"}}`
			}
			_, _ = fmt.Fprintf(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"bash","arguments":"{\"command\":\"browser record --text hello --count 9007199254740993\"}"}}%s]},"finish_reason":"tool_calls"}]}`, nativeCall)
			return
		}
		found := false
		for _, message := range request.Messages {
			if message.Role == "tool" && strings.Contains(string(message.Content), "9007199254740993") {
				found = true
			}
		}
		if !found {
			t.Errorf("structured bridge result did not reach model: %#v", request.Messages)
		}
		_, _ = fmt.Fprint(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":"complete"},"finish_reason":"stop"}]}`)
	}))
	defer modelHTTP.Close()
	harness, template := cliConfiguration()
	template.Spec.Tools[0].MCP.Tools = []string{"record"}
	if mixed {
		template.Spec.Tools = append(template.Spec.Tools, v1alpha3.ToolBinding{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: "native"}, Tools: []string{"native_lookup"}}})
	}
	model := modelConfig()
	model.Spec.Stream = new(false)
	model.Spec.OpenAI = &v1alpha3.OpenAIConfig{BaseURL: modelHTTP.URL}
	result, err := cliCompiler(t, harness.Spec.Workload.Image, model, remoteMCPServer("browser", mcpHTTP.URL), remoteMCPServer("native", mcpHTTP.URL)).CompileAgent(t.Context(), inlineAgent(harness, template))
	require.NoError(t, err)
	var config adk.AgentConfig
	require.NoError(t, json.Unmarshal(result.ConfigJSON, &config))
	if withSkills {
		config.SkillsDirectory = t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(config.SkillsDirectory, "sample"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(config.SkillsDirectory, "sample", "SKILL.md"), []byte("---\nname: sample\ndescription: Sample skill\n---\nRun local commands.\n"), 0o600))
	}
	runtimeContext, cancelRuntime := context.WithCancel(t.Context())
	defer cancelRuntime()
	runnerConfig, err := runtimeRunner.CreateRunnerConfig(runtimeContext, &config, nil, "cli_runtime", nil, nil)
	require.NoError(t, err)
	executor, err := runtimea2a.NewKAgentExecutor(runtimea2a.KAgentExecutorConfig{RunnerConfig: runnerConfig, AppName: "cli_runtime", Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	contextID := "cli-context"
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("record hello"))
	message.ContextID = contextID
	completed := false
	for event, err := range executor.Execute(t.Context(), &a2asrv.ExecutorContext{TaskID: a2atype.NewTaskID(), ContextID: contextID, Message: message}) {
		require.NoError(t, err)
		if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && update.Status.State == a2atype.TaskStateCompleted {
			completed = true
		}
	}
	require.True(t, completed)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 2, turns.Load())
	if mixed {
		require.EqualValues(t, 1, nativeCalls.Load())
	} else {
		require.Zero(t, nativeCalls.Load())
	}
}
