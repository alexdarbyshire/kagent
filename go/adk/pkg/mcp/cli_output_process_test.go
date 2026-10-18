package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestCLIProcessOutputFile(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "output", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "result", InputSchema: json.RawMessage(`{"type":"object","properties":{"mode":{"type":"string"},"output-file":{"type":"string"},"query":{"type":"object","properties":{"output-file":{"type":"string"}}}},"required":["mode"]}`)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		var args struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
			return nil, err
		}
		text := "PRIVATE_OUTPUT_MARKER"
		if args.Mode == "large-error" {
			text = strings.Repeat("x", cliOutputLimit)
		}
		if args.Mode == "write-error" {
			text = strings.Repeat("x", 4096)
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}, StructuredContent: json.RawMessage(`{"exact":9007199254740993,"url":"https://example.invalid/no-fetch","base64":"AP8="}`), IsError: strings.Contains(args.Mode, "error")}, nil
	})
	const wireResult = `{"content":[{"type":"text","text":"PRIVATE_OUTPUT_MARKER"}],"structuredContent":{"exact":9007199254740993,"decimal":1.234567890123456789,"tiny":1e-400,"url":"https://example.invalid/no-fetch"},"_meta":{"negative":-9007199254740993},"extension":{"preserved":true}}`
	const duplicateResult = `{"content":[],"extension":{"same":1,"same":2},"_meta":{"same":1,"same":2}}`
	const emptyResult = `{"content":[]}`
	const helpResult = `{"content":[{"type":"text","text":"output: MCP tools\nUsage: output TOOL --help"}]}`
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Arguments struct {
						Mode string `json:"mode"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
				return
			}
			mode := request.Params.Arguments.Mode
			if request.Method == "tools/call" && (strings.HasPrefix(mode, "wire") || mode == "disconnect") {
				calls.Add(1)
				if mode == "disconnect" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if err := connection.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				result := wireResult
				if mode == "wire-duplicate" {
					result = duplicateResult
				}
				if mode == "wire-empty" {
					result = emptyResult
				}
				if mode == "wire-help" {
					result = helpResult
				}
				if mode == "wire-future" {
					result = `{"resultType":"future",` + result[1:]
				}
				if mode == "wire-input-required" {
					result = `{"resultType":"input_required",` + result[1:]
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result); err != nil {
					t.Error(err)
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	binding := filepath.Join(t.TempDir(), "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"output","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	env, cleanup, err := PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{{Name: "output", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}}, false, nil)(cliSessionContext{Context: t.Context(), id: "output"}, t.Context())
	require.NoError(t, err)
	defer cleanup()
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				args          []string
				errorContains string
				calls         int32
				saved         bool
				status        int
				stdout        string
				exactSaved    string
			}{
				{name: "stdout opaque duplicate fields", args: []string{"result", "--mode=wire-duplicate"}, calls: 1, stdout: duplicateResult + "\n"},
				{name: "save opaque duplicate fields", args: []string{"result", "--mode=wire-duplicate", "--output-file=result.json"}, calls: 1, saved: true, exactSaved: duplicateResult + "\n"},
				{name: "stdout exact", args: []string{"result", "--mode=wire"}, calls: 1, stdout: wireResult + "\n"},
				{name: "stdout empty", args: []string{"result", "--mode=wire-empty"}, calls: 1, stdout: emptyResult + "\n"},
				{name: "stdout looks like help", args: []string{"result", "--mode=wire-help"}, calls: 1, stdout: helpResult + "\n"},
				{name: "save empty", args: []string{"result", "--mode=wire-empty", "--output-file=result.json"}, calls: 1, saved: true, exactSaved: emptyResult + "\n"},
				{name: "save looks like help", args: []string{"result", "--mode=wire-help", "--output-file=result.json"}, calls: 1, saved: true, exactSaved: helpResult + "\n"},
				{name: "stdout future", args: []string{"result", "--mode=wire-future"}, calls: 1, errorContains: "unsupported", stdout: `{"resultType":"future",` + wireResult[1:] + "\n"},
				{name: "save", args: []string{"result", "--mode=ok", "--output-file=result.json"}, calls: 1, saved: true},
				{name: "wire exact", args: []string{"result", "--mode=wire", "--output-file=result.json"}, calls: 1, saved: true},
				{name: "wire future", args: []string{"result", "--mode=wire-future", "--output-file=result.json"}, errorContains: "unsupported", calls: 1, saved: true},
				{name: "wire input required", args: []string{"result", "--mode=wire-input-required", "--output-file=result.json"}, errorContains: "unsupported", calls: 1, saved: true},
				{name: "lost response removed without replay", args: []string{"result", "--mode=disconnect", "--output-file=result.json"}, errorContains: "not retried", calls: 1},
				{name: "tool error retained", args: []string{"result", "--output-file", "result.json", "--mode=error"}, errorContains: "returned isError", calls: 1, saved: true},
				{name: "input file", args: []string{"result", "--input-file=request.json", "--output-file=result.json"}, calls: 1, saved: true},
				{name: "text input", args: []string{"result", "--text-file=mode=mode.txt", "--output-file=result.json"}, calls: 1, saved: true},
				{name: "reserved child", args: []string{"result", "--mode=ok", "--query.output-file=literal", "--output-file=result.json"}, calls: 1, saved: true},
				{name: "existing file", args: []string{"result", "--mode=ok", "--output-file=exists"}, errorContains: "tool not called"},
				{name: "dangling symlink", args: []string{"result", "--mode=ok", "--output-file=link"}, errorContains: "tool not called"},
				{name: "directory", args: []string{"result", "--mode=ok", "--output-file=."}, errorContains: "tool not called"},
				{name: "fifo", args: []string{"result", "--mode=ok", "--output-file=fifo"}, errorContains: "tool not called"},
				{name: "missing directory", args: []string{"result", "--mode=ok", "--output-file=missing/result.json"}, errorContains: "tool not called"},
				{name: "invalid input cleans up", status: 2, args: []string{"result", "--output-file=result.json"}, errorContains: "invalid arguments"},
				{name: "excluded tool cleans up", status: 2, args: []string{"unknown", "--output-file=result.json"}, errorContains: "unknown or excluded"},
				{name: "duplicate", status: 2, args: []string{"result", "--output-file=result.json", "--output-file=other"}, errorContains: "duplicate --output-file"},
				{name: "empty", status: 2, args: []string{"result", "--output-file="}, errorContains: "requires a new file path"},
				{name: "dash", status: 2, args: []string{"result", "--output-file=-"}, errorContains: "requires a new file path"},
				{name: "missing value", status: 2, args: []string{"result", "--output-file"}, errorContains: "missing value"},
				{name: "help refuses save", status: 2, args: []string{"result", "--output-file=result.json", "--help"}, errorContains: "--help must be used alone"},
				{name: "size and tool failures both reported", args: []string{"result", "--mode=large-error", "--output-file=result.json"}, errorContains: "returned isError", calls: 1},
				{name: "filesystem and tool failures both reported", args: []string{"result", "--mode=write-error", "--output-file=result.json"}, errorContains: "returned isError", calls: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					directory := t.TempDir()
					require.NoError(t, os.WriteFile(filepath.Join(directory, "exists"), []byte("untouched"), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "request.json"), []byte(`{"mode":"ok","output-file":"literal"}`), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "mode.txt"), []byte("ok"), 0600))
					require.NoError(t, os.Symlink("result.json", filepath.Join(directory, "link")))
					require.NoError(t, syscall.Mkfifo(filepath.Join(directory, "fifo"), 0600))
					before := calls.Load()
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					command := exec.CommandContext(ctx, binary, append([]string{"--binding-file", binding}, tc.args...)...)
					if tc.name == "filesystem and tool failures both reported" {
						// Limit only this subprocess so the write fails after dispatch.
						command = exec.CommandContext(ctx, "bash", append([]string{"-c", `ulimit -f 1; exec "$@"`, "output-test", binary, "--binding-file", binding}, tc.args...)...)
					}
					command.Dir = directory
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout = &stdout
					command.Stderr = &stderr
					err := command.Run()
					if tc.errorContains != "" {
						var exit *exec.ExitError
						require.ErrorAs(t, err, &exit)
						status := tc.status
						if status == 0 {
							status = 1
						}
						require.Equal(t, status, exit.ExitCode(), "%s", stderr.String())
						require.Contains(t, stderr.String(), tc.errorContains)
					} else {
						require.NoError(t, err, "%s", stderr.String())
					}
					require.Equal(t, before+tc.calls, calls.Load())
					require.Equal(t, tc.stdout, stdout.String())
					require.NotContains(t, stderr.String(), "PRIVATE_OUTPUT_MARKER")
					if tc.name == "size and tool failures both reported" {
						require.Contains(t, stderr.String(), "32 MiB")
						require.Contains(t, stderr.String(), "not replayed")
					}
					if tc.name == "filesystem and tool failures both reported" {
						require.Contains(t, stderr.String(), "file too large")
						require.Contains(t, stderr.String(), "not replayed")
					}
					saved, err := os.ReadFile(filepath.Join(directory, "result.json"))
					if tc.saved {
						require.NoError(t, err)
						if tc.exactSaved != "" {
							require.Equal(t, tc.exactSaved, string(saved))
						} else {
							require.Contains(t, string(saved), "9007199254740993")
							require.Contains(t, string(saved), "PRIVATE_OUTPUT_MARKER")
							require.Contains(t, string(saved), "https://example.invalid/no-fetch")
							if strings.HasPrefix(tc.name, "wire") {
								expected := wireResult
								if tc.name == "wire future" {
									expected = `{"resultType":"future",` + expected[1:]
								}
								if tc.name == "wire input required" {
									expected = `{"resultType":"input_required",` + expected[1:]
								}
								require.Equal(t, expected+"\n", string(saved))
							}
						}
						info, err := os.Stat(filepath.Join(directory, "result.json"))
						require.NoError(t, err)
						require.Equal(t, os.FileMode(0600), info.Mode().Perm())
					} else {
						require.ErrorIs(t, err, os.ErrNotExist)
					}
					existing, err := os.ReadFile(filepath.Join(directory, "exists"))
					require.NoError(t, err)
					require.Equal(t, "untouched", string(existing))
				})
			}
		})
	}
}
