package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Data restore starts fresh containers under the same Actor identity. The
// private command path proves rematerialization; /data proves durable restore.
func TestMCPCLIRevisionLifecycle(t *testing.T) {
	kube := interactionKubeClient(t)
	harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
	var calls atomic.Int32
	mcp := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "lifecycle", Version: "1"}, nil)
	recordCall := func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "called:" + request.Params.Name}}}, nil
	}
	for _, name := range []string{"old_tool", "new_tool"} {
		mcp.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, recordCall)
	}
	mcpHTTP := startMCPCLIHTTPServer(t, mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcp }, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-lifecycle-", Namespace: "kagent"}, Spec: v1alpha3.RemoteMCPServerSpec{Description: "Lifecycle command", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp, URL: reachableServerURL(t, mcpHTTP.URL, "/mcp")}}
	require.NoError(t, kube.Create(t.Context(), server))
	t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), server)) })
	command := server.Name
	commands := map[string]string{
		"old":      command + " --help; command -v " + command + "; " + command + " old_tool",
		"capture":  "command -v " + command + " > /data/cli-path; printf durable > /data/cli-marker; " + command + " old_tool",
		"restored": "test \"$(command -v " + command + ")\" != \"$(cat /data/cli-path)\" && test ! -e \"$(cat /data/cli-path)\" && test \"$(cat /data/cli-marker)\" = durable && echo restored && " + command + " --help && " + command + " old_tool",
		"new":      command + " --help && " + command + " new_tool && if " + command + " old_tool; then echo unexpected-old-call; else echo excluded-old-tool; fi",
		"removed":  "if command -v " + command + "; then echo stale-command; else echo absent-command; fi",
	}
	modelHTTP := startMCPCLIHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "decode", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		last := request.Messages[len(request.Messages)-1]
		var content string
		if err := json.Unmarshal(last.Content, &content); err != nil {
			t.Error(err)
			http.Error(w, "content", 500)
			return
		}
		if last.Role == "tool" {
			encoded, err := json.Marshal(content)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"final","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, encoded)
			return
		}
		if content == "native" || content == "empty" {
			for _, message := range request.Messages {
				if strings.Contains(string(message.Content), "MCP commands available through bash") {
					t.Error("revision without CLI bindings retained command discovery")
				}
			}
		}
		if content == "empty" {
			for _, tool := range request.Tools {
				if tool.Function.Name == "bash" || tool.Function.Name == "new_tool" {
					t.Errorf("removed binding retained tool %q", tool.Function.Name)
				}
			}
			_, _ = fmt.Fprint(w, `{"id":"empty","choices":[{"index":0,"message":{"role":"assistant","content":"bindings-removed"},"finish_reason":"stop"}]}`)
			return
		}
		name, arguments := "bash", ""
		if content == "native" {
			for _, tool := range request.Tools {
				if tool.Function.Name == "bash" {
					t.Error("native-only revision retained CLI execution tools")
				}
			}
			name, arguments = "new_tool", "{}"
		} else {
			bash := 0
			for _, tool := range request.Tools {
				if tool.Function.Name == "bash" {
					bash++
				}
			}
			if bash != 1 {
				t.Errorf("expected one Bash registration, got %d", bash)
			}
			script, ok := commands[content]
			if renamed, found := strings.CutPrefix(content, "renamed:"); found {
				script, ok = renamed+" new_tool", true
			}
			if !ok {
				t.Errorf("unknown lifecycle prompt %q", content)
				http.Error(w, "prompt", 500)
				return
			}
			encoded, err := json.Marshal(struct {
				Command string `json:"command"`
			}{script})
			if err != nil {
				t.Error(err)
				return
			}
			arguments = string(encoded)
		}
		_, _ = fmt.Fprintf(w, `{"id":"call","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"cli","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, name, arguments)
	}))
	model := harness.createModel(t, kube, reachableModelURL(t, modelHTTP.URL), nil)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(t.Context(), ctrlclient.ObjectKeyFromObject(model), model); err != nil {
			return err
		}
		model.Spec.Stream = new(false)
		return kube.Update(t.Context(), model)
	}))
	template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-lifecycle-agent-", Namespace: "kagent", Labels: harness.labels()}, Spec: v1alpha3.AgentTemplateSpec{ModelConfig: &corev1.LocalObjectReference{Name: model.Name}, SystemPrompt: "Use the requested tool.", Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"old_tool"}, ExposeAsCLI: new(true)}}}}}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harness.name)
	old := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	send := func(fixture *interactionFixture, prompt string) string {
		t.Helper()
		_, _, task := fixture.send(t, prompt)
		require.Equal(t, a2atype.TaskStateCompleted, task.Status.State, "%s", taskText(task))
		return taskText(task)
	}
	require.Contains(t, send(old, "capture"), "called:old_tool")
	assertActorSuspended(t, old)
	before, err := old.sessions.GetSession(old.ctx, &apiv1alpha1.GetSessionRequest{SessionId: old.sessionID})
	require.NoError(t, err)
	actor, err := findSubstrateActor(old.ctx, old.system, "", substrate.ActorName(old.sessionID))
	require.NoError(t, err)
	require.NotNil(t, actor)
	uid := actor.GetMetadata().GetUid()
	previousRevision := before.GetSession().GetPreparedRevision()
	update := func() string {
		t.Helper()
		require.NoError(t, kube.Update(t.Context(), template))
		var revision string
		require.NoError(t, wait.PollUntilContextTimeout(t.Context(), time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			current := &v1alpha3.Agent{}
			if err := kube.Get(ctx, ctrlclient.ObjectKeyFromObject(template), current); err != nil {
				return false, err
			}
			revision = current.Status.LatestSuccessfulRevision
			return revision != "" && revision != previousRevision && revision == current.Status.DesiredRevision, nil
		}))
		previousRevision = revision
		return revision
	}
	template.Spec.Tools[0].MCP.Tools = []string{"new_tool"}
	update()
	next := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	newResult := send(next, "new")
	require.Contains(t, newResult, "called:new_tool")
	require.Contains(t, newResult, "excluded-old-tool")
	require.NotContains(t, newResult, "unexpected-old-call")
	require.EqualValues(t, 2, calls.Load(), "rejected selection must not invoke the old tool")
	// Catalogs remain remote and live even when a Session's binding is pinned.
	mcp.AddTool(&mcpsdk.Tool{Name: "old_tool", Description: "LIVE_CATALOG_CHANGED", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, recordCall)
	// The Actor's automatic Data suspension can precede durable settlement.
	// Retry only the public precondition while that idle operation completes.
	require.NoError(t, wait.PollUntilContextTimeout(old.ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		response, err := old.sessions.SuspendSession(ctx, &apiv1alpha1.SuspendSessionRequest{SessionId: old.sessionID})
		if status.Code(err) == codes.FailedPrecondition {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return response.GetSession().GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, nil
	}))
	resumed, err := old.sessions.ResumeSession(old.ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: old.sessionID})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.GetSession().GetState())
	require.EqualValues(t, 2, calls.Load(), "Resume must not replay either Session's completed call")
	resumed, err = old.sessions.ResumeSession(old.ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: old.sessionID})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.GetSession().GetState())
	require.EqualValues(t, 2, calls.Load(), "repeated Resume is idempotent and must not replay calls")
	restored := send(old, "restored")
	require.Contains(t, restored, "restored")
	require.Contains(t, restored, "called:old_tool")
	require.Contains(t, restored, "LIVE_CATALOG_CHANGED")
	require.NotContains(t, restored, "new_tool")
	require.EqualValues(t, 3, calls.Load(), "Data restore must not replay completed calls")
	after, err := old.sessions.GetSession(old.ctx, &apiv1alpha1.GetSessionRequest{SessionId: old.sessionID})
	require.NoError(t, err)
	require.Equal(t, before.GetSession().GetPreparedRevision(), after.GetSession().GetPreparedRevision())
	actor, err = findSubstrateActor(old.ctx, old.system, "", substrate.ActorName(old.sessionID))
	require.NoError(t, err)
	require.Equal(t, uid, actor.GetMetadata().GetUid())
	template.Spec.Tools[0].MCP.ExposeAsCLI = new(false)
	update()
	native := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	require.Contains(t, send(native, "native"), "called:new_tool")
	require.EqualValues(t, 4, calls.Load())
	// Renaming the reference removes the old executable while retaining a new
	// CLI binding, so absence is observed through the real Bash tool.
	template.Spec.Tools[0].MCP.ExposeAsCLI = new(true)
	newName := server.DeepCopy()
	newName.ObjectMeta = metav1.ObjectMeta{GenerateName: "cli-renamed-", Namespace: "kagent"}
	require.NoError(t, kube.Create(t.Context(), newName))
	t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), newName)) })
	template.Spec.Tools[0].MCP.Server.Name = newName.Name
	update()
	renamed := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	removed := send(renamed, "removed")
	require.Contains(t, removed, "absent-command")
	require.NotContains(t, removed, "stale-command")
	require.EqualValues(t, 4, calls.Load(), "help, removed commands and replacement cannot call tools")
	require.Contains(t, send(renamed, "renamed:"+newName.Name), "called:new_tool")
	require.EqualValues(t, 5, calls.Load())
	template.Spec.Tools = nil
	update()
	empty := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
	require.Contains(t, send(empty, "empty"), "bindings-removed")
	require.EqualValues(t, 5, calls.Load(), "removing all bindings must not retain or replay tools")
	require.Contains(t, send(old, "old"), "called:old_tool")
	require.EqualValues(t, 6, calls.Load())
}
