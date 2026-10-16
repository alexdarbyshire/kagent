package nativeartifacts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	runtimeRunner "github.com/kagent-dev/kagent/go/adk/pkg/runner"
	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

var pixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=")

func TestCLIImagesReachNativeModelAndStaySessionScoped(t *testing.T) {
	testConfiguredImages(t, true, false)
}
func TestNativeImagesReachModelThroughManagedScopedClient(t *testing.T) {
	testConfiguredImages(t, false, false)
}

func TestCLIErrorImagesPreserveEnvelopeWithoutReplay(t *testing.T) {
	testConfiguredImages(t, true, true)
}

func testConfiguredImages(t *testing.T, cli bool, isError bool) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	t.Setenv("TMPDIR", t.TempDir())
	var jpegData bytes.Buffer
	raster := image.NewRGBA(image.Rect(0, 0, 1, 1))
	raster.Set(0, 0, color.RGBA{R: 193, G: 42, B: 81, A: 255})
	require.NoError(t, jpeg.Encode(&jpegData, raster, nil))
	originals := [][]byte{pixelPNG, jpegData.Bytes()}
	var captures atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "capture", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		captures.Add(1)
		return &mcp.CallToolResult{IsError: isError, Meta: mcp.Meta{"source": "pixel fixture"}, Content: []mcp.Content{&mcp.TextContent{Text: "page title"}, &mcp.ImageContent{MIMEType: "image/png", Data: originals[0]}, &mcp.ImageContent{MIMEType: "image/jpeg", Data: originals[1]}}, StructuredContent: map[string]any{"count": int64(9007199254740993), "url": "https://example.org"}}, nil
	})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(remote.Close)
	var mu sync.Mutex
	var filenames []string
	var seen [][]byte
	phase := "capture"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var input struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		seen = nil
		for _, message := range input.Messages {
			var parts []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if json.Unmarshal(message.Content, &parts) == nil {
				for _, part := range parts {
					if part.Type == "image_url" {
						_, encoded, _ := strings.Cut(part.ImageURL.URL, ",")
						decoded, e := base64.StdEncoding.DecodeString(encoded)
						require.NoError(t, e)
						seen = append(seen, decoded)
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		last := input.Messages[len(input.Messages)-1]
		var text string
		_ = json.Unmarshal(last.Content, &text)
		toolName := "bash"
		arguments := `{"command":"browser capture"}`
		if !cli {
			toolName = "capture"
			arguments = `{}`
		}
		if phase == "capture" && last.Role == "tool" {
			if cli {
				require.Contains(t, text, "9007199254740993")
			}
			require.Contains(t, text, "page title")
			if cli {
				require.Contains(t, text, "pixel fixture")
			}
			require.NotContains(t, text, base64.StdEncoding.EncodeToString(pixelPNG))
			raw := text
			if isError {
				var failure struct {
					Error string `json:"error"`
				}
				if json.Unmarshal([]byte(text), &failure) == nil && failure.Error != "" {
					raw = failure.Error
				}
				start := strings.Index(raw, "{")
				require.NotEqual(t, -1, start)
				raw = raw[start:]
				require.Contains(t, raw, `"isError":true`)
			}
			var envelope map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(raw), &envelope))
			encodedRefs := envelope["structuredContent"]
			if !cli {
				encodedRefs = envelope["output"]
			}
			var adapted struct {
				Artifacts []struct {
					Filename string `json:"filename"`
				} `json:"artifacts"`
			}
			require.NoError(t, json.Unmarshal(encodedRefs, &adapted))
			filenames = nil
			for _, ref := range adapted.Artifacts {
				filenames = append(filenames, ref.Filename)
			}
			require.Len(t, filenames, 2)
			phase = "loading"
			toolName = "load_artifacts"
			encoded, _ := json.Marshal(map[string]any{"artifact_names": filenames})
			arguments = string(encoded)
		} else if phase == "followup" || phase == "foreign" || phase == "restart" {
			if last.Role != "tool" {
				phase = "loading"
				toolName = "load_artifacts"
				encoded, _ := json.Marshal(map[string]any{"artifact_names": filenames})
				arguments = string(encoded)
			} else {
				_, _ = fmt.Fprint(w, `{"id":"done","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
				return
			}
		} else if phase == "loading" {
			_, _ = fmt.Fprint(w, `{"id":"done","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"call","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"capture-call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`, toolName, arguments)
	}))
	t.Cleanup(peer.Close)
	owner, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	sessions := session.InMemoryService()
	for _, id := range []string{"mine", "foreign"} {
		_, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "cli-artifacts", UserID: "alex", SessionID: id})
		require.NoError(t, err)
	}
	agentConfig := &adk.AgentConfig{Model: &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: peer.URL}, CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"capture"}}}}}
	if !cli {
		agentConfig.HttpTools = []adk.HttpMcpServerConfig{agentConfig.CLITools[0].HTTP}
		agentConfig.CLITools = nil
	}
	config, err := runtimeRunner.CreateRunnerConfig(owner, agentConfig, sessions, "cli-artifacts", nil, nil)
	require.NoError(t, err)
	run, err := adkrunner.New(config)
	require.NoError(t, err)
	invoke := func(runner *adkrunner.Runner, id string) error {
		for _, err := range runner.Run(t.Context(), "alex", id, genai.NewContentFromText("view", "user"), adkagent.RunConfig{}) {
			if err != nil {
				return err
			}
		}
		return nil
	}
	require.NoError(t, invoke(run, "mine"))
	require.Equal(t, originals, seen)
	require.Equal(t, int32(1), captures.Load())
	phase = "followup"
	require.NoError(t, invoke(run, "mine"))
	require.Equal(t, originals, seen)
	stored, err := sessions.Get(t.Context(), &session.GetRequest{AppName: "cli-artifacts", UserID: "alex", SessionID: "mine"})
	require.NoError(t, err)
	for event := range stored.Session.Events().All() {
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		require.NotContains(t, string(raw), base64.StdEncoding.EncodeToString(pixelPNG))
		require.NotContains(t, string(raw), "inlineData")
	}
	phase = "foreign"
	require.Error(t, invoke(run, "foreign"))
	require.Empty(t, seen)
	config.ArtifactService = nil
	// A replacement store models the explicit artifact loss of actor restart.
	config.ArtifactService = artifact.InMemoryService()
	restarted, err := adkrunner.New(config)
	require.NoError(t, err)
	phase = "restart"
	err = invoke(restarted, "mine")
	require.ErrorContains(t, err, "failed to load artifact screenshot-")
	require.Empty(t, seen)
}

func TestCLIRejectsTruncatedImagesBeforeSavingAnyBatchMember(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	t.Setenv("TMPDIR", t.TempDir())
	var jpegData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil))
	corruptedPNG := bytes.Clone(pixelPNG)
	corruptedPNG[45] ^= 0xff
	for _, testcase := range []struct {
		name, mime string
		data       []byte
	}{
		{"PNG header only", "image/png", pixelPNG[:33]},
		{"PNG corrupt data", "image/png", corruptedPNG},
		{"JPEG missing EOI", "image/jpeg", jpegData.Bytes()[:jpegData.Len()-2]},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			_, _, err := image.DecodeConfig(bytes.NewReader(testcase.data))
			require.NoError(t, err, "regression must retain a valid image header")
			_, _, err = image.Decode(bytes.NewReader(testcase.data))
			require.Error(t, err, "standard decoder independently confirms invalid payload")
			var captures atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "capture", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				captures.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: pixelPNG}, &mcp.ImageContent{MIMEType: testcase.mime, Data: testcase.data}}}, nil
			})
			remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
			t.Cleanup(remote.Close)
			var mu sync.Mutex
			var result string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				last := input.Messages[len(input.Messages)-1]
				if last.Role == "tool" {
					mu.Lock()
					err := json.Unmarshal(last.Content, &result)
					mu.Unlock()
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = fmt.Fprint(w, `{"id":"done","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"id":"capture","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"capture-call","type":"function","function":{"name":"bash","arguments":"{\"command\":\"browser capture\"}"}}]},"finish_reason":"tool_calls"}]}`)
			}))
			t.Cleanup(peer.Close)
			owner, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			sessions := session.InMemoryService()
			_, err = sessions.Create(t.Context(), &session.CreateRequest{AppName: "invalid-cli-images", UserID: "alex", SessionID: testcase.name})
			require.NoError(t, err)
			config, err := runtimeRunner.CreateRunnerConfig(owner, &adk.AgentConfig{Model: &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}, BaseUrl: peer.URL}, CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"capture"}}}}}, sessions, "invalid-cli-images", nil, nil)
			require.NoError(t, err)
			run, err := adkrunner.New(config)
			require.NoError(t, err)
			for _, err := range run.Run(t.Context(), "alex", testcase.name, genai.NewContentFromText("capture", "user"), adkagent.RunConfig{}) {
				require.NoError(t, err)
			}
			mu.Lock()
			observed := result
			mu.Unlock()
			require.Contains(t, observed, "invalid MCP", "invalid image must reach the model as an honest failure")
			require.Contains(t, observed, "operation not replayed")
			require.NotContains(t, observed, "Screenshot artifact")
			require.Equal(t, int32(1), captures.Load(), "image validation cannot replay the capture")
			listed, err := config.ArtifactService.List(t.Context(), &artifact.ListRequest{AppName: "invalid-cli-images", UserID: "alex", SessionID: testcase.name})
			require.NoError(t, err)
			require.Empty(t, listed.FileNames, "a later invalid image rejects the whole batch before the first Save")
		})
	}
}
