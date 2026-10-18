package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestCLIProcessUsageStatus(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", build)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "outcomes", Version: "1"}, nil)
	const schema = `{"type":"object","properties":{"value":{"type":"integer","default":9007199254740993},"text":{"type":"string","description":"Text description"},"group":{"type":"object","properties":{"child":{"type":"string"}}}},"required":["value"]}`
	server.AddTool(&mcpsdk.Tool{Name: "echo", Description: "Echo fixture description", InputSchema: json.RawMessage(schema)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	server.AddTool(&mcpsdk.Tool{Name: "excluded", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	var remoteFailure atomic.Int32
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			r.Body = io.NopCloser(bytes.NewReader(data))
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			require.NoError(t, json.Unmarshal(data, &request))
			if request.Method == "tools/call" {
				calls.Add(1)
			}
			if request.Method == "tools/list" && remoteFailure.Load() != 0 {
				switch remoteFailure.Load() {
				case 1:
					w.Header().Set("Content-Type", "application/json")
					_, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo","inputSchema":{"type":"invalid"}}]}}`, request.ID)
					require.NoError(t, err)
				case 2:
					w.WriteHeader(http.StatusServiceUnavailable)
				case 3:
					w.WriteHeader(http.StatusUnauthorized)
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	binding := filepath.Join(t.TempDir(), "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"outcomes","http":{"params":{"url":"`+remote.URL+`"},"tools":["echo"]}}`), 0600))
	owner, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	env, cleanup, err := PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{{Name: "outcomes", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}, Tools: []string{"echo"}}}}, false, nil)(cliSessionContext{Context: t.Context(), id: "outcomes"}, t.Context())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				args       []string
				status     int
				diagnostic string
			}{
				{"excluded tool", []string{"excluded"}, 2, "unknown or excluded tool"},
				{"unknown tool", []string{"unknown"}, 2, "unknown or excluded tool"},
				{"empty tool", []string{""}, 2, "unknown or excluded tool"},
				{"unknown flag", []string{"echo", "--unknown=x"}, 2, "unknown argument"},
				{"missing value", []string{"echo", "--value"}, 2, "missing value"},
				{"full schema validation", []string{"echo", "--value=1.5"}, 2, "invalid arguments"},
				{"required", []string{"echo"}, 2, "invalid arguments"},
				{"invalid JSON", []string{"echo", "--value={"}, 2, "JSON"},
				{"duplicate JSON keys", []string{"echo", "--input-file=-"}, 2, "duplicate JSON key"},
				{"duplicate assignment", []string{"echo", "--value=1", "--value=2"}, 2, "duplicate"},
				{"conflicting modes", []string{"echo", "--value=1", "--input-file=missing"}, 2, "cannot be combined"},
				{"file syntax", []string{"echo", "--text-file=text"}, 2, "NAME=PATH"},
				{"help with input", []string{"echo", "--help", "--text-file=text=missing"}, 2, "--help must be used alone"},
				{"help with output", []string{"echo", "--help", "--output-file=missing/result"}, 2, "--help must be used alone"},
				{"dash output", []string{"echo", "--output-file=-"}, 2, "new file path"},
				{"empty output", []string{"echo", "--output-file="}, 2, "new file path"},
				{"missing JSON input", []string{"echo", "--input-file=missing"}, 1, "failed to read"},
				{"missing text input", []string{"echo", "--text-file=text=missing"}, 1, "failed to read"},
				{"nonregular JSON input", []string{"echo", "--input-file=."}, 1, "regular file"},
				{"nonregular text input", []string{"echo", "--text-file=text=."}, 1, "regular file"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					command := exec.CommandContext(t.Context(), binary, append([]string{"--binding-file", binding}, tc.args...)...)
					command.Dir = t.TempDir()
					command.Stdin = strings.NewReader(`{"value":1,"value":2}`)
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout, command.Stderr = &stdout, &stderr
					err := command.Run()
					var exit *exec.ExitError
					require.ErrorAs(t, err, &exit)
					require.Equal(t, tc.status, exit.ExitCode(), "%s", stderr.String())
					require.Contains(t, stderr.String(), tc.diagnostic)
					require.Empty(t, stdout.String())
					require.Zero(t, calls.Load())
				})
			}
			for _, tc := range []struct {
				name string
				args []string
			}{
				{"bare catalog", nil}, {"catalog help", []string{"--help"}},
				{"operation help", []string{"echo", "--help"}}, {"schema help", []string{"echo", "--help", "schema"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					command := exec.CommandContext(t.Context(), binary, append([]string{"--binding-file", binding}, tc.args...)...)
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout, command.Stderr = &stdout, &stderr
					require.NoError(t, command.Run(), "%s", stderr.String())
					require.Zero(t, command.ProcessState.ExitCode())
					require.Zero(t, calls.Load())
					if tc.name == "schema help" {
						require.Equal(t, schema+"\n", stdout.String())
					} else {
						require.Contains(t, stdout.String(), "Echo fixture description")
						require.NotContains(t, stdout.String(), "excluded")
						require.Contains(t, stdout.String(), "Exit status: 0")
						require.Contains(t, stdout.String(), "2 for invalid usage")
						require.Contains(t, stdout.String(), "Exit 1 never authorizes a retry")
					}
					if tc.name == "operation help" {
						for _, text := range []string{"--value <JSON> (integer; required)", "--text <literal text>", "--child <literal text>", "Text description", "Default (documentation only): 9007199254740993", "--help schema", "--input-file", "--base64-file NAME=PATH", "--text-file NAME=PATH", "8 MiB", "16 MiB", "24 MiB", "--output-file PATH", "32 MiB", "--save-text NAME=PATH", "--save-base64 NAME=PATH", "--verify-sha256 NAME", "content.resource.blob"} {
							require.Contains(t, stdout.String(), text)
						}
					}
				})
			}
			for _, failure := range []int32{1, 2, 3} {
				t.Run(fmt.Sprintf("remote operational failure %d", failure), func(t *testing.T) {
					remoteFailure.Store(failure)
					defer remoteFailure.Store(0)
					command := exec.CommandContext(t.Context(), binary, "--binding-file", binding, "echo", "--value=1")
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout, command.Stderr = &stdout, &stderr
					var exit *exec.ExitError
					require.ErrorAs(t, command.Run(), &exit)
					require.Equal(t, 1, exit.ExitCode(), "%s", stderr.String())
					require.Empty(t, stdout.String())
					require.NotEmpty(t, stderr.String())
					require.Zero(t, calls.Load())
					if failure == 1 {
						require.Contains(t, stderr.String(), "unsupported tool schema")
					}
				})
			}
			for _, data := range []string{`{"name":"outcomes","unknown":true}`, `{"name":`, `{}`} {
				t.Run("malformed private binding "+data, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "binding.json")
					require.NoError(t, os.WriteFile(path, []byte(data), 0600))
					command := exec.CommandContext(t.Context(), binary, "--binding-file", path, "echo", "--value=1")
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout, command.Stderr = &stdout, &stderr
					var exit *exec.ExitError
					require.ErrorAs(t, command.Run(), &exit)
					require.Equal(t, 1, exit.ExitCode(), "%s", stderr.String())
					require.Empty(t, stdout.String())
					require.NotEmpty(t, stderr.String())
					require.Zero(t, calls.Load())
				})
			}
		})
	}
}

func TestCLIProcessValidatesPrivateOutcomes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", build)
	directory, err := os.MkdirTemp("", "mcp-outcome-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	binding := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"outcomes"}`), 0600))
	for _, tc := range []struct {
		name     string
		response string
		args     []string
		wantHelp string
	}{
		{"empty catalog", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[]}}`, []string{"--help"}, "outcomes: MCP tools"},
		{"null catalog rows", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":null}}`, []string{"--help"}, "outcomes: MCP tools"},
		{"catalog without description", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{"name":"echo"}]}}`, []string{"--help"}, "outcomes: MCP tools"},
		{"catalog empty description", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{"name":"echo","description":""}]}}`, []string{"--help"}, "outcomes: MCP tools"},
		{"empty operation", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[]}}`, []string{"echo", "--help"}, "outcomes echo:"},
		{"null parameter rows", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":null}}`, []string{"echo", "--help"}, "outcomes echo:"},
		{"JSON", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[{"name":"value","mode":"JSON","types":["schema-defined"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, "--value <JSON> (schema-defined; optional)"},
		{"literal text", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[{"name":"value","mode":"literal text","types":["schema-defined"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, "--value <literal text> (schema-defined; optional)"},
		{"JSON or literal text", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[{"name":"value","mode":"JSON or literal text","types":["schema-defined"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, "--value <JSON or literal text> (schema-defined; optional)"},
		{"empty optional annotations", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[{"name":"value","mode":"JSON","types":["string"],"requirement":"optional","descriptions":[],"defaults":[]}]}}`, []string{"echo", "--help"}, "--value <JSON> (string; optional)"},
		{"empty optional annotation entries", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","description":"","parameters":[{"name":"value","mode":"JSON","types":["string"],"requirement":"optional","descriptions":[""],"defaults":[""]}]}}`, []string{"echo", "--help"}, "--value <JSON> (string; optional)"},
		{"empty catalog row", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{}]}}`, []string{"--help"}, ""},
		{"null catalog row", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[null]}}`, []string{"--help"}, ""},
		{"empty tool name", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{"name":""}]}}`, []string{"--help"}, ""},
		{"null tool name", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{"name":null}]}}`, []string{"--help"}, ""},
		{"empty parameter row", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{}]}}`, []string{"echo", "--help"}, ""},
		{"null parameter row", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[null]}}`, []string{"echo", "--help"}, ""},
		{"name missing", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"mode":"JSON","types":["string"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"name empty", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"","mode":"JSON","types":["string"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"mode missing", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","types":["string"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"mode empty", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"","types":["string"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"mode unsupported", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"bogus","types":["string"],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"types missing", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"types empty", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","types":[],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"types empty entry", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","types":[""],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"types null entry", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","types":[null],"requirement":"optional"}]}}`, []string{"echo", "--help"}, ""},
		{"requirement missing", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","types":["string"]}]}}`, []string{"echo", "--help"}, ""},
		{"requirement empty", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","types":["string"],"requirement":""}]}}`, []string{"echo", "--help"}, ""},
		{"missing kind", `{}`, nil, ""},
		{"unknown kind", `{"kind":"future","result":{"content":[]}}`, nil, ""},
		{"empty result", `{"kind":"result"}`, nil, ""},
		{"null result", `{"kind":"result","result":null}`, nil, ""},
		{"non-envelope result", `{"kind":"result","result":"help"}`, nil, ""},
		{"result usage failure", `{"kind":"result","result":{"content":[]},"failure":{"category":"usage","diagnostic":"bad flags"}}`, nil, ""},
		{"missing failure", `{"kind":"failure"}`, nil, ""},
		{"unknown failure category", `{"kind":"failure","failure":{"category":"future","diagnostic":"failed"}}`, nil, ""},
		{"empty diagnostic", `{"kind":"failure","failure":{"category":"operational"}}`, nil, ""},
		{"failure with result", `{"kind":"failure","result":{"content":[]},"failure":{"category":"operational","diagnostic":"failed"}}`, nil, ""},
		{"help in tool call", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[]}}`, nil, ""},
		{"operation help in tool call", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo"}}`, nil, ""},
		{"result in help", `{"kind":"result","result":{"content":[]}}`, []string{"--help"}, ""},
		{"wrong command help", `{"kind":"catalog_help","catalog":{"command":"other","tools":[]}}`, []string{"--help"}, ""},
		{"wrong operation help", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"other"}}`, []string{"echo", "--help"}, ""},
		{"mixed help and result", `{"kind":"result","result":{"content":[]},"catalog":{"command":"outcomes"}}`, nil, ""},
		{"unknown field", `{"kind":"result","result":{"content":[]},"output":"help"}`, nil, ""},
		{"duplicate discriminator", `{"kind":"failure","kind":"result","result":{"content":[]}}`, nil, ""},
		{"case alias discriminator", `{"kind":"failure","Kind":"result","result":{"content":[]}}`, nil, ""},
		{"case alias failure category", `{"kind":"failure","failure":{"category":"operational","Category":"usage","diagnostic":"failed"}}`, nil, ""},
		{"case alias catalog command", `{"kind":"catalog_help","catalog":{"command":"other","Command":"outcomes"}}`, []string{"--help"}, ""},
		{"case alias tool name", `{"kind":"catalog_help","catalog":{"command":"outcomes","tools":[{"name":"excluded","Name":"echo"}]}}`, []string{"--help"}, ""},
		{"case alias parameter mode", `{"kind":"operation_help","operation":{"command":"outcomes","tool":"echo","parameters":[{"name":"value","mode":"JSON","Mode":"literal text"}]}}`, []string{"echo", "--help"}, ""},
		{"invalid schema shape", `{"kind":"schema_help","schema":[]}`, []string{"echo", "--help", "schema"}, ""},
		{"trailing data", `{"kind":"result","result":{"content":[]}} {}`, nil, ""},
		{"truncated response", `{"kind":"result","result":`, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			socket := filepath.Join(directory, "socket")
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, "/command/outcomes", r.URL.Path)
				_, err := fmt.Fprint(w, tc.response)
				require.NoError(t, err)
			}))
			require.NoError(t, server.Listener.Close())
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)
			args := tc.args
			if args == nil {
				args = []string{"echo", "--output-file=result.json", "--save-text=text=artifact"}
			}
			command := exec.CommandContext(t.Context(), binary, append([]string{"--binding-file", binding}, args...)...)
			command.Env = append(os.Environ(), cliInvocationSocket+"="+socket)
			command.Dir = t.TempDir()
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err = command.Run()
			if tc.wantHelp != "" {
				require.NoError(t, err, "%s", stderr.String())
				require.Empty(t, stderr.String())
				require.Contains(t, stdout.String(), tc.wantHelp)
			} else {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.Equal(t, 1, exit.ExitCode(), "%s", stderr.String())
				require.NotEmpty(t, stderr.String())
				require.Empty(t, stdout.String())
			}
			require.Equal(t, int32(1), requests.Load(), "private command must not replay")
			_, err = os.Stat(filepath.Join(command.Dir, "result.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(filepath.Join(command.Dir, "artifact"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCLIProcessSaveResultWithReturnedCloseError(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", build)
	directory, err := os.MkdirTemp("", "mcp-close-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	listener, err := net.Listen("unix", filepath.Join(directory, "socket"))
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		for _, local := range []string{"save-text", "output-file", "artifact", "response.json"} {
			if bytes.Contains(body, []byte(local)) {
				t.Errorf("local output intention crossed socket: %s", local)
			}
		}
		_, err = io.WriteString(w, `{"kind":"result","result":{"content":[{"type":"text","text":"complete"}]},"failure":{"category":"operational","diagnostic":"returned close failure"}}`)
		if err != nil {
			t.Error(err)
		}
	}))
	require.NoError(t, server.Listener.Close())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	binding := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"close"}`), 0600))
	command := exec.CommandContext(t.Context(), binary, "--binding-file", binding, "echo", "--save-text=text=artifact", "--output-file=response.json")
	command.Dir = directory
	command.Env = append(os.Environ(), cliInvocationSocket+"="+filepath.Join(directory, "socket"))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	var exit *exec.ExitError
	require.ErrorAs(t, command.Run(), &exit)
	require.Equal(t, 1, exit.ExitCode())
	require.Contains(t, stderr.String(), "returned close failure")
	require.Empty(t, stdout.String())
	require.EqualValues(t, 1, requests.Load())
	artifact, err := os.ReadFile(filepath.Join(directory, "artifact"))
	require.NoError(t, err)
	require.Equal(t, "complete", string(artifact))
	raw, err := os.ReadFile(filepath.Join(directory, "response.json"))
	require.NoError(t, err)
	require.Equal(t, "{\"content\":[{\"type\":\"text\",\"text\":\"complete\"}]}\n", string(raw))
}
