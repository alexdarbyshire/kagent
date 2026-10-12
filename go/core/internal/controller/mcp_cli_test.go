package controller

import (
	"encoding/json"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/krt/krttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMCPCLIPreparationWithoutImageRegistration(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "mcp-cli", nil)
	h := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "go"}, Spec: v1alpha3.HarnessSpec{
		Kagent: &v1alpha3.KagentHarness{}, Workload: v1alpha3.HarnessWorkload{Image: "example.com/go@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}},
	}}
	a := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "agent"}, Spec: v1alpha3.AgentTemplateSpec{
		ModelConfig: &corev1.LocalObjectReference{Name: "model"}, Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: "browser"}, ExposeAsCLI: new(true)}}},
	}}
	mock := krttest.NewMock(t, []any{h, &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "model"}, Spec: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "fixture"}},
		&v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "browser"}, Spec: v1alpha3.RemoteMCPServerSpec{URL: "http://browser.example/mcp"}},
		&atev1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "default"}},
	})
	templates := krt.NewStaticCollection(nil, []*v1alpha3.AgentTemplate{a}, opts.WithName("AgentTemplates")...)
	agents := krt.NewStaticCollection(nil, []*v1alpha3.Agent{testAgent(a, h)}, opts.WithName("Agents")...)
	configMaps := krttest.GetMockCollection[*corev1.ConfigMap](mock)
	secrets := krttest.GetMockCollection[*corev1.Secret](mock)
	_, models := newModelConfigReconciliations(krttest.GetMockCollection[*v1alpha3.ModelConfig](mock), configMaps, secrets, opts)
	collections := v2translator.Collections{Harnesses: krttest.GetMockCollection[*v1alpha3.Harness](mock), AgentTemplates: templates, ResolvedModelConfigs: models,
		RemoteMCPServers: krttest.GetMockCollection[*v1alpha3.RemoteMCPServer](mock), ConfigMaps: configMaps, Secrets: secrets, WorkerPools: krttest.GetMockCollection[*atev1alpha1.WorkerPool](mock)}
	observations := krttest.GetMockCollection[AgentRuntimeObservation](mock)
	prepared := newAgentReconciliations(agents, collections, observations, opts)
	waitFor(t, func() bool { state := prepared.GetKey("test/agent"); return state != nil && state.Target != nil })
	var config adk.AgentConfig
	require.NoError(t, json.Unmarshal(prepared.GetKey("test/agent").Target.Revision.ConfigJSON, &config))
	require.Len(t, config.CLITools, 1)
	require.Empty(t, config.HttpTools)
	updated := a.DeepCopy()
	updated.Spec.Tools[0].MCP.RequireApproval = true
	templates.UpdateObject(updated)
	waitFor(t, func() bool {
		state := prepared.GetKey("test/agent")
		return state != nil && state.CompilationFailure != nil
	})
	state := prepared.GetKey("test/agent")
	require.Nil(t, state.Target)
	require.Equal(t, "UnsupportedConfiguration", state.CompilationFailure.Reason)
	require.Contains(t, state.CompilationFailure.Message, "requireApproval")
}
