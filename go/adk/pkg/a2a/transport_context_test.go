package a2a_test

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	nativea2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/tools"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type transportContextModel struct {
	step   int
	result map[string]any
}

func (*transportContextModel) Name() string { return "transport-context-probe" }
func (m *transportContextModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.step == 0 {
			m.step++
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "ask-payer", Name: "ask_user", Args: map[string]any{"questions": []any{map[string]any{"question": "Who paid?"}}}}}}}}, nil)
			return
		}
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				if part.FunctionResponse != nil && part.FunctionResponse.Name == "ask_user" {
					m.result = part.FunctionResponse.Response
				}
			}
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("continued", "model")}, nil)
	}
}

func TestAskUserTransportContextReachesResumedModelSeparatelyFromHumanAnswer(t *testing.T) {
	for _, withContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary answer unchanged", true: "expense latest source and original receipt"}[withContext], func(t *testing.T) {
			sessions := session.InMemoryService()
			if _, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "expenses", UserID: "alex", SessionID: "receipt-session"}); err != nil {
				t.Fatal(err)
			}
			probe := &transportContextModel{}
			ask, err := tools.NewAskUserTool()
			if err != nil {
				t.Fatal(err)
			}
			actor, err := llmagent.New(llmagent.Config{Name: "expenses", Model: probe, Tools: []tool.Tool{ask}})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := runner.New(runner.Config{AppName: "expenses", Agent: actor, SessionService: sessions})
			if err != nil {
				t.Fatal(err)
			}
			confirmationID := ""
			for event, err := range runtime.Run(t.Context(), "alex", "receipt-session", genai.NewContentFromText("original receipt source op-original", "user"), agent.RunConfig{}) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Content != nil {
					for _, part := range event.Content.Parts {
						if part.FunctionCall != nil && part.FunctionCall.Name == "adk_request_confirmation" {
							confirmationID = part.FunctionCall.ID
						}
					}
				}
			}
			if confirmationID == "" {
				t.Fatal("real runner did not ask payer")
			}
			transport := map[string]any{"slack_source": map[string]any{"operation_id": "op-payer-reply", "message": "1791209460.123456", "recorded_at": "2026-10-06T00:11:00Z"}, "receipt": map[string]any{"path": "receipts/original.jpg", "sha256": "original-hash"}, "receipt_source": map[string]any{"operation_id": "op-original", "message": "1791209400.123456"}}
			payload := map[string]any{"type": apia2a.HITLTypeAskUserResponse, "id": confirmationID, "answers": []any{map[string]any{"answer": []string{"Aili"}}}}
			if withContext {
				payload["transport_context"] = transport
			}
			incoming := nativea2a.AttachHitlExtension(a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Aili")), payload)
			stored := &a2atype.Task{Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired, Message: nativea2a.AttachHitlExtension(a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("Who paid?")), &apia2a.AskUserRequest{Type: apia2a.HITLTypeAskUserRequest, ID: confirmationID})}}
			resumed, err := nativea2a.BuildResumeHITLMessage(stored, incoming)
			if err != nil {
				t.Fatal(err)
			}
			content := &genai.Content{Role: "user"}
			for _, part := range resumed.Parts {
				activity, ok := apia2a.ParseToolActivity(part)
				if !ok {
					t.Fatal("resume lacks public tool-result contract")
				}
				response, ok := activity.Response.(map[string]any)
				if !ok {
					t.Fatal("resume tool result is not structured")
				}
				converted := genai.NewPartFromFunctionResponse(activity.Name, response)
				converted.FunctionResponse.ID = activity.ID
				content.Parts = append(content.Parts, converted)
			}
			for _, err := range runtime.Run(t.Context(), "alex", "receipt-session", content, agent.RunConfig{}) {
				if err != nil {
					t.Fatal(err)
				}
			}
			if probe.result == nil {
				t.Fatal("model never received resumed ask_user result")
			}
			var answers []struct {
				Question string   `json:"question"`
				Answer   []string `json:"answer"`
			}
			raw, ok := probe.result["result"].(string)
			if !ok {
				t.Fatal("ordinary answer result changed")
			}
			if err = json.Unmarshal([]byte(raw), &answers); err != nil {
				t.Fatal(err)
			}
			if len(answers) != 1 || answers[0].Question != "Who paid?" || !reflect.DeepEqual(answers[0].Answer, []string{"Aili"}) {
				t.Fatal("human answer changed", raw)
			}
			if withContext {
				if !reflect.DeepEqual(probe.result["transport_context"], transport) {
					t.Fatalf("latest financial source/original provenance absent from resumed model input: %#v", probe.result)
				}
			} else if _, exists := probe.result["transport_context"]; exists {
				t.Fatal("ordinary response gained transport context")
			}
		})
	}
}
