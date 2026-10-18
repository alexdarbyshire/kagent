package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestCLIProcessLiveCommandContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", build)
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			var calls, initializes atomic.Int32
			received := make(chan json.RawMessage, 8)
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "live", Version: "1"}, nil)
			setSchema := func(schema string) {
				server.AddTool(&mcpsdk.Tool{Name: "record", InputSchema: json.RawMessage(schema)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					received <- bytes.Clone(request.Params.Arguments)
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
				})
			}
			advertise := func(name string) {
				setSchema(`{"type":"object","properties":{"body":{"type":"object","properties":{"` + name + `":{"type":"string"},"binary":{"type":"string"},"count":{"type":"integer"}}}},"additionalProperties":false}`)
			}
			advertise("old")
			handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					data, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					r.Body = io.NopCloser(bytes.NewReader(data))
					var request struct {
						Method string `json:"method"`
					}
					require.NoError(t, json.Unmarshal(data, &request))
					if request.Method == "initialize" {
						initializes.Add(1)
					}
					if request.Method == "tools/call" {
						calls.Add(1)
					}
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(remote.Close)
			dir := t.TempDir()
			binding := filepath.Join(dir, "binding.json")
			require.NoError(t, os.WriteFile(binding, []byte(`{"name":"live","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
			var env []string
			if mode == "managed" {
				owner, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				var cleanup func()
				env, cleanup, err = PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{{Name: "live", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}}, false, nil)(cliSessionContext{Context: t.Context(), id: "live"}, t.Context())
				require.NoError(t, err)
				t.Cleanup(cleanup)
			}
			run := func(t *testing.T, status int, delta int32, args ...string) string {
				t.Helper()
				before := calls.Load()
				cmd := exec.CommandContext(t.Context(), binary, append([]string{"--binding-file", binding, "record"}, args...)...)
				cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
				cmd.Stdin = strings.NewReader(`{"body":{"fresh":"stdin","count":9007199254740993}}`)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if status == 0 {
					require.NoError(t, err, "%s", stderr.String())
					require.Empty(t, stderr.String())
				} else {
					require.Error(t, err)
					require.Empty(t, stdout.String())
					require.NotEmpty(t, stderr.String())
				}
				require.Equal(t, status, cmd.ProcessState.ExitCode(), "%s", stderr.String())
				require.Equal(t, before+delta, calls.Load())
				return stdout.String()
			}
			require.Contains(t, run(t, 0, 0, "--help"), "--old <literal text>")
			run(t, 0, 1, "--old", "--input-file=literal")
			require.JSONEq(t, `{"body":{"old":"--input-file=literal"}}`, string(<-received))
			advertise("fresh")
			help := run(t, 0, 0, "--help")
			require.Contains(t, help, "--fresh <literal text>")
			require.NotContains(t, help, "--old <")
			run(t, 2, 0, "--old=retired")
			run(t, 0, 1, "--fresh=--output-file=literal", "--count", "9007199254740993")
			got := string(<-received)
			require.JSONEq(t, `{"body":{"fresh":"--output-file=literal","count":9007199254740993}}`, got)
			require.Contains(t, got, "9007199254740993")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "text=one"), []byte("literal\ntext\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "bytes=two"), []byte{0, 255, 1}, 0600))
			require.Empty(t, run(t, 0, 1, "--text-file", "fresh=text=one", "--base64-file=binary=bytes=two", "--output-file=result=one"))
			require.JSONEq(t, `{"body":{"fresh":"literal\ntext\n","binary":"AP8B"}}`, string(<-received))
			data, err := os.ReadFile(filepath.Join(dir, "result=one"))
			require.NoError(t, err)
			require.JSONEq(t, `{"content":[]}`, string(data))
			require.Empty(t, run(t, 0, 1, "--input-file", "-", "--output-file=result=two"))
			require.Contains(t, string(<-received), "9007199254740993")
			run(t, 2, 0, "--input-file=-", "--text-file=fresh=text=one")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "whole=input"), []byte(`{"body":{"fresh":"file"}}`), 0600))
			require.Empty(t, run(t, 0, 1, "--input-file=whole=input", "--output-file", "result=three"))
			require.JSONEq(t, `{"body":{"fresh":"file"}}`, string(<-received))
			const conditional = `{"type":"object","properties":{"body":{"type":"object","properties":{"mode":{"type":"string"}},"required":["mode"],"if":{"properties":{"mode":{"const":"text"}}},"then":{"properties":{"value":{"type":"string"},"detail":{"type":"string"}},"required":["value","detail"],"not":{"required":["count"]}},"else":{"properties":{"value":{"type":"integer"},"count":{"type":"integer"}},"required":["value","count"],"not":{"required":["detail"]}}}},"required":["body"]}`
			for _, tc := range []struct {
				name, schema, help, want string
				args                     []string
				invalid                  bool
			}{
				{"conditional literal", conditional, "--value <JSON or literal text>", `{"body":{"mode":"text","value":"literal","detail":"branch"}}`, []string{"--mode=text", "--value=literal", "--detail=branch"}, false},
				{"conditional exact integer", conditional, "--count <JSON>", `{"body":{"mode":"number","value":9007199254740993,"count":2}}`, []string{"--mode=number", "--value=9007199254740993", "--count=2"}, false},
				{"conditional required", conditional, "--detail <literal text>", "", []string{"--mode=text", "--value=literal"}, true},
				{"conditional prohibited", conditional, "--count <JSON>", "", []string{"--mode=text", "--value=literal", "--detail=branch", "--count=2"}, true},
				{"branch type not reinterpreted", conditional, "--value <JSON or literal text>", "", []string{"--mode=text", "--value=4", "--detail=branch"}, true},
				{"optional parent and documented default", `{"type":"object","properties":{"body":{"type":"object","properties":{"value":{"type":"integer","default":9007199254740993}},"required":["value"]}}}`, "Default (documentation only): 9007199254740993", `{}`, nil, false},
				{"local refs", `{"type":"object","properties":{"body":{"$ref":"#/$defs/body"}},"$defs":{"body":{"type":"object","properties":{"text":{"$ref":"#/$defs/text"}}},"text":{"type":"string"}}}`, "--text <literal text>", `{"body":{"text":"literal"}}`, []string{"--text=literal"}, false},
				{"cyclic schema fallback", `{"type":"object","properties":{"body":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"},"text":{"type":"string"}}}}}`, "--next <JSON>", `{"body":{"next":{"text":"nested"}}}`, []string{`--next={"text":"nested"}`}, false},
				{"strict duplicate nested JSON", `{"type":"object","properties":{"value":{"type":["string","object"]}}}`, "--value <JSON or literal text>", "", []string{`--value={"a":1,"a":2}`}, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					setSchema(tc.schema)
					require.Contains(t, run(t, 0, 0, "--help"), tc.help)
					require.JSONEq(t, tc.schema, run(t, 0, 0, "--help", "schema"))
					if tc.invalid {
						run(t, 2, 0, tc.args...)
						return
					}
					run(t, 0, 1, tc.args...)
					got := string(<-received)
					require.JSONEq(t, tc.want, got)
					if strings.Contains(tc.want, "9007199254740993") {
						require.Contains(t, got, "9007199254740993")
					}
				})
			}
			if mode == "managed" {
				require.Equal(t, int32(1), initializes.Load(), "commands retain one relationship while refreshing schema")
			}
		})
	}
}
