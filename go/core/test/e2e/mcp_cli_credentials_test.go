package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/mockllm"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Exercise the recording fixtures independently so a broken test origin cannot
// be mistaken for a credential or routing regression in the built runtime.
func TestMCPCLICredentialFixture(t *testing.T) {
	t.Setenv("KAGENT_E2E_LOCAL_HOST", "localhost")
	for _, protocol := range []v1alpha3.RemoteMCPServerProtocol{v1alpha3.RemoteMCPServerProtocolStreamableHttp, v1alpha3.RemoteMCPServerProtocolSse} {
		for _, gateway := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gateway=%t", protocol, gateway), func(t *testing.T) {
				endpoint, origin, proxy := startMCPCLICredentialOrigin(t, protocol, gateway, "fixture-credential")
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
				require.NoError(t, err)
				response, err := http.DefaultClient.Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
				client := &http.Client{Transport: mcpCLIFixtureTransport{}}
				var transport mcpsdk.Transport = &mcpsdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}
				if protocol == v1alpha3.RemoteMCPServerProtocolSse {
					transport = &mcpsdk.SSEClientTransport{Endpoint: endpoint, HTTPClient: client}
				}
				session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "fixture-probe", Version: "1"}, nil).Connect(t.Context(), transport, nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, session.Close()) }()
				tools, err := session.ListTools(t.Context(), nil)
				require.NoError(t, err)
				require.Len(t, tools.Tools, 1)
				result, err := session.CallTool(t.Context(), &mcpsdk.CallToolParams{Name: "add_numbers", Arguments: map[string]any{"a": 3, "b": 5}})
				require.NoError(t, err)
				require.False(t, result.IsError)
				require.Equal(t, "8", result.Content[0].(*mcpsdk.TextContent).Text)
				if gateway {
					require.Len(t, proxy.snapshot(), len(origin.snapshot()))
				}
			})
		}
	}
}

type mcpCLIFixtureTransport struct{}

func (mcpCLIFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("X-CLI-Secret", "fixture-credential")
	return http.DefaultTransport.RoundTrip(request)
}

func TestMCPCLICredentialRouting(t *testing.T) {
	t.Parallel()
	for _, protocol := range []v1alpha3.RemoteMCPServerProtocol{v1alpha3.RemoteMCPServerProtocolStreamableHttp, v1alpha3.RemoteMCPServerProtocolSse} {
		for _, gateway := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gateway=%t", protocol, gateway), func(t *testing.T) {
				t.Parallel()
				kube := interactionKubeClient(t)
				// This synthetic value is stored only in a Secret. The origin refuses
				// requests without actual Substrate credential injection.
				credential := "cli-e2e-" + uuid.NewString()
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-auth-", Namespace: "kagent"}, StringData: map[string]string{"token": credential}}
				require.NoError(t, kube.Create(t.Context(), secret))
				t.Cleanup(func() { require.NoError(t, kube.Delete(context.Background(), secret)) })
				endpoint, origin, proxy := startMCPCLICredentialOrigin(t, protocol, gateway, credential)
				server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-credentials-", Namespace: "kagent"}, Spec: v1alpha3.RemoteMCPServerSpec{
					Description: "Add numbers using a credential-protected connection", Protocol: protocol, URL: endpoint,
					HeadersFrom: []v1alpha3.ValueRef{{Name: "X-CLI-Secret", ValueFrom: &v1alpha3.ValueSource{Type: v1alpha3.SecretValueSource, Name: secret.Name, Key: "token"}}},
				}}
				require.NoError(t, kube.Create(t.Context(), server))
				t.Cleanup(func() {
					if err := kube.Delete(context.Background(), server); err != nil && !apierrors.IsNotFound(err) {
						t.Errorf("delete credential MCP server: %v", err)
					}
				})
				config, err := mockllm.LoadConfigFromFile("mocks/invoke_mcp_agent.json", interactionMocks)
				require.NoError(t, err)
				prompt := "Add 3 and 5 using the authenticated CLI."
				require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":"Add 3 and 5 using the authenticated CLI."}`), &config.OpenAI[0].Match.Message))
				call := &config.OpenAI[0].Response.Choices[0].Message.ToolCalls[0]
				call.Function.Name = "bash"
				arguments, err := json.Marshal(map[string]string{"command": server.Name + " --help && " + server.Name + " add_numbers --a 3 --b 5"})
				require.NoError(t, err)
				call.Function.Arguments = string(arguments)
				modelRecorder := startModelRecorder(t, startMockLLMConfig(t, config), func(body []byte) error {
					if bytes.Contains(body, []byte(credential)) {
						return fmt.Errorf("Secret credential leaked into model input")
					}
					return nil
				})
				harness := testHarness{name: "kagent", runtimeLabel: "kagent"}
				model := harness.createModel(t, kube, reachableModelURL(t, modelRecorder.URL), nil)
				template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{GenerateName: "cli-auth-agent-", Namespace: "kagent", Labels: harness.labels()}, Spec: v1alpha3.AgentTemplateSpec{
					ModelConfig: &corev1.LocalObjectReference{Name: model.Name}, SystemPrompt: "Use the configured CLI to add numbers.",
					Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"add_numbers"}, ExposeAsCLI: new(true)}}},
				}}
				createAndWaitInteractionTemplateForHarness(t, kube, template, harness.name)
				fixture := newInteractionFixtureForHarnessTemplate(t, interactionTarget(t), harness.name, template.Name)
				_, _, task := fixture.send(t, prompt)
				require.Equal(t, a2atype.TaskStateCompleted, task.Status.State, "%s", taskText(task))
				require.Contains(t, taskText(task), "result is 8")
				// The public inventory exposes the compiled runtime template. Inspect
				// that exact Session's template, rather than an unrelated manifest.
				system := apiv1alpha1.NewSystemServiceClient(newControllerConn(t, interactionTarget(t)))
				actor, err := findSubstrateActor(fixture.ctx, system, "", substrate.ActorName(fixture.sessionID))
				require.NoError(t, err)
				require.NotNil(t, actor)
				compiled, err := system.GetSubstrateSummary(fixture.ctx, &apiv1alpha1.GetSubstrateSummaryRequest{Namespace: "kagent", Atespace: actor.GetActorTemplate().GetAtespace()})
				require.NoError(t, err)
				require.Empty(t, compiled.GetAteApiError())
				found := false
				for _, runtimeTemplate := range compiled.GetActorTemplates() {
					if runtimeTemplate.GetMetadata().GetName() == actor.GetActorTemplate().GetName() {
						found = true
						body, err := protojson.Marshal(runtimeTemplate)
						require.NoError(t, err)
						require.NotContains(t, string(body), credential, "compiled runtime inputs must retain credential placeholders")
					}
				}
				require.True(t, found, "Session runtime template must be present for leak inspection")
				calls := 0
				for _, request := range origin.snapshot() {
					require.Equal(t, credential, request.header.Get("X-CLI-Secret"))
					require.True(t, strings.HasPrefix(request.path, mcpCLICredentialRoute), "unexpected origin route: %s", request.path)
					if gateway {
						require.Equal(t, "recording-gateway", request.header.Get("X-CLI-Gateway"), "origin must not be reached directly")
					}
					if request.method == "tools/call" {
						calls++
						require.JSONEq(t, `{"a":3,"b":5}`, mcpToolCallArguments(t, request.body))
					}
				}
				require.Equal(t, 1, calls, "help must not call a tool and dispatch must not replay")
				if gateway {
					require.Len(t, proxy.snapshot(), len(origin.snapshot()), "every origin request must traverse the configured gateway")
				}
				require.NotEmpty(t, modelRecorder.Requests("Content-Type", "application/json"), "leak inspection must observe actual model input")
			})
		}
	}
}

const mcpCLICredentialRoute = "/configured-gateway/tenant/cli-mcp"

type mcpCLICredentialRequest struct {
	path, method string
	header       http.Header
	body         []byte
}

type mcpCLICredentialRecorder struct {
	mu       sync.Mutex
	requests []mcpCLICredentialRequest
}

func (r *mcpCLICredentialRecorder) record(request *http.Request, body []byte) {
	var rpc struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &rpc)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, mcpCLICredentialRequest{path: request.URL.Path, method: rpc.Method, header: request.Header.Clone(), body: body})
}

func (r *mcpCLICredentialRecorder) snapshot() []mcpCLICredentialRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mcpCLICredentialRequest(nil), r.requests...)
}

func startMCPCLICredentialOrigin(t *testing.T, protocol v1alpha3.RemoteMCPServerProtocol, gateway bool, credential string) (string, *mcpCLICredentialRecorder, *mcpCLICredentialRecorder) {
	t.Helper()
	origin, gatewayRecorder := &mcpCLICredentialRecorder{}, &mcpCLICredentialRecorder{}
	var sessions sync.Map
	server := startMCPCLIHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		origin.record(r, body)
		if r.Header.Get("X-CLI-Secret") != credential || (gateway && r.Header.Get("X-CLI-Gateway") != "recording-gateway") {
			http.Error(w, "credential or gateway marker missing", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != mcpCLICredentialRoute && r.URL.Path != mcpCLICredentialRoute+"/messages" {
			http.NotFound(w, r)
			return
		}
		if protocol == v1alpha3.RemoteMCPServerProtocolSse && r.Method == http.MethodGet {
			id := uuid.NewString()
			responses := make(chan []byte, 16)
			sessions.Store(id, responses)
			defer sessions.Delete(id)
			w.Header().Set("Content-Type", "text/event-stream")
			if _, err := fmt.Fprintf(w, "event: endpoint\ndata: %s/messages?session=%s\n\n", mcpCLICredentialRoute, id); err != nil {
				return
			}
			flusher := w.(http.Flusher)
			flusher.Flush()
			for {
				select {
				case response := <-responses:
					if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", response); err != nil {
						return
					}
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		response, err := mcpCLICredentialResponse(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if protocol == v1alpha3.RemoteMCPServerProtocolSse {
			value, ok := sessions.Load(r.URL.Query().Get("session"))
			if !ok {
				http.Error(w, "unknown SSE session", http.StatusNotFound)
				return
			}
			if response != nil {
				select {
				case value.(chan []byte) <- response:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if response == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	endpoint := server.URL
	if gateway {
		upstream, err := url.Parse(server.URL)
		require.NoError(t, err)
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		proxy.FlushInterval = -1
		endpoint = startMCPCLIHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read gateway request", http.StatusBadRequest)
				return
			}
			gatewayRecorder.record(r, body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Set("X-CLI-Gateway", "recording-gateway")
			proxy.ServeHTTP(w, r)
		})).URL
	}
	return reachableServerURL(t, endpoint, mcpCLICredentialRoute), origin, gatewayRecorder
}

func startMCPCLIHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	require.NoError(t, server.Listener.Close())
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func mcpCLICredentialResponse(body []byte) ([]byte, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				A float64 `json:"a"`
				B float64 `json:"b"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if len(request.ID) == 0 {
		return nil, nil
	}
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "credential-fixture", "version": "1"}}
	case "tools/list":
		result = json.RawMessage(`{"tools":[{"name":"add_numbers","description":"Add two numbers","inputSchema":{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"],"additionalProperties":false}}]}`)
	case "tools/call":
		if request.Params.Name != "add_numbers" {
			return nil, fmt.Errorf("unexpected tool %q", request.Params.Name)
		}
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": fmt.Sprintf("%g", request.Params.Arguments.A+request.Params.Arguments.B)}}}
	case "ping":
		result = map[string]any{}
	default:
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}
