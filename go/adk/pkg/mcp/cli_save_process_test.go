package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestCLIProcessSaveNamedOutput(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, envelope, target, expected string
				flag, digest, failure            string
				args                             []string
				status                           int
				noCall, noRaw, errorArtifact     bool
				request                          string
				changedPath                      string
				removeOnly                       bool
			}{
				{name: "conversion markdown", envelope: `{"content":[{"type":"text","text":"compatibility"}],"structuredContent":{"document":{"md_content":"# Heading\n\nBody\n"}}}`, target: "document.md_content", expected: "# Heading\n\nBody\n"},
				{name: "pdf output verified", envelope: fmt.Sprintf(`{"content":[],"structuredContent":{"output":{"base64_string":"AP8=","sha256":"%x"},"page_mapping":[2,1]}}`, sha256.Sum256([]byte{0, 255})), target: "base64_string", flag: "save-base64", digest: "output.sha256", expected: string([]byte{0, 255})},
				{name: "archive bytes", envelope: `{"content":[],"structuredContent":{"base64_string":"AP8=","sha256":"ignored"}}`, target: "base64_string", flag: "save-base64", expected: string([]byte{0, 255})},
				{name: "empty text", envelope: `{"content":[],"structuredContent":{"text":""}}`, target: "text"},
				{name: "empty binary", envelope: `{"content":[],"structuredContent":{"data":""}}`, target: "data", flag: "save-base64"},
				{name: "digest mismatch", envelope: `{"content":[],"structuredContent":{"data":"AP8=","sha":"0000000000000000000000000000000000000000000000000000000000000000"}}`, target: "data", flag: "save-base64", digest: "sha", failure: "SHA-256 mismatch"},
				{name: "digest malformed", envelope: `{"content":[],"structuredContent":{"data":"AP8=","sha":"short"}}`, target: "data", flag: "save-base64", digest: "sha", failure: "SHA-256"},
				{name: "digest missing", envelope: `{"content":[],"structuredContent":{"data":"AP8="}}`, target: "data", flag: "save-base64", digest: "sha", failure: "named output"},
				{name: "uppercase digest", envelope: fmt.Sprintf(`{"content":[],"structuredContent":{"data":"AP8=","sha":"%s"}}`, strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256([]byte{0, 255})))), target: "data", flag: "save-base64", digest: "sha", expected: string([]byte{0, 255})},
				{name: "base64 trailing bits", envelope: `{"content":[],"structuredContent":{"data":"AB=="}}`, target: "data", flag: "save-base64", failure: "canonical"},
				{name: "base64 whitespace", envelope: `{"content":[],"structuredContent":{"data":"AP8=\n"}}`, target: "data", flag: "save-base64", failure: "canonical"},
				{name: "base64 URL alphabet", envelope: `{"content":[],"structuredContent":{"data":"__8="}}`, target: "data", flag: "save-base64", failure: "canonical"},
				{name: "base64 missing padding", envelope: `{"content":[],"structuredContent":{"data":"AP8"}}`, target: "data", flag: "save-base64", failure: "canonical"},
				{name: "base64 invalid character", envelope: `{"content":[],"structuredContent":{"data":"AP?="}}`, target: "data", flag: "save-base64", failure: "canonical"},

				{name: "literal dotted precedence", envelope: `{"content":[],"structuredContent":{"a.b":"literal","a":{"b":"generated"}}}`, target: "a.b", expected: "literal"},
				{name: "literal short precedence", envelope: `{"content":[],"structuredContent":{"b":"literal","a":{"b":"generated"}}}`, target: "b", expected: "literal"},
				{name: "ambiguous short", envelope: `{"content":[],"structuredContent":{"a":{"b":"one"},"c":{"b":"two"}}}`, target: "b", failure: "named output"},
				{name: "ambiguous dotted projection", envelope: `{"content":[],"structuredContent":{"a.b":{"c":"one"},"a":{"b.c":"two"}}}`, target: "a.b.c", failure: "named output"},
				{name: "qualified survives short ambiguity", envelope: `{"content":[],"structuredContent":{"a":{"b":"one"},"c":{"b":"two"}}}`, target: "a.b", expected: "one"},
				{name: "case and punctuation", envelope: `{"content":[],"structuredContent":{"My-Group":{"some_name":"exact"}}}`, target: "My-Group.some_name", expected: "exact"},
				{name: "null", envelope: `{"content":[],"structuredContent":{"data":null}}`, target: "data", failure: "one string"},
				{name: "number", envelope: `{"content":[],"structuredContent":{"data":42}}`, target: "data", failure: "one string"},
				{name: "no recursive projection", envelope: `{"content":[],"structuredContent":{"a":{"b":{"c":"value"}}}}`, target: "a.b.c", failure: "named output"},
				{name: "no array projection", envelope: `{"content":[],"structuredContent":{"images":[{"data":"AA=="}]}}`, target: "images.data", failure: "named output"},
				{name: "native image", envelope: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}]}`, target: "content.image", flag: "save-base64", expected: string([]byte{0, 255})},
				{name: "native image shortcut", envelope: `{"content":[{"type":"text","text":"ignored"},{"type":"image","data":"AP8=","mimeType":"image/png"}]}`, target: "image", flag: "save-base64", expected: string([]byte{0, 255})},
				{name: "native text JSON is literal", envelope: `{"content":[{"type":"text","text":"{\"data\":\"literal\"}"}]}`, target: "text", expected: `{"data":"literal"}`},
				{name: "native text empty", envelope: `{"content":[{"type":"text","text":""}]}`, target: "content.text"},
				{name: "native resource text", envelope: `{"content":[{"type":"resource","resource":{"uri":"test://resource","text":"resource text"}}]}`, target: "content.resource.text", expected: "resource text"},
				{name: "native resource blob", envelope: `{"content":[{"type":"resource","resource":{"uri":"test://resource","blob":"AP8="}}]}`, target: "resource.blob", flag: "save-base64", expected: string([]byte{0, 255})},
				{name: "multiple images", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"image","data":"AQ==","mimeType":"image/png"}]}`, target: "image", flag: "save-base64", failure: "one string"},
				{name: "missing text block is not skipped", envelope: `{"content":[{"type":"text"},{"type":"text","text":"valid"}]}`, target: "text", failure: "one string"},
				{name: "missing image data is not skipped", envelope: `{"content":[{"type":"image","mimeType":"image/png"},{"type":"image","data":"AA==","mimeType":"image/png"}]}`, target: "image", flag: "save-base64", failure: "one string"},

				{name: "multiple texts", envelope: `{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}`, target: "text", failure: "one string"},
				{name: "structured suppresses compatibility", envelope: `{"content":[{"type":"text","text":"compatibility"}],"structuredContent":{"data":"actual"}}`, target: "content.text", failure: "named output"},
				{name: "structured keeps image", envelope: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}],"structuredContent":{"data":"actual"}}`, target: "image", flag: "save-base64", expected: string([]byte{0, 255})},
				{name: "literal beats native canonical", envelope: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}],"structuredContent":{"content.image":"literal"}}`, target: "content.image", expected: "literal"},
				{name: "literal beats native shortcut", envelope: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}],"structuredContent":{"image":"literal"}}`, target: "image", expected: "literal"},
				{name: "native shortcut collides child", envelope: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}],"structuredContent":{"group":{"image":"child"}}}`, target: "image", failure: "named output"},
				{name: "malformed structured array", envelope: `{"content":[{"type":"text","text":"fallback"}],"structuredContent":[]}`, target: "text", failure: "structuredContent"},
				{name: "malformed structured null", envelope: `{"content":[{"type":"text","text":"fallback"}],"structuredContent":null}`, target: "text", failure: "structuredContent"},

				{name: "selected duplicate", envelope: `{"content":[],"structuredContent":{"data":"first","data":"last"}}`, target: "data", failure: "duplicate"},
				{name: "selected ancestor duplicate", envelope: `{"content":[],"structuredContent":{"output":{"data":"first"},"output":{"data":"last"}}}`, target: "output.data", failure: "duplicate"},
				{name: "selected envelope duplicate", envelope: `{"content":[],"structuredContent":{"data":"first"},"structuredContent":{"data":"last"}}`, target: "data", failure: "duplicate"},
				{name: "escaped duplicate key", envelope: `{"content":[],"structuredContent":{"d\u0061ta":"first","data":"last"}}`, target: "data", failure: "duplicate"},
				{name: "opaque unrelated duplicates", envelope: `{"content":[],"structuredContent":{"data":"retained","unused":{"same":1,"same":2,"bad":"\ud800"},"other":"first","other":"last","exact":9007199254740993,"tiny":1e-400},"_meta":{"same":1,"same":2}}`, target: "data", expected: "retained"},
				{name: "opaque sibling duplicate", envelope: `{"content":[],"structuredContent":{"group":{"data":"retained","same":1,"same":2}}}`, target: "group.data", expected: "retained"},
				{name: "high surrogate", envelope: `{"content":[],"structuredContent":{"data":"\ud800"}}`, target: "data", failure: "Unicode"},
				{name: "low surrogate", envelope: `{"content":[],"structuredContent":{"data":"\udc00"}}`, target: "data", failure: "Unicode"},
				{name: "unpaired then ordinary", envelope: `{"content":[],"structuredContent":{"data":"\ud800\u0061"}}`, target: "data", failure: "Unicode"},
				{name: "valid Unicode", envelope: `{"content":[],"structuredContent":{"data":"\ud83d\ude00\u00e9\ufffd"}}`, target: "data", expected: "\U0001f600\u00e9\ufffd"},
				{name: "escaped slash u remains literal", envelope: `{"content":[],"structuredContent":{"data":"\\ud800"}}`, target: "data", expected: `\ud800`},
				{name: "invalid UTF8", envelope: "{\"content\":[],\"structuredContent\":{\"data\":\"" + string([]byte{255}) + "\"}}", target: "data", failure: "Unicode"},
				{name: "invalid Unicode key selected", envelope: `{"content":[],"structuredContent":{"\ud800":"value"}}`, target: "\ufffd", failure: "Unicode"},
				{name: "invalid Unicode ancestor key selected", envelope: `{"content":[],"structuredContent":{"\ud800":{"data":"value"}}}`, target: "data", failure: "Unicode"},
				{name: "native duplicate selected text", envelope: `{"content":[{"type":"text","text":"first","text":"last"}]}`, target: "text", failure: "duplicate"},
				{name: "native unpaired surrogate", envelope: `{"content":[{"type":"text","text":"\ud800"}]}`, target: "text", failure: "Unicode"},
				{name: "native opaque duplicate metadata", envelope: `{"content":[{"type":"text","text":"retained","_meta":{"same":1,"same":2}}]}`, target: "text", expected: "retained"},
				{name: "digest duplicate", envelope: `{"content":[],"structuredContent":{"data":"value","sha":"first","sha":"last"}}`, target: "data", digest: "sha", failure: "duplicate"},

				{name: "save flag equals and path equals", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact=part", "--output-file=response.json"}, expected: "value"},
				{name: "whole JSON input", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact", "--output-file=response.json", "--input-file=request.json"}, expected: "value", request: `{"save-text":"literal","save-base64":"literal","verify-sha256":"literal"}`},
				{name: "stdin JSON input", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact", "--output-file=response.json", "--input-file=-"}, expected: "value", request: `{"save-text":"literal","save-base64":"literal","verify-sha256":"literal"}`},
				{name: "text file input", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact", "--output-file=response.json", "--text-file=mode=input.txt"}, expected: "value", request: `{"mode":"file value"}`},
				{name: "base64 file input", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact", "--output-file=response.json", "--base64-file=mode=input.txt"}, expected: "value", request: `{"mode":"ZmlsZSB2YWx1ZQ=="}`},
				{name: "ordinary flag input", envelope: `{"content":[],"structuredContent":{"data":"value"}}`, args: []string{"--save-text=data=artifact", "--output-file=response.json", "--mode=argument"}, expected: "value", request: `{"mode":"argument"}`},
				{name: "missing raw request", args: []string{"--save-text=data=artifact"}, status: 2, noCall: true, noRaw: true, failure: "requires --output-file"},
				{name: "duplicate saves", args: []string{"--save-text=data=artifact", "--save-base64=data=other", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "one save"},
				{name: "duplicate digests", args: []string{"--save-text=data=artifact", "--verify-sha256=sha", "--verify-sha256=other", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "verify-sha256"},
				{name: "digest without save", args: []string{"--verify-sha256=sha", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "requires a save"},
				{name: "empty name", args: []string{"--save-text==artifact", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "NAME=PATH"},
				{name: "empty path", args: []string{"--save-text=data=", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "NAME=PATH"},
				{name: "dash path", args: []string{"--save-text=data=-", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "NAME=PATH"},
				{name: "missing equals", args: []string{"--save-text=data", "--output-file=response.json"}, status: 2, noCall: true, noRaw: true, failure: "NAME=PATH"},
				{name: "help refuses save", args: []string{"--save-text=data=artifact", "--output-file=response.json", "--help"}, status: 2, noCall: true, noRaw: true, failure: "--help"},
				{name: "same destination", args: []string{"--save-text=data=response.json", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "existing artifact", args: []string{"--save-text=data=exists", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "artifact symlink", args: []string{"--save-text=data=link", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "artifact FIFO", args: []string{"--save-text=data=fifo", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "artifact directory", args: []string{"--save-text=data=.", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "artifact missing parent", args: []string{"--save-text=data=missing/artifact", "--output-file=response.json"}, noCall: true, noRaw: true, failure: "tool not called"},
				{name: "unreadable input", args: []string{"--save-text=data=artifact", "--output-file=response.json", "--input-file=missing.json"}, noCall: true, noRaw: true, failure: "failed to read"},
				{name: "invalid input cleanup", args: []string{"--save-text=data=artifact", "--output-file=response.json", "--mode=not-allowed", "--other=x"}, status: 2, noCall: true, noRaw: true, failure: "unknown argument"},
				{name: "error result saves", envelope: `{"content":[],"structuredContent":{"data":"mechanical"},"isError":true}`, target: "data", expected: "mechanical", failure: "isError", errorArtifact: true},
				{name: "unsupported result saves", envelope: `{"content":[],"structuredContent":{"data":"mechanical"},"resultType":"future"}`, target: "data", expected: "mechanical", failure: "unsupported", errorArtifact: true},
				{name: "replacement on failure retained", envelope: `{"content":[],"structuredContent":{"data":null}}`, target: "data", failure: "replacement was not removed", changedPath: "artifact"},
				{name: "artifact replacement after successful result", envelope: `{"content":[],"structuredContent":{"data":"intended bytes"}}`, target: "data", failure: "replacement was not removed", changedPath: "artifact"},
				{name: "raw replacement after successful result", envelope: `{"content":[],"structuredContent":{"data":"intended bytes"}}`, target: "data", failure: "replacement was not removed", changedPath: "response.json"},
				{name: "empty artifact replacement after successful result", envelope: `{"content":[],"structuredContent":{"data":""}}`, target: "data", failure: "replacement was not removed", changedPath: "artifact"},
				{name: "artifact removed after successful result", envelope: `{"content":[],"structuredContent":{"data":"intended bytes"}}`, target: "data", failure: "output path", changedPath: "artifact", removeOnly: true},
				{name: "raw removed after successful result", envelope: `{"content":[],"structuredContent":{"data":"intended bytes"}}`, target: "data", failure: "output path", changedPath: "response.json", removeOnly: true},

				{name: "source exact 32 MiB", envelope: `{"content":[],"structuredContent":{"data":"` + strings.Repeat("x", cliOutputLimit-len(`{"content":[],"structuredContent":{"data":""}}`)-1) + `"}}`, target: "data", expected: strings.Repeat("x", cliOutputLimit-len(`{"content":[],"structuredContent":{"data":""}}`)-1)},
				{name: "source over 32 MiB", envelope: `{"content":[],"structuredContent":{"data":"` + strings.Repeat("x", cliOutputLimit-len(`{"content":[],"structuredContent":{"data":""}}`)) + `"}}`, target: "data", failure: "32 MiB", noRaw: true},
				{name: "binary near source limit", envelope: `{"content":[],"structuredContent":{"data":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, (24<<20)-64)) + `"}}`, target: "data", flag: "save-base64", expected: string(bytes.Repeat([]byte{255}, (24<<20)-64))},
				{name: "raw I/O failure", envelope: `{"content":[],"structuredContent":{"data":"` + strings.Repeat("x", 4096) + `"}}`, target: "data", failure: "file too large", noRaw: true},

				{name: "disconnect after dispatch", target: "data", failure: "not retried", noRaw: true},
				{name: "cancel after dispatch", target: "data", failure: "context canceled", noRaw: true},
				{name: "transformation failure", envelope: `{"content":[],"structuredContent":{"data":"original"}}`, target: "data", failure: "transform rejected", noRaw: true},
				{name: "transformed image", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`, target: "image", flag: "save-base64", expected: string([]byte{0, 255})},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if mode == "standalone" && (tc.name == "transformation failure" || tc.name == "transformed image") {
						t.Skip("trusted actor transformer is installed only in managed execution")
					}
					started := make(chan struct{})
					var calls atomic.Int32
					var arguments atomic.Value
					directory := t.TempDir()
					server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "named", Version: "1"}, nil)
					server.AddTool(&mcpsdk.Tool{Name: "result", InputSchema: json.RawMessage(`{"type":"object","properties":{"mode":{"type":"string"},"save-text":{"type":"string"},"save-base64":{"type":"string"},"verify-sha256":{"type":"string"}},"additionalProperties":false}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
						panic("wire fixture handles calls")
					})
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
									Arguments json.RawMessage `json:"arguments"`
								} `json:"params"`
							}
							if err := json.Unmarshal(body, &request); err != nil {
								t.Error(err)
								return
							}
							if request.Method == "tools/call" {
								calls.Add(1)
								close(started)
								if tc.name == "disconnect after dispatch" {
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
								if tc.name == "cancel after dispatch" {
									<-r.Context().Done()
									return
								}
								arguments.Store(request.Params.Arguments)
								if tc.changedPath != "" {
									if err := os.Remove(filepath.Join(directory, tc.changedPath)); err != nil {
										t.Error(err)
									}
									if !tc.removeOnly {
										if err := os.WriteFile(filepath.Join(directory, tc.changedPath), []byte("replacement"), 0600); err != nil {
											t.Error(err)
										}
									}
								}
								w.Header().Set("Content-Type", "application/json")
								if _, err := fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, tc.envelope); err != nil {
									t.Error(err)
								}
								return
							}
						}
						handler.ServeHTTP(w, r)
					}))
					defer remote.Close()
					binding := filepath.Join(directory, "binding.json")
					require.NoError(t, os.WriteFile(binding, []byte(`{"name":"named","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
					flag := tc.flag
					if flag == "" {
						flag = "save-text"
					}
					args := []string{"--binding-file", binding, "result", "--" + flag, tc.target + "=artifact", "--output-file=response.json"}
					if tc.args != nil {
						args = append([]string{"--binding-file", binding, "result"}, tc.args...)
					}
					if tc.digest != "" {
						args = append(args, "--verify-sha256="+tc.digest)
					}
					command := exec.CommandContext(t.Context(), binary, args...)
					if tc.name == "raw I/O failure" {
						command = exec.CommandContext(t.Context(), "bash", append([]string{"-c", `ulimit -f 1; exec "$@"`, "save-test", binary}, args...)...)
					}
					command.Dir = directory
					require.NoError(t, os.WriteFile(filepath.Join(directory, "request.json"), []byte(`{"save-text":"literal","save-base64":"literal","verify-sha256":"literal"}`), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "input.txt"), []byte("file value"), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "exists"), []byte("untouched"), 0600))
					require.NoError(t, os.Symlink("absent", filepath.Join(directory, "link")))
					require.NoError(t, syscall.Mkfifo(filepath.Join(directory, "fifo"), 0600))
					command.Stdin = strings.NewReader(`{"save-text":"literal","save-base64":"literal","verify-sha256":"literal"}`)
					if mode == "managed" {
						owner, cancel := context.WithCancel(t.Context())
						defer cancel()
						if tc.name == "transformation failure" || tc.name == "transformed image" {
							owner = WithResultTransformer(owner, func(context.Context, json.RawMessage) (json.RawMessage, error) {
								if tc.name == "transformation failure" {
									return nil, errors.New("transform rejected")
								}
								return json.RawMessage(`{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}]}`), nil
							})
						}
						env, cleanup, err := PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{{Name: "named", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}}, false, nil)(cliSessionContext{Context: t.Context(), id: "named"}, t.Context())
						require.NoError(t, err)
						defer cleanup()
						command.Env = append(os.Environ(), env...)
					}
					var stdout, stderr bytes.Buffer
					command.Stdout, command.Stderr = &stdout, &stderr
					var runErr error
					if tc.name == "cancel after dispatch" {
						require.NoError(t, command.Start())
						<-started
						require.NoError(t, command.Process.Signal(os.Interrupt))
						runErr = command.Wait()
					} else {
						runErr = command.Run()
					}
					if tc.failure == "" {
						require.NoError(t, runErr, "%s", stderr.String())
					} else {
						var exit *exec.ExitError
						require.ErrorAs(t, runErr, &exit)
						status := tc.status
						if status == 0 {
							status = 1
						}
						require.Equal(t, status, exit.ExitCode(), "%s", stderr.String())
						require.Contains(t, stderr.String(), tc.failure)
					}
					require.Empty(t, stdout.String())
					expectedCalls := int32(1)
					if tc.noCall {
						expectedCalls = 0
					}
					require.Equal(t, expectedCalls, calls.Load())
					if tc.request != "" {
						require.JSONEq(t, tc.request, string(arguments.Load().(json.RawMessage)))
					}
					require.Less(t, stderr.Len(), 2048)
					require.NotContains(t, stderr.String(), "PRIVATE_OUTPUT_MARKER")
					artifactPath := filepath.Join(directory, "artifact")
					if tc.name == "save flag equals and path equals" {
						artifactPath += "=part"
					}
					artifact, err := os.ReadFile(artifactPath)
					if tc.changedPath == "artifact" && !tc.removeOnly {
						require.NoError(t, err)
						require.Equal(t, "replacement", string(artifact))
					} else if tc.failure != "" && !tc.errorArtifact {
						require.ErrorIs(t, err, os.ErrNotExist)
					} else {
						require.NoError(t, err)
						require.Equal(t, tc.expected, string(artifact))
						info, err := os.Stat(artifactPath)
						require.NoError(t, err)
						require.Equal(t, os.FileMode(0600), info.Mode().Perm())
					}
					raw, err := os.ReadFile(filepath.Join(directory, "response.json"))
					if tc.changedPath == "response.json" {
						if tc.removeOnly {
							require.ErrorIs(t, err, os.ErrNotExist)
						} else {
							require.NoError(t, err)
							require.Equal(t, "replacement", string(raw))
						}
						return
					}
					if tc.noRaw {
						require.ErrorIs(t, err, os.ErrNotExist)
						return
					}
					require.NoError(t, err)
					var compact bytes.Buffer
					expectedEnvelope := tc.envelope
					if tc.name == "transformed image" {
						expectedEnvelope = `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}]}`
					}
					require.NoError(t, json.Compact(&compact, []byte(expectedEnvelope)))
					require.Equal(t, compact.String()+"\n", string(raw))
					if tc.name == "digest mismatch" {
						remote.Close()
						recovery := exec.CommandContext(t.Context(), binary, "extract", "--from-file=response.json", "--save-base64=data=recovered")
						recovery.Dir = directory
						recovery.Env = []string{cliInvocationSocket + "=/does/not/exist"}
						var recoveredOut, recoveredErr bytes.Buffer
						recovery.Stdout, recovery.Stderr = &recoveredOut, &recoveredErr
						require.NoError(t, recovery.Run(), "%s", recoveredErr.String())
						require.Empty(t, recoveredOut.String())
						recovered, err := os.ReadFile(filepath.Join(directory, "recovered"))
						require.NoError(t, err)
						require.Equal(t, []byte{0, 255}, recovered)
						retained, err := os.ReadFile(filepath.Join(directory, "response.json"))
						require.NoError(t, err)
						require.Equal(t, raw, retained)
						require.Equal(t, int32(1), calls.Load())
					}
				})
			}
		})
	}
}
