package models_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kagent-dev/kagent/go/adk/pkg/models"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestNativeOpenAIProviderWirePreservesTransportContextAndUnchangedHumanResult(t *testing.T) {
	for _, withContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary result bytes unchanged", true: "separate service provenance reaches provider"}[withContext], func(t *testing.T) {
			var body struct {
				Messages []struct{ Role, Content string }
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, `{"id":"probe","object":"chat.completion","created":1,"model":"local-qwen","choices":[{"index":0,"message":{"role":"assistant","content":"probe completed"},"finish_reason":"stop"}]}`); err != nil {
					t.Error(err)
				}
			}))
			defer peer.Close()
			provider := &models.OpenAIModel{Config: &models.OpenAIConfig{Model: "local-qwen"}, Client: openai.NewClient(option.WithAPIKey("synthetic-local-test"), option.WithBaseURL(peer.URL), option.WithHTTPClient(peer.Client())), Logger: slog.New(slog.DiscardHandler)}
			humanResult := `[{"question":"Return the qualification token?","answer":["PROBE_ACTUAL_HUMAN_ANSWER"]}]`
			transport := map[string]any{"slack_source": map[string]any{"operation_id": "PROBE_LATEST_OPERATION", "recorded_at": "PROBE_LATEST_RECORDED_AT"}, "receipt": map[string]any{"path": "PROBE_ORIGINAL_REFERENCE", "sha256": "PROBE_ORIGINAL_HASH"}, "receipt_source": map[string]any{"operation_id": "PROBE_ORIGINAL_OPERATION"}}
			response := map[string]any{"result": humanResult}
			if withContext {
				response["transport_context"] = transport
			}
			call := genai.NewPartFromFunctionCall("ask_user", map[string]any{"questions": []any{map[string]any{"question": "Return the qualification token?"}}})
			call.FunctionCall.ID = "ask-payer"
			result := genai.NewPartFromFunctionResponse("ask_user", response)
			result.FunctionResponse.ID = "ask-payer"
			request := &model.LLMRequest{Contents: []*genai.Content{{Role: "model", Parts: []*genai.Part{call}}, {Role: "user", Parts: []*genai.Part{result}}}}
			for _, err := range provider.GenerateContent(t.Context(), request, false) {
				if err != nil {
					t.Fatal(err)
				}
			}
			toolContent := ""
			for _, message := range body.Messages {
				if message.Role == "tool" {
					toolContent = message.Content
				}
			}
			if !withContext {
				if toolContent != humanResult {
					t.Fatal("no-context result bytes changed")
				}
				return
			}
			var delivered struct {
				Result    string         `json:"result"`
				Transport map[string]any `json:"transport_context"`
			}
			if err := json.Unmarshal([]byte(toolContent), &delivered); err != nil {
				t.Fatalf("provider tool content did not retain separate result/context envelope: %v", err)
			}
			if delivered.Result != humanResult || !reflect.DeepEqual(delivered.Transport, transport) {
				t.Fatal("provider serialized tool content lost separate context or changed human result")
			}
		})
	}
}
