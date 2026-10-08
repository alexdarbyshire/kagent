package translator_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	kagenttranslator "github.com/kagent-dev/kagent/go/core/internal/translator/kagent"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCompileAgentRejectsCLIWithoutRegisteredGoImage(t *testing.T) {
	var binding v1alpha3.MCPToolBinding
	require.NoError(t, json.Unmarshal([]byte(`{"server":{"kind":"RemoteMCPServer","name":"browser"},"exposeAsCLI":true}`), &binding))
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "go", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
		Kagent: &v1alpha3.KagentHarness{}, Workload: v1alpha3.HarnessWorkload{Image: "example.com/go@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}},
	}}
	template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "test"}, Spec: v1alpha3.AgentTemplateSpec{
		ModelConfig: &corev1.LocalObjectReference{Name: "default-model"}, Tools: []v1alpha3.ToolBinding{{MCP: &binding}},
	}}
	_, err := compiler(t, modelConfig(), remoteMCPServer("browser", "http://browser.example/mcp")).CompileAgent(t.Context(), inlineAgent(harness, template))
	require.ErrorContains(t, err, "KAGENT_MCP_CLI_GO_IMAGES")
}

func TestCompileAgentCLIReplacesNativeAndRetainsEgress(t *testing.T) {
	harness, template := cliConfiguration()
	server := remoteMCPServer("browser", "http://browser.example:8080/gateway/mcp")
	server.Spec.Description = "Browse documents"
	result, err := cliCompiler(t, harness.Spec.Workload.Image, modelConfig(), server).CompileAgent(t.Context(), inlineAgent(harness, template))
	require.NoError(t, err)
	var runtime struct {
		CLI []struct {
			Name string                  `json:"name"`
			HTTP adk.HttpMcpServerConfig `json:"http"`
		} `json:"cli_tools"`
		HTTP []adk.HttpMcpServerConfig `json:"http_tools"`
	}
	require.NoError(t, json.Unmarshal(result.ConfigJSON, &runtime))
	require.Len(t, runtime.CLI, 1)
	require.Empty(t, runtime.HTTP)
	require.Equal(t, "browser", runtime.CLI[0].Name)
	require.Equal(t, server.Spec.URL, runtime.CLI[0].HTTP.Params.Url)
	require.Equal(t, []string{"navigate"}, runtime.CLI[0].HTTP.Tools)
	require.Contains(t, result.EgressDestinations, "http://browser.example:8080")
	require.Equal(t, harness.Spec.Workload.Args, result.Args)
}

func TestCompileAgentCLISSEAndInvocationAuthentication(t *testing.T) {
	harness, template := cliConfiguration()
	harness.Spec.Env = []v1alpha3.RuntimeEnvVar{{Name: "KAGENT_PROPAGATE_TOKEN", Value: "true"}, {Name: "KAGENT_STS_WELL_KNOWN_URI", Value: "https://sts.example/.well-known/oauth-authorization-server"}}
	server := remoteMCPServer("browser", "http://browser.example:8080/gateway/sse")
	server.Spec.Protocol = v1alpha3.RemoteMCPServerProtocolSse
	server.Spec.Timeout = &metav1.Duration{Duration: 3 * time.Second}
	server.Spec.SseReadTimeout = &metav1.Duration{Duration: 10 * time.Second}
	result, err := cliCompiler(t, harness.Spec.Workload.Image, modelConfig(), server).CompileAgent(t.Context(), inlineAgent(harness, template))
	require.NoError(t, err)
	var config map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(result.ConfigJSON, &config))
	var bindings []struct {
		HTTP adk.HttpMcpServerConfig `json:"http"`
		SSE  *adk.SseMcpServerConfig `json:"sse"`
	}
	require.NoError(t, json.Unmarshal(config["cli_tools"], &bindings))
	require.Len(t, bindings, 1)
	require.Empty(t, bindings[0].HTTP.Params.Url)
	require.NotNil(t, bindings[0].SSE)
	require.Equal(t, server.Spec.URL, bindings[0].SSE.Params.Url)
	require.Equal(t, 3.0, *bindings[0].SSE.Params.Timeout)
	require.Equal(t, 10.0, *bindings[0].SSE.Params.SseReadTimeout)
	require.Equal(t, []string{"navigate"}, bindings[0].SSE.Tools)
	require.Empty(t, config["sse_tools"])
	require.Contains(t, result.EgressDestinations, "http://browser.example:8080")
}

func cliConfiguration() (*v1alpha3.Harness, *v1alpha3.AgentTemplate) {
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "go", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
		Kagent: &v1alpha3.KagentHarness{}, Workload: v1alpha3.HarnessWorkload{Image: "example.com/go@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Args: []string{"--log-level", "debug"}},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}},
	}}
	template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "test"}, Spec: v1alpha3.AgentTemplateSpec{
		ModelConfig: &corev1.LocalObjectReference{Name: "default-model"}, Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{
			Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: "browser"}, ExposeAsCLI: new(true), Tools: []string{"navigate"},
		}}},
	}}
	return harness, template
}

func cliCompiler(t *testing.T, image string, objects ...any) *v2translator.Compiler {
	t.Helper()
	collections := mockCollections(t, append(objects, defaultWorkerPool())...)
	ctx := krt.TestingDummyContext{}
	return v2translator.NewCompiler(ctx, collections, map[v2translator.HarnessType]v2translator.HarnessCompiler{
		v2translator.HarnessTypeKagent: kagenttranslator.NewCompiler(ctx, collections, []string{image}),
	})
}

func TestCompileAgentCLIRejectsUnsupportedAndCollidingBindings(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*v1alpha3.Harness, *v1alpha3.AgentTemplate, *v1alpha3.RemoteMCPServer)
		want   string
	}{
		{"protected", func(_ *v1alpha3.Harness, a *v1alpha3.AgentTemplate, _ *v1alpha3.RemoteMCPServer) {
			a.Spec.Tools[0].MCP.RequireApproval = true
		}, "requireApproval"},
		{"read timeout", func(_ *v1alpha3.Harness, _ *v1alpha3.AgentTemplate, s *v1alpha3.RemoteMCPServer) {
			s.Spec.SseReadTimeout = &metav1.Duration{Duration: 1}
		}, "sseReadTimeout"},
		{"SSE nonpositive read timeout", func(_ *v1alpha3.Harness, _ *v1alpha3.AgentTemplate, s *v1alpha3.RemoteMCPServer) {
			s.Spec.Protocol = v1alpha3.RemoteMCPServerProtocolSse
			s.Spec.SseReadTimeout = &metav1.Duration{}
		}, "positive sseReadTimeout"},
		{"termination", func(_ *v1alpha3.Harness, _ *v1alpha3.AgentTemplate, s *v1alpha3.RemoteMCPServer) {
			s.Spec.TerminateOnClose = new(false)
		}, "terminateOnClose"},
		{"entrypoint", func(h *v1alpha3.Harness, _ *v1alpha3.AgentTemplate, _ *v1alpha3.RemoteMCPServer) {
			h.Spec.Workload.Command = []string{"kagent-adk"}
		}, "entrypoint"},
		{"unpinned", func(h *v1alpha3.Harness, _ *v1alpha3.AgentTemplate, _ *v1alpha3.RemoteMCPServer) {
			h.Spec.Workload.Image = "example.com/go:latest"
		}, "digest-pinned"},
		{"duplicate", func(_ *v1alpha3.Harness, a *v1alpha3.AgentTemplate, _ *v1alpha3.RemoteMCPServer) {
			a.Spec.Tools = append(a.Spec.Tools, *a.Spec.Tools[0].DeepCopy())
		}, "collision"},
		{"builtin", func(_ *v1alpha3.Harness, a *v1alpha3.AgentTemplate, s *v1alpha3.RemoteMCPServer) {
			s.Name = "test"
			a.Spec.Tools[0].MCP.Server.Name = s.Name
		}, "reserved"},
		{"platform command", func(_ *v1alpha3.Harness, a *v1alpha3.AgentTemplate, s *v1alpha3.RemoteMCPServer) {
			s.Name = "awk"
			a.Spec.Tools[0].MCP.Server.Name = s.Name
		}, "reserved"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, a := cliConfiguration()
			s := remoteMCPServer("browser", "http://browser.example/mcp")
			tt.change(h, a, s)
			_, err := cliCompiler(t, h.Spec.Workload.Image, modelConfig(), s).CompileAgent(t.Context(), inlineAgent(h, a))
			require.ErrorContains(t, err, tt.want)
			var validation *v2translator.ValidationError
			require.ErrorAs(t, err, &validation)
		})
	}
}

func TestCompileAgentCLISharedAndReferencedConfiguration(t *testing.T) {
	for _, referenced := range []bool{false, true} {
		t.Run(fmt.Sprintf("referenced=%v", referenced), func(t *testing.T) {
			h, root := cliConfiguration()
			child := root.DeepCopy()
			child.Name = "child-template"
			root.Spec.Tools = append(root.Spec.Tools, v1alpha3.ToolBinding{SubAgent: &v1alpha3.SubAgentToolBinding{Name: "child", Description: "Delegate", TemplateRef: &corev1.LocalObjectReference{Name: child.Name}}})
			root.Spec.Tools = append(root.Spec.Tools, v1alpha3.ToolBinding{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: "native"}, RequireApproval: true}})
			s := remoteMCPServer("browser", "https://browser.example/mcp")
			native := remoteMCPServer("native", "https://native.example/mcp")
			publicAgent := inlineAgent(h, root)
			if referenced {
				publicAgent.Spec.Template = nil
				publicAgent.Spec.Harness = nil
				publicAgent.Spec.TemplateRef = &corev1.LocalObjectReference{Name: root.Name}
				publicAgent.Spec.HarnessRef = &corev1.LocalObjectReference{Name: h.Name}
			}
			compile := func() (*v2translator.CompileResult, error) {
				return cliCompiler(t, h.Spec.Workload.Image, modelConfig(), s, native, child, root, h).CompileAgent(t.Context(), publicAgent)
			}
			revision, err := compile()
			require.NoError(t, err)
			var config adk.AgentConfig
			require.NoError(t, json.Unmarshal(revision.ConfigJSON, &config))
			require.Len(t, config.CLITools, 1)
			require.Len(t, config.HttpTools, 1)
			require.True(t, config.HttpTools[0].RequireApproval, "native approval remains supported")
			require.Len(t, config.SubAgents, 1)
			require.Len(t, config.SubAgents[0].CLITools, 1, "same command names in Shared children are independent")
			require.Empty(t, config.SubAgents[0].HttpTools)
			child.Spec.Tools[0].MCP.RequireApproval = true
			_, err = compile()
			require.ErrorContains(t, err, "requireApproval")
		})
	}
}

func TestCompileAgentCLIFalseAndUnsupportedHarnesses(t *testing.T) {
	for _, exposure := range []*bool{nil, new(false)} {
		h, a := cliConfiguration()
		a.Spec.Tools[0].MCP.ExposeAsCLI = exposure
		result, err := compiler(t, modelConfig(), remoteMCPServer("browser", "https://browser.example/mcp")).CompileAgent(t.Context(), inlineAgent(h, a))
		require.NoError(t, err)
		var config adk.AgentConfig
		require.NoError(t, json.Unmarshal(result.ConfigJSON, &config))
		require.Empty(t, config.CLITools)
		require.Len(t, config.HttpTools, 1)
	}
	for _, runtime := range []string{"python", "codex", "claude", "byo"} {
		t.Run(runtime, func(t *testing.T) {
			h, a := cliConfiguration()
			switch runtime {
			case "python":
				h.Spec.Workload.Command = []string{"kagent-adk"}
			case "codex":
				h.Spec.Kagent = nil
				h.Spec.Codex = &v1alpha3.CodexHarness{}
			case "claude":
				h.Spec.Kagent = nil
				h.Spec.Claude = &v1alpha3.ClaudeHarness{}
			case "byo":
				h.Spec.Kagent = nil
				h.Spec.BYO = &v1alpha3.BYOHarness{}
			}
			_, err := compiler(t, modelConfig(), remoteMCPServer("browser", "https://browser.example/mcp")).CompileAgent(t.Context(), inlineAgent(h, a))
			require.Error(t, err)
			var validation *v2translator.ValidationError
			require.ErrorAs(t, err, &validation)
		})
	}
}
