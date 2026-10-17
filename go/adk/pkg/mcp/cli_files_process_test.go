package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestCLIProcessLocalFiles(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	received := make(chan json.RawMessage, 1)
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "files", Version: "1"}, nil)
	schemas := map[string]string{
		"convert":  `{"type":"object","properties":{"body":{"type":"object","properties":{"filename":{"type":"string"},"base64_string":{"type":"string"},"note":{"type":["string","number"]}},"required":["filename","base64_string"]}},"required":["body"]}`,
		"stamp":    `{"type":"object","properties":{"body":{"type":"object","properties":{"pdf_base64":{"type":"string"},"image_base64":{"type":"string"},"edits":{"type":"array"}},"required":["pdf_base64","image_base64","edits"]}},"required":["body"]}`,
		"reject":   `{"type":"object","properties":{"text":{"type":"string","enum":["allowed"]},"number":{"type":"number"}}}`,
		"overlap":  `{"type":"object","properties":{"path.id":{"type":"string"},"id":{"type":"string"},"path":{"type":"object","properties":{"id":{"type":"integer"},"label":{"type":"string"}}}}}`,
		"reserved": `{"type":"object","properties":{"base64-file":{"type":"string"},"text-file":{"type":"string"},"query":{"type":"object","properties":{"base64-file":{"type":"string"},"text-file":{"type":"string"}}}}}`,
		"limits":   `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}}}`,
	}
	for name, schema := range schemas {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: json.RawMessage(schema)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			received <- bytes.Clone(request.Params.Arguments)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
		})
	}
	var calls atomic.Int32
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{MaxRequestBodyBytes: 32 << 20})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var message struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "tools/call" {
				calls.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	directory := t.TempDir()
	binding := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"files","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "dummy=pdf"), []byte{0, 255, 1}, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "note"), []byte("123\n"), 0600))
	for name, data := range map[string][]byte{
		"signature": []byte("DUMMY"), "empty": {}, "marker": []byte("E2_PRIVATE_FILE_MARKER_7b39"),
		"request.json": []byte(`{"base64-file":"literal","text-file":"literal"}`),
		"eight":        bytes.Repeat([]byte("x"), 8<<20), "over-eight": bytes.Repeat([]byte("x"), (8<<20)+1),
		"json-expansion": make([]byte, 5<<20),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0600))
	}
	require.NoError(t, os.Symlink("dummy=pdf", filepath.Join(directory, "link")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(directory, "fifo"), 0600))
	t.Run("cancelled local input", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var output bytes.Buffer
		err := RunCLI(ctx, []string{"--binding-file", binding, "limits", "--text-file=a=note"}, io.NopCloser(strings.NewReader("")), &output)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, calls.Load())
		require.Empty(t, output.String())
	})
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	config := adk.MCPCLIConfig{Name: "files", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}
	env, cleanup, err := PrepareCLIEnvironment(owner, []adk.MCPCLIConfig{config}, false, nil)(cliSessionContext{Context: t.Context(), id: "files"}, t.Context())
	require.NoError(t, err)
	defer cleanup()
	for _, mode := range []string{"standalone", "managed"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				args          []string
				want          string
				errorContains string
				help          bool
				large         bool
			}{
				{name: "conversion", args: []string{"convert", "--filename=dummy.pdf", "--base64-file", "base64_string=dummy=pdf", "--text-file=note=note"}, want: `{"body":{"filename":"dummy.pdf","base64_string":"AP8B","note":"123\n"}}`},
				{name: "two binary files and edits", args: []string{"stamp", "--base64-file=pdf_base64=dummy=pdf", "--base64-file", "image_base64=signature", `--edits=[{"type":"image","page":1}]`}, want: `{"body":{"pdf_base64":"AP8B","image_base64":"RFVNTVk=","edits":[{"type":"image","page":1}]}}`},
				{name: "omitted optional and symlink", args: []string{"convert", "--filename=dummy.pdf", "--base64-file=base64_string=link"}, want: `{"body":{"filename":"dummy.pdf","base64_string":"AP8B"}}`},
				{name: "empty string", args: []string{"limits", "--text-file=a=empty"}, want: `{"a":""}`},
				{name: "reserved children", args: []string{"reserved", "--text-file=query.text-file=note", "--base64-file=query.base64-file=dummy=pdf"}, want: `{"query":{"text-file":"123\n","base64-file":"AP8B"}}`},
				{name: "reserved literals via JSON", args: []string{"reserved", "--input-file=request.json"}, want: `{"base64-file":"literal","text-file":"literal"}`},
				{name: "duplicate literal first", args: []string{"limits", "--a=literal", "--text-file=a=note"}, errorContains: "duplicate or overlapping"},
				{name: "duplicate file first", args: []string{"limits", "--text-file=a=note", "--a=literal"}, errorContains: "duplicate or overlapping"},
				{name: "duplicate file modes", args: []string{"limits", "--text-file=a=note", "--base64-file=a=dummy=pdf"}, errorContains: "duplicate or overlapping"},
				{name: "overlap parent first", args: []string{"overlap", "--path={}", "--text-file=label=note"}, errorContains: "duplicate or overlapping"},
				{name: "overlap child first", args: []string{"overlap", "--text-file=label=note", "--path={}"}, errorContains: "duplicate or overlapping"},
				{name: "whole request first", args: []string{"limits", "--input-file=request.json", "--text-file=a=note"}, errorContains: "cannot be combined"},
				{name: "whole request last", args: []string{"limits", "--text-file=a=note", "--input-file=request.json"}, errorContains: "cannot be combined"},
				{name: "missing file", args: []string{"limits", "--text-file=a=missing"}, errorContains: "failed to read"},
				{name: "failing second file", args: []string{"stamp", "--base64-file=pdf_base64=dummy=pdf", "--base64-file=image_base64=missing", "--edits=[]"}, errorContains: "failed to read"},
				{name: "directory", args: []string{"limits", "--text-file=a=."}, errorContains: "regular file"},
				{name: "fifo", args: []string{"limits", "--text-file=a=fifo"}, errorContains: "regular file"},
				{name: "device", args: []string{"limits", "--text-file=a=/dev/null"}, errorContains: "regular file"},
				{name: "utf8 refusal", args: []string{"limits", "--text-file=a=dummy=pdf"}, errorContains: "UTF-8"},
				{name: "file size", args: []string{"limits", "--text-file=a=over-eight"}, errorContains: "8 MiB"},
				{name: "aggregate size", args: []string{"limits", "--base64-file=a=eight", "--base64-file=b=eight", "--text-file=c=note"}, errorContains: "16 MiB"},
				{name: "assembled size", args: []string{"limits", "--text-file=a=json-expansion"}, errorContains: "24 MiB"},
				{name: "maximum raw bytes", args: []string{"limits", "--base64-file=a=eight", "--base64-file=b=eight"}, large: true},
				{name: "schema refuses private text", args: []string{"reject", "--text-file=text=marker"}, errorContains: "validation failed at /text (enum)"},
				{name: "schema refuses string for number", args: []string{"reject", "--text-file=number=note"}, errorContains: "validation failed at /number (type)"},
				{name: "schema refuses private base64", args: []string{"reject", "--base64-file=number=marker"}, errorContains: "validation failed at /number (type)"},
				{name: "empty target", args: []string{"limits", "--text-file==note"}, errorContains: "NAME=PATH"},
				{name: "empty path", args: []string{"limits", "--text-file=a="}, errorContains: "NAME=PATH"},
				{name: "leading dashes", args: []string{"limits", "--text-file=--a=note"}, errorContains: "NAME=PATH"},
				{name: "stdin file unsupported", args: []string{"limits", "--text-file=a=-"}, errorContains: "NAME=PATH"},
				{name: "unknown target", args: []string{"limits", "--text-file=missing=note"}, errorContains: "unknown argument"},
				{name: "nested pointer unsupported", args: []string{"limits", "--text-file=/a=note"}, errorContains: "unknown argument"},
				{name: "help", args: []string{"limits", "--help"}, help: true},
				{name: "help with file never reads", args: []string{"limits", "--text-file=a=fifo", "--help"}, errorContains: "--help must be used alone"},
				{name: "command help with file never reads", args: []string{"--help", "--text-file=a=fifo"}, errorContains: "--help must be used alone"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := calls.Load()
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
					defer cancel()
					command := exec.CommandContext(ctx, binary, append([]string{"--binding-file", binding}, tc.args...)...)
					command.Dir = directory
					if mode == "managed" {
						command.Env = append(os.Environ(), env...)
					}
					output, err := command.CombinedOutput()
					require.NotContains(t, string(output), "E2_PRIVATE_FILE_MARKER_7b39")
					require.NotContains(t, string(output), base64.StdEncoding.EncodeToString([]byte("E2_PRIVATE_FILE_MARKER_7b39")))
					if tc.errorContains != "" {
						require.Error(t, err, "%s", output)
						require.Contains(t, string(output), tc.errorContains)
						require.Equal(t, before, calls.Load(), "local failure dispatched tools/call")
						return
					}
					require.NoError(t, err, "%s", output)
					if tc.help {
						require.Contains(t, string(output), "--base64-file NAME=PATH")
						require.Contains(t, string(output), "--text-file NAME=PATH")
						require.Equal(t, before, calls.Load())
						return
					}
					require.Equal(t, before+1, calls.Load())
					got := <-received
					if tc.large {
						var values map[string]string
						require.NoError(t, json.Unmarshal(got, &values))
						require.Len(t, values["a"], 11184812)
						require.Equal(t, values["a"], values["b"])
						require.True(t, strings.HasSuffix(values["a"], "="))
					} else {
						require.JSONEq(t, tc.want, string(got))
					}
				})
			}
		})
	}
}
