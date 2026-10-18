package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/api/adk"
)

func TestCLIProcessCollection(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, envelope, target, flag, digest string
				want                                 []string
				indices                              []int
				failure                              string
				status                               int
				args                                 []string
				noCall, noRaw, errorArtifact         bool
				mutation                             string
				request                              string
				help                                 bool
			}{
				{name: "PDF pages", envelope: `{"content":[],"structuredContent":{"images":[{"page":2,"base64_string":"AA=="},{"page":4,"base64_string":"AP8="}]}}`, target: "images.base64_string", flag: "save-base64-dir", want: []string{"\x00", "\x00\xff"}, indices: []int{0, 1}},
				{name: "browser downloads", envelope: `{"content":[],"structuredContent":{"downloads":[{"id":"a","suggestedFilename":"../../same.pdf","dataBase64":"AA==","sha256":"opaque"},{"id":"b","suggestedFilename":"../../same.pdf","dataBase64":"AP8=","url":"https://invalid/no-fetch"}]}}`, target: "downloads.dataBase64", flag: "save-base64-dir", want: []string{"\x00", "\x00\xff"}, indices: []int{0, 1}},
				{name: "same row digests", envelope: fmt.Sprintf(`{"content":[],"structuredContent":{"images":[{"data":"AA==","sha256":"%x"},{"data":"AP8=","sha256":"%s"}]}}`, sha256.Sum256([]byte{0}), strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256([]byte{0, 255})))), target: "images.data", flag: "save-base64-dir", digest: "images.sha256", want: []string{"\x00", "\x00\xff"}, indices: []int{0, 1}},
				{name: "text rows", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first\n"},{"text":""},{"text":"\ud83d\ude00"}]}}`, target: "pages.text", want: []string{"first\n", "", "\U0001f600"}, indices: []int{0, 1, 2}},
				{name: "empty array unknown member", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.unobserved", want: []string{}},
				{name: "empty array paired digest", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.unobserved", digest: "pages.sha256", want: []string{}},
				{name: "native images indices", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"text","text":"ignored"},{"type":"image","data":"AP8=","mimeType":"image/png"}]}`, target: "content.image", flag: "save-base64-dir", want: []string{"\x00", "\x00\xff"}, indices: []int{0, 2}},
				{name: "native texts", envelope: `{"content":[{"type":"text","text":"first"},{"type":"text","text":"{\"tabs\":[]}"}]}`, target: "text", want: []string{"first", `{"tabs":[]}`}, indices: []int{0, 1}},
				{name: "native resource texts", envelope: `{"content":[{"type":"resource","resource":{"uri":"test://a","text":"first"}},{"type":"text","text":"ignored"},{"type":"resource","resource":{"uri":"test://b","text":"second"}}]}`, target: "resource.text", want: []string{"first", "second"}, indices: []int{0, 2}},
				{name: "native resource blobs", envelope: `{"content":[{"type":"resource","resource":{"uri":"test://a","blob":"AA=="}},{"type":"resource","resource":{"uri":"test://b","blob":"AP8="}}]}`, target: "content.resource.blob", flag: "save-base64-dir", want: []string{"\x00", "\x00\xff"}, indices: []int{0, 1}},
				{name: "native digest singular", envelope: fmt.Sprintf(`{"content":[{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"image","data":"AA==","mimeType":"image/png"}],"structuredContent":{"sha":"%x"}}`, sha256.Sum256([]byte{0})), target: "image", flag: "save-base64-dir", digest: "sha", want: []string{"\x00", "\x00"}, indices: []int{0, 1}},
				{name: "literal dotted no fallback", envelope: `{"content":[],"structuredContent":{"pages.text":"literal","pages":[{"text":"generated"}]}}`, target: "pages.text", failure: "literal property"},
				{name: "literal native no fallback", envelope: `{"content":[{"type":"text","text":"native"}],"structuredContent":{"text":null}}`, target: "text", failure: "literal property"},
				{name: "ambiguous qualified groups", envelope: `{"content":[],"structuredContent":{"a.b":[{"c":"one"}],"a":[{"b.c":"two"}]}}`, target: "a.b.c", failure: "ambiguous"},
				{name: "native child collision", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}],"structuredContent":{"g":{"image":"child"}}}`, target: "image", failure: "ambiguous"},
				{name: "native array collision", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}],"structuredContent":{"content":[{"image":"AA=="}]}}`, target: "content.image", failure: "ambiguous"},
				{name: "missing array", envelope: `{"content":[],"structuredContent":{}}`, target: "pages.text", failure: "missing"},
				{name: "null array", envelope: `{"content":[],"structuredContent":{"pages":null}}`, target: "pages.text", failure: "missing"},
				{name: "non-object row", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"},"second"]}}`, target: "pages.text", failure: "object rows"},
				{name: "missing member", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"},{}]}}`, target: "pages.text", failure: "member missing"},
				{name: "browser skipped data", envelope: `{"content":[],"structuredContent":{"downloads":[{"dataBase64":"AA=="},{"dataSkipped":true,"readError":"unavailable"}]}}`, target: "downloads.dataBase64", flag: "save-base64-dir", failure: "member missing"},
				{name: "null member after valid", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"},{"text":null}]}}`, target: "pages.text", failure: "one string"},
				{name: "non-string member", envelope: `{"content":[],"structuredContent":{"pages":[{"text":42}]}}`, target: "pages.text", failure: "one string"},
				{name: "no short array field", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}]}}`, target: "text", failure: "missing"},
				{name: "no recursive array", envelope: `{"content":[],"structuredContent":{"pages":[{"nested":{"text":"first"}}]}}`, target: "pages.nested.text", failure: "member missing"},
				{name: "dotted member literal", envelope: `{"content":[],"structuredContent":{"pages":[{"nested.text":"first"}]}}`, target: "pages.nested.text", want: []string{"first"}, indices: []int{0}},
				{name: "structured suppresses native text", envelope: `{"content":[{"type":"text","text":"compatibility"}],"structuredContent":{"pages":[]}}`, target: "content.text", failure: "missing"},
				{name: "non-object structured", envelope: `{"content":[{"type":"text","text":"fallback"}],"structuredContent":[]}`, target: "text", failure: "structuredContent"},
				{name: "duplicate member", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first","text":"last"}]}}`, target: "pages.text", failure: "duplicate"},
				{name: "duplicate group", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}],"pages":[]}}`, target: "pages.text", failure: "duplicate"},
				{name: "duplicate structured", envelope: `{"content":[],"structuredContent":{"pages":[]},"structuredContent":{"pages":[]}}`, target: "pages.text", failure: "duplicate"},
				{name: "opaque duplicate metadata", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first","meta":1,"meta":2,"bad":"\ud800","exact":9007199254740993}]}}`, target: "pages.text", want: []string{"first"}, indices: []int{0}},
				{name: "malformed Unicode", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"},{"text":"\ud800"}]}}`, target: "pages.text", failure: "Unicode"},
				{name: "invalid UTF8", envelope: "{\"content\":[],\"structuredContent\":{\"pages\":[{\"text\":\"" + string([]byte{255}) + "\"}]}}", target: "pages.text", failure: "Unicode"},
				{name: "malformed member name", envelope: `{"content":[],"structuredContent":{"pages":[{"\ud800":"first"}]}}`, target: "pages.\ufffd", failure: "Unicode"},
				{name: "bad base64 after valid", envelope: `{"content":[],"structuredContent":{"images":[{"data":"AA=="},{"data":"AB=="}]}}`, target: "images.data", flag: "save-base64-dir", failure: "canonical"},
				{name: "digest wrong group", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}],"other":[{"sha":"ignored"}]}}`, target: "pages.text", digest: "other.sha", failure: "same array group"},
				{name: "digest scalar refused", envelope: `{"content":[],"structuredContent":{"pages":[],"sha":"ignored"}}`, target: "pages.text", digest: "sha", failure: "literal property"},
				{name: "digest missing row", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}]}}`, target: "pages.text", digest: "pages.sha", failure: "member missing"},
				{name: "digest malformed", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first","sha":"short"}]}}`, target: "pages.text", digest: "pages.sha", failure: "64 hex digits"},
				{name: "digest mismatch", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first","sha":"0000000000000000000000000000000000000000000000000000000000000000"}]}}`, target: "pages.text", digest: "pages.sha", failure: "mismatch"},
				{name: "duplicate digest", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first","sha":"one","sha":"two"}]}}`, target: "pages.text", digest: "pages.sha", failure: "duplicate"},
				{name: "native missing image", envelope: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"image","mimeType":"image/png"}]}`, target: "image", flag: "save-base64-dir", failure: "one string"},
				{name: "tool error still saves", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"mechanical"}]},"isError":true}`, target: "pages.text", want: []string{"mechanical"}, indices: []int{0}, failure: "isError", errorArtifact: true},
				{name: "unsupported still saves", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"mechanical"}]},"resultType":"future"}`, target: "pages.text", want: []string{"mechanical"}, indices: []int{0}, failure: "unsupported", errorArtifact: true},
				{name: "100 members", envelope: `{"content":[],"structuredContent":{"pages":[` + strings.TrimSuffix(strings.Repeat(`{"text":""},`, 100), ",") + `]}}`, target: "pages.text", want: make([]string, 100)},
				{name: "101 members", envelope: `{"content":[],"structuredContent":{"pages":[` + strings.TrimSuffix(strings.Repeat(`{"text":""},`, 101), ",") + `]}}`, target: "pages.text", failure: "100 files"},
				{name: "missing raw", args: []string{"--save-text-dir=pages.text=artifacts"}, failure: "requires --output-file", status: 2, noCall: true, noRaw: true},
				{name: "duplicate save", args: []string{"--save-text-dir=pages.text=artifacts", "--save-text=other=file", "--output-file=response.json"}, failure: "one save", status: 2, noCall: true, noRaw: true},
				{name: "dash path", args: []string{"--save-text-dir=pages.text=-", "--output-file=response.json"}, failure: "NAME=PATH", status: 2, noCall: true, noRaw: true},
				{name: "help and save", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--help"}, failure: "--help", status: 2, noCall: true, noRaw: true},
				{name: "overlapping raw", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=artifacts/response.json"}, failure: "tool not called", noCall: true, noRaw: true},
				{name: "identical raw", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=artifacts"}, failure: "tool not called", noCall: true, noRaw: true},
				{name: "raw as parent", args: []string{"--save-text-dir=pages.text=response.json/artifacts", "--output-file=response.json"}, failure: "tool not called", noCall: true, noRaw: true},
				{name: "existing directory", target: "pages.text", mutation: "existing directory", failure: "tool not called", noCall: true, noRaw: true},
				{name: "existing file", target: "pages.text", mutation: "existing file", failure: "tool not called", noCall: true, noRaw: true},
				{name: "existing symlink", target: "pages.text", mutation: "existing symlink", failure: "tool not called", noCall: true, noRaw: true},
				{name: "directory replaced", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}]}}`, target: "pages.text", mutation: "replace", failure: "replacement was not removed"},
				{name: "late file conflict", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"},{"text":"second"}]}}`, target: "pages.text", mutation: "0002", failure: "directory not empty"},
				{name: "manifest conflict", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}]}}`, target: "pages.text", mutation: "manifest.json", failure: "directory not empty"},
				{name: "raw limit", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"` + strings.Repeat("x", cliOutputLimit) + `"}]}}`, target: "pages.text", failure: "32 MiB", noRaw: true},
				{name: "gateway compatibility copy limit", envelope: `{"content":[{"type":"text","text":"` + strings.Repeat("x", 17<<20) + `"}],"structuredContent":{"pages":[{"text":"` + strings.Repeat("x", 17<<20) + `"}]}}`, target: "pages.text", failure: "32 MiB", noRaw: true},
				{name: "manifest write failure", envelope: `{"content":[],"structuredContent":{"pages":[` + strings.TrimSuffix(strings.Repeat(`{"text":""},`, 10), ",") + `]}}`, target: "pages.text", failure: "file too large"},
				{name: "cancel after dispatch", target: "pages.text", failure: "context canceled", noRaw: true},
				{name: "whole JSON reserved properties", envelope: `{"content":[],"structuredContent":{"pages":[{"text":"first"}]}}`, target: "pages.text", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--input-file=request.json"}, request: `{"save-text-dir":"literal","save-base64-dir":"literal"}`, want: []string{"first"}},
				{name: "stdin JSON", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.text", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--input-file=-"}, request: `{"save-text-dir":"literal","save-base64-dir":"literal"}`, want: []string{}},
				{name: "text file input", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.text", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--text-file=mode=input.txt"}, request: `{"mode":"file value"}`, want: []string{}},
				{name: "base64 file input", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.text", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--base64-file=mode=input.txt"}, request: `{"mode":"ZmlsZSB2YWx1ZQ=="}`, want: []string{}},
				{name: "ordinary argument", envelope: `{"content":[],"structuredContent":{"pages":[]}}`, target: "pages.text", args: []string{"--save-text-dir=pages.text=artifacts", "--output-file=response.json", "--mode=value"}, request: `{"mode":"value"}`, want: []string{}},
				{name: "help collection grammar", args: []string{"--help"}, help: true, noCall: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls atomic.Int32
					var arguments atomic.Value
					started := make(chan struct{})
					directory := t.TempDir()
					server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "collection", Version: "1"}, nil)
					server.AddTool(&mcpsdk.Tool{Name: "result", InputSchema: json.RawMessage(`{"type":"object","properties":{"mode":{"type":"string"},"save-text-dir":{"type":"string"},"save-base64-dir":{"type":"string"}},"additionalProperties":false}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
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
								arguments.Store(request.Params.Arguments)
								close(started)
								if tc.name == "cancel after dispatch" {
									<-r.Context().Done()
									return
								}
								switch tc.mutation {
								case "replace":
									if err := os.Rename(filepath.Join(directory, "artifacts"), filepath.Join(directory, "moved")); err != nil {
										t.Error(err)
										return
									}
									if err := os.Mkdir(filepath.Join(directory, "artifacts"), 0700); err != nil {
										t.Error(err)
										return
									}
									if err := os.WriteFile(filepath.Join(directory, "artifacts", "replacement"), []byte("untouched"), 0600); err != nil {
										t.Error(err)
										return
									}
								case "0002", "manifest.json":
									if err := os.WriteFile(filepath.Join(directory, "artifacts", tc.mutation), []byte("untouched"), 0600); err != nil {
										t.Error(err)
										return
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
					switch tc.mutation {
					case "existing directory":
						require.NoError(t, os.Mkdir(filepath.Join(directory, "artifacts"), 0700))
					case "existing file":
						require.NoError(t, os.WriteFile(filepath.Join(directory, "artifacts"), []byte("untouched"), 0600))
					case "existing symlink":
						require.NoError(t, os.Symlink("missing", filepath.Join(directory, "artifacts")))
					}
					binding := filepath.Join(directory, "binding.json")
					require.NoError(t, os.WriteFile(filepath.Join(directory, "request.json"), []byte(`{"save-text-dir":"literal","save-base64-dir":"literal"}`), 0600))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "input.txt"), []byte("file value"), 0600))
					require.NoError(t, os.WriteFile(binding, []byte(`{"name":"collection","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
					flag := tc.flag
					if flag == "" {
						flag = "save-text-dir"
					}
					args := []string{"--binding-file", binding, "result", "--" + flag, tc.target + "=artifacts", "--output-file=response.json"}
					if tc.args != nil {
						args = append([]string{"--binding-file", binding, "result"}, tc.args...)
					}
					if tc.digest != "" {
						args = append(args, "--verify-sha256="+tc.digest)
					}
					command := exec.CommandContext(t.Context(), binary, args...)
					if tc.name == "manifest write failure" {
						command = exec.CommandContext(t.Context(), "bash", append([]string{"-c", `ulimit -f 1; exec "$@"`, "collection-test", binary}, args...)...)
					}
					command.Dir = directory
					command.Stdin = strings.NewReader(`{"save-text-dir":"literal","save-base64-dir":"literal"}`)
					if mode == "managed" {
						env, cleanup, err := PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{{Name: "collection", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}}, false, nil)(cliSessionContext{Context: t.Context(), id: "collection"}, t.Context())
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
					require.Less(t, stderr.Len(), 2048)
					wantCalls := int32(1)
					if tc.noCall {
						wantCalls = 0
					}
					require.Equal(t, wantCalls, calls.Load())
					if tc.help {
						require.Contains(t, stdout.String(), "--save-text-dir GROUP.FIELD=DIR")
						require.Contains(t, stdout.String(), "--save-base64-dir GROUP.FIELD=DIR")
						require.Contains(t, stdout.String(), "manifest.json")
						require.NotContains(t, stdout.String(), "  --save-text-dir <")
						return
					}
					require.Empty(t, stdout.String())
					if tc.request != "" {
						require.JSONEq(t, tc.request, string(arguments.Load().(json.RawMessage)))
					}
					raw, err := os.ReadFile(filepath.Join(directory, "response.json"))
					if tc.noRaw {
						require.ErrorIs(t, err, os.ErrNotExist)
					} else {
						require.NoError(t, err)
						require.Equal(t, tc.envelope+"\n", string(raw))
					}
					if tc.name == "digest mismatch" {
						remote.Close()
						recovery := exec.CommandContext(t.Context(), binary, "extract", "--from-file=response.json", "--save-text-dir=pages.text=recovered")
						recovery.Dir = directory
						recovery.Env = []string{cliInvocationSocket + "=/does/not/exist"}
						var recoveredOut, recoveredErr bytes.Buffer
						recovery.Stdout, recovery.Stderr = &recoveredOut, &recoveredErr
						require.NoError(t, recovery.Run(), "%s", recoveredErr.String())
						require.Empty(t, recoveredOut.String())
						recovered, err := os.ReadFile(filepath.Join(directory, "recovered", "0001"))
						require.NoError(t, err)
						require.Equal(t, "first", string(recovered))
						manifest, err := os.ReadFile(filepath.Join(directory, "recovered", "manifest.json"))
						require.NoError(t, err)
						require.Contains(t, string(manifest), fmt.Sprintf("%x", sha256.Sum256(raw)))
						retained, err := os.ReadFile(filepath.Join(directory, "response.json"))
						require.NoError(t, err)
						require.Equal(t, raw, retained)
						require.Equal(t, int32(1), calls.Load())
					}
					if tc.failure != "" && !tc.errorArtifact {
						switch tc.mutation {
						case "replace", "0002", "manifest.json":
							name := tc.mutation
							if name == "replace" {
								name = "replacement"
							}
							data, err := os.ReadFile(filepath.Join(directory, "artifacts", name))
							require.NoError(t, err)
							require.Equal(t, "untouched", string(data))
							entries, err := os.ReadDir(filepath.Join(directory, "artifacts"))
							require.NoError(t, err)
							require.Len(t, entries, 1)
						case "existing directory", "existing file", "existing symlink":
							_, err := os.Lstat(filepath.Join(directory, "artifacts"))
							require.NoError(t, err)
						default:
							_, err := os.Lstat(filepath.Join(directory, "artifacts"))
							require.ErrorIs(t, err, os.ErrNotExist)
						}
						return
					}
					info, err := os.Stat(filepath.Join(directory, "artifacts"))
					require.NoError(t, err)
					require.Equal(t, os.FileMode(0700), info.Mode().Perm())
					manifestBytes, err := os.ReadFile(filepath.Join(directory, "artifacts", "manifest.json"))
					require.NoError(t, err)
					info, err = os.Stat(filepath.Join(directory, "artifacts", "manifest.json"))
					require.NoError(t, err)
					require.Equal(t, os.FileMode(0600), info.Mode().Perm())
					entries, err := os.ReadDir(filepath.Join(directory, "artifacts"))
					require.NoError(t, err)
					require.Len(t, entries, len(tc.want)+1)
					var manifest struct {
						Version      int    `json:"version"`
						Target       string `json:"target"`
						Conversion   string `json:"conversion"`
						SourceSHA256 string `json:"source_sha256"`
						Files        []struct {
							Name        string `json:"name"`
							SourceIndex int    `json:"source_index"`
							Length      int    `json:"length"`
							SHA256      string `json:"sha256"`
						} `json:"files"`
					}
					require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
					require.Equal(t, 1, manifest.Version)
					if tc.name == "native images indices" || tc.name == "same row digests" || tc.name == "empty array unknown member" {
						remote.Close()
						recovery := exec.CommandContext(t.Context(), binary, "extract", "--from-file=response.json", "--"+flag+"="+tc.target+"=recovered")
						if tc.digest != "" {
							recovery.Args = append(recovery.Args, "--verify-sha256="+tc.digest)
						}
						recovery.Dir = directory
						recovery.Env = []string{cliInvocationSocket + "=/does/not/exist"}
						recoveredOutput, err := recovery.CombinedOutput()
						require.NoError(t, err, "%s", recoveredOutput)
						require.Empty(t, recoveredOutput)
						recoveredManifest, err := os.ReadFile(filepath.Join(directory, "recovered", "manifest.json"))
						require.NoError(t, err)
						require.Equal(t, manifestBytes, recoveredManifest)
						for i, want := range tc.want {
							data, err := os.ReadFile(filepath.Join(directory, "recovered", fmt.Sprintf("%04d", i+1)))
							require.NoError(t, err)
							require.Equal(t, want, string(data))
						}
						require.Equal(t, int32(1), calls.Load())
					}

					require.Equal(t, tc.target, manifest.Target)
					conversion := "text"
					if tc.flag == "save-base64-dir" {
						conversion = "base64"
					}
					require.Equal(t, conversion, manifest.Conversion)
					require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), manifest.SourceSHA256)
					require.Len(t, manifest.Files, len(tc.want))
					for i, want := range tc.want {
						name := fmt.Sprintf("%04d", i+1)
						data, err := os.ReadFile(filepath.Join(directory, "artifacts", name))
						require.NoError(t, err)
						require.Equal(t, want, string(data))
						require.Equal(t, name, manifest.Files[i].Name)
						index := i
						if tc.indices != nil {
							index = tc.indices[i]
						}
						require.Equal(t, index, manifest.Files[i].SourceIndex)
						require.Equal(t, len(want), manifest.Files[i].Length)
						require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(want))), manifest.Files[i].SHA256)
						info, err := os.Stat(filepath.Join(directory, "artifacts", name))
						require.NoError(t, err)
						require.Equal(t, os.FileMode(0600), info.Mode().Perm())
					}
				})
			}
		})
	}
}
