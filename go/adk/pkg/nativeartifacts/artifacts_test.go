package nativeartifacts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"iter"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/loadartifactstool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/genai"
)

// An actual PNG, so assertions exercise binary Parts rather than textual stand-ins.
var pixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=")

type scriptedModel struct {
	step        int
	filename    string
	sawPixels   bool
	sawMetadata bool
	plain       bool
	lastOutput  any
}

func (*scriptedModel) Name() string { return "pixel-probe" }
func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var part *genai.Part
		switch m.step {
		case 0:
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: "capture-1", Name: "capture", Args: map[string]any{}}}
		case 1:
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil && p.FunctionResponse.Name == "capture" {
						m.lastOutput = p.FunctionResponse.Response["output"]
						raw, _ := json.Marshal(p.FunctionResponse.Response)
						m.sawMetadata = strings.Contains(string(raw), "example.org") && strings.Contains(string(raw), "page title")
					}
				}
			}
			if m.plain {
				m.step++
				yield(&model.LLMResponse{Content: genai.NewContentFromText("done", "model")}, nil)
				return
			}
			// The store's names advertised by the built-in loader are also observable
			// via tool results. Decode the compact envelope, not private adapter state.
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil && p.FunctionResponse.Name == "capture" {
						output, ok := p.FunctionResponse.Response["output"].(map[string]any)
						if !ok {
							continue
						}
						refs, ok := output["artifacts"].([]any)
						if !ok {
							continue
						}
						if len(refs) > 0 {
							ref, _ := refs[0].(map[string]any)
							m.filename, _ = ref["filename"].(string)
						}
					}
				}
			}
			if m.filename == "" {
				yield(nil, fmt.Errorf("capture did not return an artifact filename"))
				return
			}
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: "load-1", Name: "load_artifacts", Args: map[string]any{"artifact_names": []any{m.filename}}}}
		case 3:
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: "load-later", Name: "load_artifacts", Args: map[string]any{"artifact_names": []any{m.filename}}}}
		case 2, 4:
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.InlineData != nil && p.InlineData.MIMEType == "image/png" && bytes.Equal(p.InlineData.Data, pixelPNG) {
						m.sawPixels = true
					}
				}
			}
			part = genai.NewPartFromText("examined pixels")
		default:
			yield(nil, fmt.Errorf("unexpected model call"))
			return
		}
		m.step++
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{part}}}, nil)
	}
}

func artifactRunner(t *testing.T, probe *scriptedModel, response *mcp.CallToolResult, sessions session.Service, artifacts artifact.Service) *runner.Runner {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "test"}, nil)
	var firstSession *mcp.ServerSession
	server.AddTool(&mcp.Tool{Name: "capture", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if firstSession == nil {
			firstSession = req.Session
		} else if req.Session != firstSession {
			return nil, fmt.Errorf("MCP session changed between captures")
		}
		return response, nil
	})
	ss, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Error(err)
		}
	})
	ts, err := mcptoolset.New(mcptoolset.Config{Transport: clientTransport, Client: NewClient()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := llmagent.New(llmagent.Config{Name: "browser", Model: probe, Tools: []tool.Tool{loadartifactstool.New()}, Toolsets: []tool.Toolset{Wrap(ts)}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: a, SessionService: sessions, ArtifactService: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestScreenshotReachesModelWithoutPersistingBytes(t *testing.T) {
	if _, err := png.Decode(bytes.NewReader(pixelPNG)); err != nil {
		t.Fatalf("invalid PNG fixture: %v", err)
	}
	sessions := session.InMemoryService()
	_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	probe := &scriptedModel{}
	response := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "page title"}, &mcp.ImageContent{MIMEType: "image/png", Data: pixelPNG}}, StructuredContent: map[string]any{"url": "https://example.org"}}
	r := artifactRunner(t, probe, response, sessions, ProcessService())
	for _, err := range r.Run(t.Context(), "alex", "one", genai.NewContentFromText("capture then view", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !probe.sawPixels {
		t.Fatal("model did not receive exact PNG pixels")
	}
	if !probe.sawMetadata {
		t.Fatal("mixed structured/image result lost original metadata or text")
	}
	firstFilename := probe.filename
	probe.step = 0
	probe.filename = ""
	for _, err := range r.Run(t.Context(), "alex", "one", genai.NewContentFromText("capture again", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if probe.filename == firstFilename {
		t.Fatal("new screenshot overwrote earlier filename")
	}
	stored, err := sessions.Get(t.Context(), &session.GetRequest{AppName: "test", UserID: "alex", SessionID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stored.Session.Events().All() {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(pixelPNG))) || bytes.Contains(raw, []byte("inlineData")) {
			t.Fatal("binary data persisted in session history")
		}
	}
}

func TestLaterRunReloadsAndOtherSessionCannotLoad(t *testing.T) {
	sessions := session.InMemoryService()
	for _, id := range []string{"reload", "other"} {
		_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
	}
	response := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixelPNG}}}
	probe := &scriptedModel{}
	r := artifactRunner(t, probe, response, sessions, ProcessService())
	run := func(r *runner.Runner, id string) error {
		for _, err := range r.Run(t.Context(), "alex", id, genai.NewContentFromText("view", "user"), agent.RunConfig{}) {
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(r, "reload"); err != nil {
		t.Fatal(err)
	}
	// A newly constructed runner shares the process store, not a per-run store.
	later := &scriptedModel{step: 3, filename: probe.filename}
	r = artifactRunner(t, later, response, sessions, ProcessService())
	if err := run(r, "reload"); err != nil {
		t.Fatal(err)
	}
	if !later.sawPixels {
		t.Fatal("later run lost screenshot")
	}
	foreign := &scriptedModel{step: 3, filename: probe.filename}
	r = artifactRunner(t, foreign, response, sessions, ProcessService())
	if err := run(r, "other"); err == nil {
		t.Fatal("another session loaded a foreign screenshot")
	}
	if foreign.sawPixels {
		t.Fatal("foreign screenshot pixels reached model")
	}
	restarted := &scriptedModel{step: 3, filename: probe.filename}
	r = artifactRunner(t, restarted, response, sessions, artifact.InMemoryService())
	if err := run(r, "reload"); err == nil {
		t.Fatal("new process store unexpectedly retained screenshot")
	} else if !strings.Contains(err.Error(), "failed to load artifact "+probe.filename) {
		t.Fatalf("restart failure did not identify missing artifact: %v", err)
	}
	if restarted.sawPixels {
		t.Fatal("restart loss was concealed")
	}
}

func TestTextOnlyMCPResultIsUnchanged(t *testing.T) {
	sessions := session.InMemoryService()
	_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: "text"})
	if err != nil {
		t.Fatal(err)
	}
	probe := &scriptedModel{plain: true}
	response := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ordinary text"}}}
	r := artifactRunner(t, probe, response, sessions, artifact.InMemoryService())
	for _, err := range r.Run(t.Context(), "alex", "text", genai.NewContentFromText("read", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if probe.lastOutput != "ordinary text" {
		t.Fatalf("text changed: %#v", probe.lastOutput)
	}
}

func TestRejectedBatchSavesNoArtifacts(t *testing.T) {
	for _, testcase := range []struct {
		name, mime string
		data       []byte
	}{
		{"budget", "image/png", make([]byte, 3*1024*1024)},
		{"invalid header", "image/png", []byte("not an image")},
		{"unsupported MIME", "image/gif", pixelPNG},
		{"empty", "image/png", nil},
		{"MIME mismatch", "image/jpeg", pixelPNG},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			sessions := session.InMemoryService()
			_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: "budget"})
			if err != nil {
				t.Fatal(err)
			}
			store := artifact.InMemoryService()
			response := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixelPNG}, &mcp.ImageContent{MIMEType: testcase.mime, Data: testcase.data}}}
			r := artifactRunner(t, &scriptedModel{}, response, sessions, store)
			failed := false
			for _, err := range r.Run(t.Context(), "alex", "budget", genai.NewContentFromText("capture", "user"), agent.RunConfig{}) {
				if err != nil {
					failed = true
				}
			}
			if !failed {
				t.Fatal("invalid screenshot batch succeeded")
			}
			listed, err := store.List(t.Context(), &artifact.ListRequest{AppName: "test", UserID: "alex", SessionID: "budget"})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.FileNames) != 0 {
				t.Fatalf("rejected batch saved artifacts: %v", listed.FileNames)
			}
		})
	}
}

func TestExactDecodedBudgetIsAccepted(t *testing.T) {
	sessions := session.InMemoryService()
	_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3*1024*1024)
	copy(data, pixelPNG)
	response := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: data}}}
	store := artifact.InMemoryService()
	r := artifactRunner(t, &scriptedModel{}, response, sessions, store)
	for _, err := range r.Run(t.Context(), "alex", "exact", genai.NewContentFromText("capture", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.List(t.Context(), &artifact.ListRequest{AppName: "test", UserID: "alex", SessionID: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.FileNames) != 1 {
		t.Fatalf("exact budget rejected: %v", listed.FileNames)
	}
}

func TestLineWrappedBase64AtExactBudgetIsAccepted(t *testing.T) {
	sessions := session.InMemoryService()
	_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "alex", SessionID: "wrapped"})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3*1024*1024)
	copy(data, pixelPNG)
	encoded := base64.StdEncoding.EncodeToString(data)
	var wrapped strings.Builder
	for offset := 0; offset < len(encoded); offset += 76 {
		end := min(offset+76, len(encoded))
		wrapped.WriteString(encoded[offset:end])
		wrapped.WriteString("\r\n")
	}
	raw, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "image", "mimeType": "image/png", "data": wrapped.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := functiontool.New(functiontool.Config{Name: "capture"}, func(ctx agent.Context, _ map[string]any) (map[string]any, error) {
		adapted, err := TransformResult(ctx, raw)
		if err != nil {
			return nil, err
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(adapted, &envelope); err != nil {
			return nil, err
		}
		var output map[string]any
		if err := json.Unmarshal(envelope["structuredContent"], &output); err != nil {
			return nil, err
		}
		return map[string]any{"output": output}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := llmagent.New(llmagent.Config{Name: "wrapped", Model: &scriptedModel{}, Tools: []tool.Tool{capture, loadartifactstool.New()}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: a, SessionService: sessions, ArtifactService: artifact.InMemoryService()})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(t.Context(), "alex", "wrapped", genai.NewContentFromText("capture", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
}
