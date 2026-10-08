package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/mockllm"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// CI registers the exact digest of its built Go image in the controller's
// KAGENT_MCP_CLI_GO_IMAGES before installation. This test never silently skips
// missing registration or a bridge-free runtime image.
func TestMCPCLIInteraction(t *testing.T) {
	t.Parallel()
	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	kube := interactionKubeClient(t)
	mcpURL, recording := startMCPMock(t)
	server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-mcp-", Namespace: "kagent"}, Spec: v1alpha3.RemoteMCPServerSpec{Description: "Add numbers through MCP CLI", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp, URL: mcpURL}}
	require.NoError(t, kube.Create(t.Context(), server))
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), server); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete MCP CLI server: %v", err)
		}
	})
	config, err := mockllm.LoadConfigFromFile("mocks/invoke_mcp_agent.json", interactionMocks)
	require.NoError(t, err)
	prompt := "Add 3 and 5 using CLI."
	require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":"Add 3 and 5 using CLI."}`), &config.OpenAI[0].Match.Message))
	call := &config.OpenAI[0].Response.Choices[0].Message.ToolCalls[0]
	call.Function.Name = "bash"
	arguments, err := json.Marshal(struct {
		Command string `json:"command"`
	}{server.Name + " --help; " + server.Name + " add_numbers --a 3 --b 5"})
	require.NoError(t, err)
	call.Function.Arguments = string(arguments)
	model := harness.createModel(t, kube, reachableModelURL(t, startMockLLMConfig(t, config)), nil)
	template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-agent-", Namespace: "kagent", Labels: harness.labels()}, Spec: v1alpha3.AgentTemplateSpec{
		ModelConfig: &corev1.LocalObjectReference{Name: model.Name}, SystemPrompt: "Use the configured CLI command to add numbers. Discover its tool arguments with help when needed.",
		Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"add_numbers"}, ExposeAsCLI: new(true)}}},
	}}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harness.name)
	fixture := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	_, _, task := fixture.send(t, prompt)
	require.Equal(t, a2atype.TaskStateCompleted, task.Status.State)
	require.Contains(t, taskText(task), "result is 8")
	calls := 0
	for _, request := range recording.Requests() {
		if bytes.Contains(request.Body, []byte(`"method":"tools/call"`)) && bytes.Contains(request.Body, []byte(`"name":"add_numbers"`)) {
			calls++
			require.JSONEq(t, `{"a":3,"b":5}`, mcpToolCallArguments(t, request.Body))
		}
	}
	require.Equal(t, 1, calls, "CLI help must not invoke tools and calls must not be replayed")
	// A protected CLI edit must stop preparation rather than silently dropping
	// the approval policy or retaining a new runnable target.
	template.Spec.Tools[0].MCP.RequireApproval = true
	require.NoError(t, kube.Update(t.Context(), template))
	agent := &v1alpha3.Agent{}
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := kube.Get(ctx, ctrlclient.ObjectKey{Namespace: template.Namespace, Name: template.Name}, agent); err != nil {
			return false, err
		}
		condition := apimeta.FindStatusCondition(agent.Status.Conditions, v1alpha3.AgentConditionCompatible)
		return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "UnsupportedConfiguration", nil
	}))
	require.Empty(t, agent.Status.DesiredRevision)
	require.Contains(t, apimeta.FindStatusCondition(agent.Status.Conditions, v1alpha3.AgentConditionCompatible).Message, "requireApproval")
}

func mcpToolCallArguments(t *testing.T, body []byte) string {
	t.Helper()
	var request struct {
		Params struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(body, &request))
	return string(request.Params.Arguments)
}
