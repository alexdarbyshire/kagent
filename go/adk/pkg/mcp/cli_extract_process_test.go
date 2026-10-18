package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIProcessExtract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	directory := t.TempDir()
	source := []byte("{\"content\":[],\"structuredContent\":{\"output\":{\"base64_string\":\"AP8=\"}}}\n")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "result.json"), source, 0600))
	command := exec.CommandContext(t.Context(), binary, "extract", "--from-file=result.json", "--save-base64", "output.base64_string=artifact")
	command.Dir = directory
	command.Env = append(os.Environ(), cliInvocationSocket+"=/does/not/exist")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Run(), "%s", stderr.String())
	require.Empty(t, stdout.String())
	artifact, err := os.ReadFile(filepath.Join(directory, "artifact"))
	require.NoError(t, err)
	require.Equal(t, []byte{0, 255}, artifact)
	retained, err := os.ReadFile(filepath.Join(directory, "result.json"))
	require.NoError(t, err)
	require.Equal(t, source, retained)
}

func TestCLIProcessExtractValidation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, tc := range []struct {
		name, source, want, failure string
		args                        []string
		status                      int
		keep                        bool
	}{
		{name: "conversion", source: `{"content":[],"structuredContent":{"document":{"md_content":"# Title\n"}}}`, args: []string{"--save-text=document.md_content=artifact"}, want: "# Title\n", keep: true},
		{name: "archive digest", source: fmt.Sprintf(`{"content":[],"structuredContent":{"base64_string":"AP8=","sha256":"%x"}}`, sha256.Sum256([]byte{0, 255})), args: []string{"--save-base64=base64_string=artifact", "--verify-sha256=sha256"}, want: "\x00\xff", keep: true},
		{name: "native image", source: `{"content":[{"type":"image","data":"AP8=","mimeType":"image/png"}]}`, args: []string{"--save-base64=image=artifact"}, want: "\x00\xff", keep: true},

		{name: "malformed embedded resource", source: `{"content":[{"type":"resource","resource":17}],"structuredContent":{"answer":"ok"}}`, args: []string{"--save-text=answer=artifact"}, status: 2},
		{name: "malformed image MIME", source: `{"content":[{"type":"image","data":"AA==","mimeType":false}],"structuredContent":{"answer":"ok"}}`, args: []string{"--save-text=answer=artifact"}, status: 2},
		{name: "unknown native type", source: `{"content":[{"type":"future"}],"structuredContent":{"answer":"ok"}}`, args: []string{"--save-text=answer=artifact"}, status: 2},

		{name: "malformed native metadata", source: `{"content":[{"type":"text","text":"ok","_meta":17}],"structuredContent":{"answer":"ok"}}`, args: []string{"--save-text=answer=artifact"}, status: 2},
		{name: "malformed native input", source: `{"content":[{"type":"tool_use","id":"a","name":"example","input":[]}],"structuredContent":{"answer":"ok"}}`, args: []string{"--save-text=answer=artifact"}, status: 2},
		{name: "opaque native metadata", source: `{"content":[{"type":"text","text":"ok","_meta":{"big":1e999,"duplicate":1,"duplicate":2,"invalid":"\ud800"}}],"structuredContent":{"answer":"ok","big":1e999,"same":1,"same":2}}`, args: []string{"--save-text=answer=artifact"}, want: "ok", keep: true},
		{name: "selected bad binary", source: `{"content":[{"type":"image","data":"?","mimeType":"image/png"}]}`, args: []string{"--save-base64=image=artifact"}, status: 1, failure: "canonical"},

		{name: "native resource blob", source: `{"content":[{"type":"resource","resource":{"uri":"test://a","blob":"AP8=","_meta":{"big":1e999}}}]}`, args: []string{"--save-base64=resource.blob=artifact"}, want: "\x00\xff", keep: true},
		{name: "native resource text", source: `{"content":[{"type":"resource","resource":{"uri":"test://a","text":"ok","_meta":{"same":1,"same":2}}}]}`, args: []string{"--save-text=resource.text=artifact"}, want: "ok", keep: true},
		{name: "selected image duplicate", source: `{"content":[{"type":"image","data":"AA==","data":"AQ==","mimeType":"image/png"}]}`, args: []string{"--save-base64=image=artifact"}, status: 1, failure: "duplicate"},
		{name: "opaque nested native content", source: `{"content":[{"type":"tool_use","id":"a","name":"example","input":{"big":1e999}},{"type":"tool_result","toolUseId":"a","content":[{"type":"image","data":"AA==","mimeType":"image/png","_meta":{"big":1e999}},{"type":"resource","resource":{"uri":"test://a","text":"ok","_meta":{"big":1e999}}}],"structuredContent":{"big":1e999}}],"structuredContent":{"answer":"ok","big":1e999}}`, args: []string{"--save-text=answer=artifact"}, want: "ok", keep: true},
		{name: "selected resource blob conversion", source: `{"content":[{"type":"resource","resource":{"uri":"test://a","blob":"?","_meta":{"big":1e999}}}]}`, args: []string{"--save-base64=resource.blob=artifact"}, status: 1, failure: "canonical"},
		{name: "other MCP content", source: `{"content":[{"type":"audio","data":"AA==","mimeType":"audio/wav"},{"type":"resource_link","uri":"test://a","name":"a"},{"type":"resource","resource":{"uri":"test://a","text":"resource"}},{"type":"text","text":"ok"}]}`, want: "ok", keep: true},
		{name: "artifact write failure", source: `{"content":[{"type":"text","text":"` + strings.Repeat("x", 4096) + `"}]}`, status: 1, failure: "file too large"},
		{name: "empty payload", source: `{"content":[{"type":"text","text":""}]}`, keep: true},
		{name: "opaque metadata", source: `{"content":[{"type":"text","text":"ok"}],"_meta":{"same":1,"same":2,"big":1e999},"unused":"\ud800"}`, want: "ok", keep: true},
		{name: "isError", source: `{"content":[{"type":"text","text":"ok"}],"isError":true}`, want: "ok", keep: true, status: 1, failure: "isError"},
		{name: "unsupported", source: `{"content":[{"type":"text","text":"ok"}],"resultType":"PRIVATE_OUTPUT_MARKER"}`, want: "ok", keep: true, status: 1, failure: "unsupported"},
		{name: "empty content", source: `{"content":[]}`, status: 1, failure: "missing"},
		{name: "non-object structured", source: `{"content":[],"structuredContent":null}`, status: 1, failure: "structuredContent"},
		{name: "selected duplicate", source: `{"content":[{"type":"text","text":"first","text":"last"}]}`, status: 1, failure: "duplicate"},
		{name: "selected unicode", source: `{"content":[{"type":"text","text":"\ud800"}]}`, status: 1, failure: "Unicode"},
		{name: "base64 bits", source: `{"content":[{"type":"image","data":"AB==","mimeType":"image/png"}]}`, args: []string{"--save-base64=image=artifact"}, status: 1, failure: "canonical"},
		{name: "jsonrpc", source: `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, status: 2},
		{name: "jsonrpc disguised", source: `{"jsonrpc":"2.0","content":[]}`, status: 2},
		{name: "malformed", source: `{"content":[`, status: 2},
		{name: "trailing", source: `{"content":[]} {}`, status: 2},
		{name: "missing content", source: `{}`, status: 2},
		{name: "null content", source: `{"content":null}`, status: 2},
		{name: "object content", source: `{"content":{}}`, status: 2},
		{name: "bad block", source: `{"content":[1]}`, status: 2},
		{name: "missing block type", source: `{"content":[{}]}`, status: 2},
		{name: "bad error type", source: `{"content":[],"isError":"false"}`, status: 2},
		{name: "null error", source: `{"content":[],"isError":null}`, status: 2},
		{name: "duplicate status", source: `{"content":[],"isError":true,"isError":false}`, status: 2},
		{name: "null result type", source: `{"content":[],"resultType":null}`, status: 2},
		{name: "duplicate content", source: `{"content":[],"content":[]}`, status: 2},
		{name: "output-file", args: []string{"--save-text=text=artifact", "--output-file=other"}, status: 2},
		{name: "input-file", args: []string{"--save-text=text=artifact", "--input-file=-"}, status: 2},
		{name: "binding-file", args: []string{"--save-text=text=artifact", "--binding-file=absent"}, status: 2},
		{name: "property", args: []string{"--save-text=text=artifact", "--property=value"}, status: 2},
		{name: "duplicate save", args: []string{"--save-text=text=artifact", "--save-base64=text=other"}, status: 2},
		{name: "duplicate source", args: []string{"--save-text=text=artifact", "--from-file=other"}, status: 2},
		{name: "duplicate digest", args: []string{"--save-text=text=artifact", "--verify-sha256=sha", "--verify-sha256=sha"}, status: 2},
		{name: "help with save", args: []string{"--save-text=text=artifact", "--help"}, status: 2},
		{name: "dash output", args: []string{"--save-text=text=-"}, status: 2},
		{name: "no save", args: []string{}, status: 2},
		{name: "same source", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=result.json"}, status: 1},
		{name: "existing", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=exists"}, status: 1},
		{name: "symlink", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=link"}, status: 1},
		{name: "directory", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=."}, status: 1},
		{name: "fifo destination", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=fifo"}, status: 1},
		{name: "missing parent", source: `{"content":[{"type":"text","text":"ok"}]}`, args: []string{"--save-text=text=missing/artifact"}, status: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(directory, "result.json"), []byte(tc.source), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(directory, "exists"), []byte("unchanged"), 0600))
			require.NoError(t, os.Symlink("absent", filepath.Join(directory, "link")))
			require.NoError(t, syscall.Mkfifo(filepath.Join(directory, "fifo"), 0600))
			args := tc.args
			if args == nil {
				args = []string{"--save-text=text=artifact"}
			}
			command := exec.CommandContext(t.Context(), binary, append([]string{"extract", "--from-file=result.json"}, args...)...)

			if tc.name == "artifact write failure" {
				command = exec.CommandContext(t.Context(), "bash", append([]string{"-c", `ulimit -f 1; exec "$@"`, "extract-test", binary}, command.Args[1:]...)...)
			}
			command.Dir = directory
			command.Env = []string{cliInvocationSocket + "=/does/not/exist"}
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if tc.status == 0 {
				require.NoError(t, err, "%s", stderr.String())
			} else {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.Equal(t, tc.status, exit.ExitCode(), "%s", stderr.String())
				require.Contains(t, stderr.String(), tc.failure)
			}
			require.Empty(t, stdout.String())
			require.Less(t, stderr.Len(), 2048)
			require.NotContains(t, stderr.String(), "PRIVATE_OUTPUT_MARKER")
			data, err := os.ReadFile(filepath.Join(directory, "artifact"))
			if tc.keep {
				require.NoError(t, err)
				require.Equal(t, tc.want, string(data))
				info, err := os.Stat(filepath.Join(directory, "artifact"))
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			source, err := os.ReadFile(filepath.Join(directory, "result.json"))
			require.NoError(t, err)
			require.Equal(t, tc.source, string(source))
			existing, err := os.ReadFile(filepath.Join(directory, "exists"))
			require.NoError(t, err)
			require.Equal(t, "unchanged", string(existing))
		})
	}
}

func TestCLIExtractLocalBoundaries(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	artifact := filepath.Join(directory, "artifact")
	args := []string{"extract", "--from-file", source, "--save-text", "text=" + artifact}
	require.NoError(t, syscall.Mkfifo(source, 0600))
	require.Equal(t, 1, CLIExitCode(RunCLI(t.Context(), args, nil, &bytes.Buffer{})))
	require.NoError(t, os.Remove(source))
	require.Equal(t, 1, CLIExitCode(RunCLI(t.Context(), args, nil, &bytes.Buffer{})))
	require.NoError(t, os.WriteFile(source, []byte(`{"content":[{"type":"text","text":"ok"}]}`), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, RunCLI(ctx, args, nil, &bytes.Buffer{}), context.Canceled)
	ctx = WithResultTransformer(t.Context(), func(context.Context, json.RawMessage) (json.RawMessage, error) { panic("extract must not transform") })
	require.NoError(t, RunCLI(ctx, args, nil, &bytes.Buffer{}))
	file, err := os.OpenFile(source, os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(32<<20+1))
	require.NoError(t, file.Close())
	limitErr := RunCLI(t.Context(), args, nil, &bytes.Buffer{})
	require.Equal(t, 1, CLIExitCode(limitErr))
	require.ErrorContains(t, limitErr, "32 MiB")
	data, err := os.ReadFile(artifact)
	require.NoError(t, err)
	require.Equal(t, "ok", string(data))
}

func TestCLIExtractNoSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "socket")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	var attempts atomic.Int32
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	t.Setenv(cliInvocationSocket, socket)
	var help bytes.Buffer
	require.NoError(t, RunCLI(t.Context(), []string{"extract", "--help"}, nil, &help))
	require.Contains(t, help.String(), "content.image")
	directory := t.TempDir()
	source := filepath.Join(directory, "result.json")
	require.NoError(t, os.WriteFile(source, []byte(`{"content":[{"type":"text","text":"local"}]}`), 0600))
	var stdout bytes.Buffer
	require.NoError(t, RunCLI(t.Context(), []string{"extract", "--from-file", source, "--save-text", "text=" + filepath.Join(directory, "artifact")}, nil, &stdout))
	require.Empty(t, stdout.String())
	require.NoError(t, listener.Close())
	<-stopped
	require.Zero(t, attempts.Load())
}
