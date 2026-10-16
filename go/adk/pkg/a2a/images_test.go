package a2a_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	pb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	nativea2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/models"
	localsession "github.com/kagent-dev/kagent/go/adk/pkg/session"
	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	api "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestCheckpointImagesReachActualResumedNativeOpenAIRequest(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "native SQLite"}[persistent], func(t *testing.T) {
			for _, withImages := range []bool{false, true} {
				t.Run(map[bool]string{false: "ordinary answer and expense context unchanged", true: "two actual raw images survive resumed runner and followup"}[withImages], func(t *testing.T) {
					var pngBytes, jpegBytes bytes.Buffer
					fixture := image.NewRGBA(image.Rect(0, 0, 7, 9))
					if err := png.Encode(&pngBytes, fixture); err != nil {
						t.Fatal(err)
					}
					if err := jpeg.Encode(&jpegBytes, fixture, nil); err != nil {
						t.Fatal(err)
					}
					originals := [][]byte{pngBytes.Bytes(), jpegBytes.Bytes()}
					requests := []map[string]any{}
					peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						requests = append(requests, body)
						w.Header().Set("Content-Type", "application/json")
						reply := `{"id":"probe","object":"chat.completion","created":1,"model":"local-qwen","choices":[{"index":0,"message":{"role":"assistant","content":"continued"},"finish_reason":"stop"}]}`
						if len(requests) == 1 {
							reply = `{"id":"probe","object":"chat.completion","created":1,"model":"local-qwen","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ask-images","type":"function","function":{"name":"ask_user","arguments":"{\"questions\":[{\"question\":\"First input?\"},{\"question\":\"Second input?\"}]}"}}]},"finish_reason":"tool_calls"}]}`
						}
						if len(requests) == 2 {
							reply = `{"id":"probe","object":"chat.completion","created":1,"model":"local-qwen","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-next","type":"function","function":{"name":"probe_next","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
						}
						if _, err := io.WriteString(w, reply); err != nil {
							t.Error(err)
						}
					}))
					defer peer.Close()
					provider := &models.OpenAIModel{Config: &models.OpenAIConfig{Model: "local-qwen"}, Client: openai.NewClient(option.WithAPIKey("synthetic-local-test"), option.WithBaseURL(peer.URL), option.WithHTTPClient(peer.Client())), Logger: slog.New(slog.DiscardHandler)}
					ask, err := tools.NewAskUserTool()
					if err != nil {
						t.Fatal(err)
					}
					next, err := functiontool.New(functiontool.Config{Name: "probe_next"}, func(_ adkagent.Context, _ struct{}) (map[string]any, error) {
						return map[string]any{"result": "next"}, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					actor, err := llmagent.New(llmagent.Config{Name: "ask", Model: provider, Tools: []tool.Tool{ask, next}})
					if err != nil {
						t.Fatal(err)
					}
					sessions := session.InMemoryService()
					dbURL := "sqlite:///" + filepath.Join(t.TempDir(), "sessions.db")
					if persistent {
						sessions, err = localsession.NewLocalSessionService(dbURL)
						if err != nil {
							t.Fatal(err)
						}
					}
					executor, err := nativea2a.NewKAgentExecutor(nativea2a.KAgentExecutorConfig{AppName: "image-probe", SessionService: sessions, Logger: slog.New(slog.DiscardHandler), RunnerConfig: runner.Config{AppName: "image-probe", Agent: actor}})
					if err != nil {
						t.Fatal(err)
					}
					ctx, callCtx := a2asrv.NewCallContext(t.Context(), a2asrv.NewServiceParams(map[string][]string{a2atype.SvcParamExtensions: {nativea2a.HITLExtensionURI}}))
					callCtx.User = a2asrv.NewAuthenticatedUser("synthetic-user", nil)
					if _, _, err := nativea2a.HITLActivationInterceptor().Before(ctx, callCtx, &a2asrv.Request{}); err != nil {
						t.Fatal(err)
					}
					initial := &a2asrv.ExecutorContext{TaskID: "image-task", ContextID: "image-session", Message: a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Ask for two inputs."))}
					var pending *a2atype.TaskStatusUpdateEvent
					for event, err := range executor.Execute(ctx, initial) {
						if err != nil {
							t.Fatal(err)
						}
						if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State == a2atype.TaskStateInputRequired {
							pending = status
						}
					}
					if pending == nil {
						t.Fatal("actual native ask_user did not pause")
					}
					request := nativea2a.GetAskUserRequest(pending.Status.Message)
					if request == nil {
						t.Fatal("pending native correlation missing")
					}
					transport := map[string]any{"slack_source": map[string]any{"operation_id": "latest-operation"}, "receipt_source": map[string]any{"operation_id": "original-operation"}}
					payload := map[string]any{"type": api.HITLTypeAskUserResponse, "id": request.ID, "answers": []any{map[string]any{"answer": []any{"first caption"}}, map[string]any{"answer": []any{"second caption"}}}, "transport_context": transport}
					metadata, err := structpb.NewStruct(map[string]any{nativea2a.HITLExtensionURI: payload})
					if err != nil {
						t.Fatal(err)
					}
					incomingProto := &pb.Message{MessageId: "image-answer", Role: pb.Role_ROLE_USER, Metadata: metadata, Extensions: []string{nativea2a.HITLExtensionURI}}
					if withImages {
						for index, data := range originals {
							mime := []string{"image/png", "image/jpeg"}[index]
							incomingProto.Parts = append(incomingProto.Parts, &pb.Part{Content: &pb.Part_Raw{Raw: data}, Filename: []string{"one.png", "two.jpg"}[index], MediaType: mime})
						}
					} else {
						incomingProto.Parts = []*pb.Part{{Content: &pb.Part_Text{Text: "second caption"}}}
					}
					wire, err := proto.Marshal(incomingProto)
					if err != nil {
						t.Fatal(err)
					}
					var decoded pb.Message
					if err := proto.Unmarshal(wire, &decoded); err != nil {
						t.Fatal(err)
					}
					incoming, err := pbconv.FromProtoMessage(&decoded)
					if err != nil {
						t.Fatal(err)
					}
					stored := &a2atype.Task{ID: initial.TaskID, ContextID: initial.ContextID, Status: pending.Status}
					continuation := &a2asrv.ExecutorContext{TaskID: initial.TaskID, ContextID: initial.ContextID, Message: incoming, StoredTask: stored}
					completeImageTask(t, executor, ctx, continuation)
					if len(requests) != 3 {
						t.Fatalf("provider request count %d", len(requests))
					}
					assertImageProviderRequest(t, requests[1], originals, withImages, true, transport)
					assertImageProviderRequest(t, requests[2], originals, withImages, true, transport)
					if persistent {
						sessions, err = localsession.NewLocalSessionService(dbURL)
						if err != nil {
							t.Fatal(err)
						}
						executor, err = nativea2a.NewKAgentExecutor(nativea2a.KAgentExecutorConfig{AppName: "image-probe", SessionService: sessions, Logger: slog.New(slog.DiscardHandler), RunnerConfig: runner.Config{AppName: "image-probe", Agent: actor}})
						if err != nil {
							t.Fatal(err)
						}
					}
					followup := &a2asrv.ExecutorContext{TaskID: "followup-task", ContextID: initial.ContextID, Message: a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Use the earlier inputs."))}
					completeImageTask(t, executor, ctx, followup)
					if len(requests) != 4 {
						t.Fatal("same-session followup missing")
					}
					assertImageProviderRequest(t, requests[3], originals, withImages, false, transport)

					storedSession, err := sessions.Get(t.Context(), &session.GetRequest{AppName: "image-probe", UserID: "synthetic-user", SessionID: initial.ContextID})
					if err != nil {
						t.Fatal(err)
					}
					imageEvents := 0
					resumedResultSeen := false
					for event := range storedSession.Session.Events().All() {
						if event.Content == nil {
							continue
						}
						imageOnly := len(event.Content.Parts) > 0
						for _, part := range event.Content.Parts {
							if part.FunctionResponse != nil && part.FunctionResponse.Name == "ask_user" {
								if result, _ := part.FunctionResponse.Response["result"].(string); result != "" {
									resumedResultSeen = true
								}
							}
							if part.InlineData == nil {
								imageOnly = false
							}
						}
						if imageOnly {
							imageEvents++
							if !resumedResultSeen {
								t.Fatal("image history precedes correlated native ask_user result")
							}
							if len(event.Content.Parts) != 2 {
								t.Fatal("retained native image ordering lost")
							}
						}
					}
					wantEvents := 0
					if withImages {
						wantEvents = 1
					}
					if imageEvents != wantEvents {
						t.Fatalf("image-only native history events %d; want %d", imageEvents, wantEvents)
					}
					if persistent {
						isolatedSessions, err := localsession.NewLocalSessionService("sqlite:///" + filepath.Join(t.TempDir(), "isolated.db"))
						if err != nil {
							t.Fatal(err)
						}
						executor, err = nativea2a.NewKAgentExecutor(nativea2a.KAgentExecutorConfig{AppName: "image-probe", SessionService: isolatedSessions, Logger: slog.New(slog.DiscardHandler), RunnerConfig: runner.Config{AppName: "image-probe", Agent: actor}})
						if err != nil {
							t.Fatal(err)
						}
					}
					isolated := &a2asrv.ExecutorContext{TaskID: "isolated-task", ContextID: "isolated-session", Message: a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Describe this text."))}
					completeImageTask(t, executor, ctx, isolated)
					if len(requests) != 5 {
						t.Fatal("isolated provider request missing")
					}
					assertImageProviderRequest(t, requests[4], nil, false, false, nil)
				})
			}
		})
	}
}

func completeImageTask(t *testing.T, executor *nativea2a.KAgentExecutor, ctx context.Context, request *a2asrv.ExecutorContext) {
	t.Helper()
	completed := false
	for event, err := range executor.Execute(ctx, request) {
		if err != nil {
			t.Fatal(err)
		}
		if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State.Terminal() {
			if status.Status.State != a2atype.TaskStateCompleted {
				t.Fatalf("native terminal state %s; want completed", status.Status.State)
			}
			completed = true
		}
	}
	if !completed {
		t.Fatal("native task never completed")
	}
}

func assertImageProviderRequest(t *testing.T, request map[string]any, originals [][]byte, withImages, checkAnswer bool, transport map[string]any) {
	t.Helper()
	var delivered [][]byte
	foundAnswer := false
	answerIndex := -1
	firstImageIndex := -1
	for messageIndex, raw := range request["messages"].([]any) {
		message := raw.(map[string]any)
		if message["role"] == "tool" {
			text, _ := message["content"].(string)
			var result struct {
				Result    string         `json:"result"`
				Transport map[string]any `json:"transport_context"`
			}
			if json.Unmarshal([]byte(text), &result) == nil && result.Result != "" {
				var answers []map[string]any
				if err := json.Unmarshal([]byte(result.Result), &answers); err != nil {
					t.Fatal(err)
				}
				if len(answers) != 2 || !reflect.DeepEqual(answers[0]["answer"], []any{"first caption"}) || !reflect.DeepEqual(answers[1]["answer"], []any{"second caption"}) || !reflect.DeepEqual(result.Transport, transport) {
					t.Fatal("answers or separate expense context changed")
				}
				foundAnswer = true
				answerIndex = messageIndex
			}
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part := rawPart.(map[string]any)
			if part["type"] != "image_url" {
				continue
			}
			url := part["image_url"].(map[string]any)["url"].(string)
			header, body, ok := strings.Cut(url, ",")
			if !ok || (header != "data:image/png;base64" && header != "data:image/jpeg;base64") {
				t.Fatal("wrong native image representation")
			}
			data, err := base64.StdEncoding.DecodeString(body)
			if err != nil {
				t.Fatal(err)
			}
			delivered = append(delivered, data)
			if firstImageIndex < 0 {
				firstImageIndex = messageIndex
			}
		}
	}
	if checkAnswer && !foundAnswer {
		t.Fatal("resumed ask_user answer not serialized")
	}
	if withImages && checkAnswer && firstImageIndex <= answerIndex {
		t.Fatal("provider images precede correlated native result")
	}
	if withImages && !reflect.DeepEqual(delivered, originals) {
		t.Fatalf("actual provider image count %d; expected ordered exact originals", len(delivered))
	}
	if !withImages && len(delivered) != 0 {
		t.Fatal("ordinary answer acquired image payload")
	}
}
