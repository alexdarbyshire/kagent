package mcp_test

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
)

func TestCLIProcessTypedArguments(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	calls := make(chan json.RawMessage, 100)
	var count atomic.Int32
	var requests atomic.Int32
	discovery := make(chan struct{}, 100)
	var ambiguous atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	connectionClosed := make(chan struct{}, 1)
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "recording", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "record", Description: "Record typed arguments", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"count":{"type":"integer"},"enabled":{"type":"boolean"},"items":{"type":"array","items":{"type":"string"}},"object":{"type":"object"},"nullable":{"type":["string","null"]}},"required":["text"],"additionalProperties":false}`)}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		count.Add(1)
		calls <- req.Params.Arguments
		var input struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return nil, err
		}
		switch input.Text {
		case "precision":
			return &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{"positive": int64(9007199254740993), "nested": map[string]any{"negative": int64(-9007199254740993)}}, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "exact"}}, StructuredContent: map[string]any{"nested": []any{int64(9007199254740993), int64(-9007199254740993)}}}, nil
		case "error":
			return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "denied"}}}, nil
		case "protocol":
			return nil, fmt.Errorf("recording protocol failure")
		case "block":
			started <- struct{}{}
			select {
			case <-ctx.Done():
			case <-release:
				return nil, fmt.Errorf("fixture cancellation deadline")
			}

			return nil, ctx.Err()
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "recorded"}}, StructuredContent: map[string]any{"ok": true}}, nil
	})

	server.AddTool(&mcpsdk.Tool{Name: "fractional", Description: "Exact fractional schema", InputSchema: json.RawMessage(`{"type":"object","properties":{"number":{"type":"number","minimum":1.0000000000000001}},"additionalProperties":false}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		count.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
	})

	server.AddTool(&mcpsdk.Tool{Name: "complex", Description: "Reserved original properties", InputSchema: json.RawMessage(`{"type":"object","$defs":{"arguments":{"type":"object","properties":{"help":{"type":"string"},"input-file":{"type":"string"},"count":{"type":"integer","minimum":9007199254740993}},"required":["help","count"]}},"allOf":[{"$ref":"#/$defs/arguments"}]}`)}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		count.Add(1)
		calls <- req.Params.Arguments
		return &mcpsdk.CallToolResult{Meta: mcpsdk.Meta{"source": "recording"}, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "complex"}}}, nil
	})
	server.AddTool(&mcpsdk.Tool{Name: "hidden", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		count.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"method":"tools/list"`)) {
				discovery <- struct{}{}
			}
			if bytes.Contains(body, []byte(`"text":"wire-json"`)) || bytes.Contains(body, []byte(`"text":"wire-sse"`)) || bytes.Contains(body, []byte(`"text":"wire-error"`)) {
				ambiguous.Add(1)
				var request struct {
					ID json.RawMessage `json:"id"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					return
				}
				isError := "false"
				if bytes.Contains(body, []byte(`"text":"wire-error"`)) {
					isError = "true"
				}
				result := `{"content":[{"type":"text","text":"exact","_meta":{"integer":9007199254740993}}],"structuredContent":{"decimal":1.234567890123456789,"tiny":1e-400,"nested":[9007199254740993,-9007199254740993]},"_meta":{"positive":9007199254740993,"negative":-9007199254740993},"isError":` + isError + `}`
				message := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":` + result + `}`
				if bytes.Contains(body, []byte(`"text":"wire-sse"`)) {
					w.Header().Set("Content-Type", "text/event-stream")
					message = strings.Replace(message, `,"result":`, ",\n\"result\":", 1)
					if _, err := io.WriteString(w, ": keepalive\r\n\r\nevent: message\r\ndata: "+strings.ReplaceAll(message, "\n", "\r\ndata: ")+"\r\n\r\n"); err != nil {
						t.Error(err)
					}
				} else {
					w.Header().Set("Content-Type", "application/json")
					if _, err := io.WriteString(w, message); err != nil {
						t.Error(err)
					}
				}
				return
			}

			if bytes.Contains(body, []byte(`"text":"future-result"`)) || bytes.Contains(body, []byte(`"text":"input-required"`)) {
				ambiguous.Add(1)
				var request struct {
					ID json.RawMessage `json:"id"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					return
				}
				resultType := "future"
				if bytes.Contains(body, []byte(`"text":"input-required"`)) {
					resultType = "input_required"
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"resultType": resultType, "content": []any{}}}); err != nil {
					t.Error(err)
				}
				return
			}

			if bytes.Contains(body, []byte(`"text":"ambiguous"`)) {
				ambiguous.Add(1)
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
			if bytes.Contains(body, []byte(`"text":"redirect"`)) {
				ambiguous.Add(1)
				w.Header().Set("Location", r.URL.String())
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if bytes.Contains(body, []byte(`"text":"block"`)) {
				go func() { <-r.Context().Done(); connectionClosed <- struct{}{} }()
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() { close(release) })
	binding := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(binding, []byte(`{"name":"recording","http":{"params":{"url":"`+httpServer.URL+`"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--binding-file", binding, "record", "--text", "literal", "--count=9007199254740993", "--enabled", "false", "--items", "[\"one\"]", "--object", "{\"nested\":true}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("command: %v stderr=%s", err, &stderr)
	}
	var result mcpsdk.CallToolResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("result JSON: %v: %s", err, &stdout)
	}
	if result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("result=%#v", result)
	}
	got := string(<-calls)
	for _, expected := range []string{`"count":9007199254740993`, `"enabled":false`, `"text":"literal"`, `"nested":true`, `"items":["one"]`} {
		if !strings.Contains(got, expected) {
			t.Fatalf("arguments %s missing %s", got, expected)
		}
	}
	run := func(input string, arguments ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"--binding-file", binding}, arguments...)...)
		cmd.Stdin = strings.NewReader(input)
		var out, diagnostic bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diagnostic
		err := cmd.Run()
		return out.String(), diagnostic.String(), err
	}
	t.Run("JSON preserves omitted and null", func(t *testing.T) {
		out, stderr, err := run(`{"text":"json","nullable":null,"object":{"deep":[null,true,2]}}`, "record", "--input-file", "-")
		if err != nil {
			t.Fatalf("command: %v %s", err, stderr)
		}
		if !json.Valid([]byte(out)) {
			t.Fatal(out)
		}
		got := string(<-calls)
		if !strings.Contains(got, `"nullable":null`) || strings.Contains(got, `"count"`) {
			t.Fatalf("omission/null: %s", got)
		}
	})
	t.Run("file input", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "args.json")
		if err := os.WriteFile(path, []byte(`{"text":"file"}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := run("", "record", "--input-file", path)
		if err != nil {
			t.Fatalf("%v %s", err, stderr)
		}
	})
	for _, tc := range []struct {
		name, input string
		args        []string
	}{
		{"duplicate flag", "", []string{"record", "--text=a", "--text=b"}},
		{"duplicate JSON", "{\"text\":\"a\",\"text\":\"b\"}", []string{"record", "--input-file", "-"}},
		{"nested duplicate", `{"text":"a","object":{"x":1,"x":2}}`, []string{"record", "--input-file", "-"}},
		{"unknown flag", "", []string{"record", "--missing=x"}},
		{"unknown JSON", `{"text":"a","missing":1}`, []string{"record", "--input-file", "-"}},
		{"missing required", "", []string{"record"}},
		{"wrong type", "", []string{"record", "--text=a", "--count=1.5"}},
		{"wrong nested type", "", []string{"record", "--text=a", "--items=[1]"}},
		{"mixed modes", `{"text":"a"}`, []string{"record", "--text=a", "--input-file", "-"}},
		{"mixed modes reversed", `{"text":"a"}`, []string{"record", "--input-file", "-", "--text=a"}},
		{"non object", `[]`, []string{"record", "--input-file", "-"}},
		{"trailing data", `{"text":"a"}{}`, []string{"record", "--input-file", "-"}},
		{"malformed JSON", `{"text":`, []string{"record", "--input-file", "-"}},
		{"unknown tool", "", []string{"excluded"}},
		{"endpoint flag", "", []string{"--endpoint", httpServer.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := count.Load()
			out, stderr, err := run(tc.input, tc.args...)
			if err == nil || stderr == "" || out != "" {
				t.Fatalf("out=%s stderr=%s error=%v", out, stderr, err)
			}
			if count.Load() != before {
				t.Fatal("invalid input dispatched")
			}
		})
	}
	t.Run("help", func(t *testing.T) {
		before := count.Load()
		out, stderr, err := run("", "--help")
		if err != nil || stderr != "" || !strings.Contains(out, "record") || !strings.Contains(out, "Record typed arguments") {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		out, stderr, err = run("", "record", "--help")
		for _, expected := range []string{"--property", "--input-file", "Required: text", `"nullable"`} {
			if !strings.Contains(out, expected) {
				t.Fatalf("help missing %s: %s %s %v", expected, out, stderr, err)
			}
		}
		if count.Load() != before {
			t.Fatal("help dispatched")
		}
	})
	t.Run("tool error envelope", func(t *testing.T) {
		before := count.Load()
		out, stderr, err := run("", "record", "--text=error")
		var result mcpsdk.CallToolResult
		if json.Unmarshal([]byte(out), &result) != nil || !result.IsError || err == nil || !strings.Contains(stderr, "isError") {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		if count.Load() != before+1 {
			t.Fatal("call repeated")
		}
	})
	t.Run("protocol failure no retry", func(t *testing.T) {
		before := count.Load()
		out, stderr, err := run("", "record", "--text=protocol")
		if out != "" || err == nil || stderr == "" {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		if count.Load() != before+1 {
			t.Fatal("call repeated")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		cmd := exec.Command(binary, "--binding-file", binding, "record", "--text=block")
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("call did not start")
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled command succeeded")
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("command did not terminate")
		}
		select {
		case <-connectionClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("local HTTP request did not close")
		}
	})

	bindingData, err := os.ReadFile(binding)
	if err != nil {
		t.Fatal(err)
	}
	writeBinding := func(t *testing.T, value string) {
		t.Helper()
		if err := os.WriteFile(binding, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(binding, bindingData, 0600); err != nil {
				t.Error(err)
			}
		})
	}
	t.Run("selection", func(t *testing.T) {
		writeBinding(t, `{"name":"recording","http":{"params":{"url":"`+httpServer.URL+`"},"tools":["record"]}}`)
		before := count.Load()
		out, stderr, err := run("", "--help")
		if err != nil || strings.Contains(out, "hidden") {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		out, stderr, err = run("", "hidden")
		if err == nil || out != "" || !strings.Contains(stderr, "excluded") {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		if count.Load() != before {
			t.Fatal("excluded tool dispatched")
		}
	})
	for _, tc := range []struct{ name, configuration string }{
		{"protected", `"require_approval":true`},
		{"forwarded headers", `"allowed_headers":["Authorization"]`},
		{"unknown binding option", `"endpoint_override":"http://wrong"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeBinding(t, `{"name":"recording","http":{"params":{"url":"`+httpServer.URL+`"},`+tc.configuration+`}}`)
			before := requests.Load()
			out, stderr, err := run("", "record", "--text=a")
			if err == nil || stderr == "" || out != "" {
				t.Fatalf("%s %s %v", out, stderr, err)
			}
			if requests.Load() != before {
				t.Fatal("invalid binding made MCP request")
			}
		})
	}
	for _, value := range []string{"ambiguous", "redirect"} {
		t.Run(value+" no replay", func(t *testing.T) {
			before := ambiguous.Load()
			out, stderr, err := run("", "record", "--text="+value)
			if err == nil || stderr == "" || out != "" {
				t.Fatalf("%s %s %v", out, stderr, err)
			}
			if ambiguous.Load() != before+1 {
				t.Fatal("ambiguous call replayed")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		writeBinding(t, `{"name":"recording","http":{"params":{"url":"`+httpServer.URL+`","timeout":0.2}}}`)
		before := count.Load()
		begin := time.Now()
		out, stderr, err := run("", "record", "--text=block")
		if err == nil || stderr == "" || out != "" {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		if time.Since(begin) > 5*time.Second {
			t.Fatal("timeout cleanup unbounded")
		}
		if count.Load() != before+1 {
			t.Fatal("timed-out call replayed")
		}
	})

	t.Run("composed schema reserved names exact bound", func(t *testing.T) {
		before := count.Load()
		out, stderr, err := run(`{"help":"original","input-file":"literal","count":9007199254740993}`, "complex", "--input-file", "-")
		if err != nil || !strings.Contains(out, `"source":"recording"`) {
			t.Fatalf("%s %s %v", out, stderr, err)
		}
		if count.Load() != before+1 {
			t.Fatal("complex input not dispatched")
		}
		before = count.Load()
		out, stderr, err = run(`{"help":"original","count":9007199254740992}`, "complex", "--input-file", "-")
		if err == nil || stderr == "" || out != "" || count.Load() != before {
			t.Fatalf("numeric bound not enforced: %s %s %v", out, stderr, err)
		}
	})

	for _, mode := range []string{"signal", "timeout"} {
		t.Run("open stdin "+mode, func(t *testing.T) {
			for len(discovery) > 0 {
				<-discovery
			}
			if mode == "timeout" {
				writeBinding(t, `{"name":"recording","http":{"params":{"url":"`+httpServer.URL+`","timeout":0.3}}}`)
			}
			before := count.Load()
			cmd := exec.Command(binary, "--binding-file", binding, "record", "--input-file", "-")
			pipe, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pipe.Close(); err != nil {
					t.Log(err)
				}
			}()
			var out, diagnostic bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diagnostic
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-discovery:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("discovery did not complete")
			}
			if mode == "signal" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil || diagnostic.Len() == 0 || out.Len() != 0 {
					t.Fatalf("%s %s %v", &out, &diagnostic, err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("open stdin blocked cancellation")
			}
			if count.Load() != before {
				t.Fatal("incomplete stdin dispatched")
			}
		})
	}

	for _, value := range []string{"future-result", "input-required"} {
		t.Run("unsupported "+value, func(t *testing.T) {
			before := ambiguous.Load()
			out, stderr, err := run("", "record", "--text="+value)
			if err == nil || !strings.Contains(stderr, "unsupported") || !json.Valid([]byte(out)) {
				t.Fatalf("unsupported result silently accepted: %s %s %v", out, stderr, err)
			}
			if ambiguous.Load() != before+1 {
				t.Fatal("unsupported result replayed")
			}
		})
	}
	t.Run("result numeric fidelity", func(t *testing.T) {
		out, stderr, err := run("", "record", "--text=precision")
		if err != nil {
			t.Fatalf("%s %v", stderr, err)
		}
		for _, expected := range []string{`"positive":9007199254740993`, `"negative":-9007199254740993`, `"nested":[9007199254740993,-9007199254740993]`} {
			if !strings.Contains(out, expected) {
				t.Fatalf("result corrupted %s: %s", expected, out)
			}
		}
	})
	for _, value := range []string{"wire-json", "wire-sse", "wire-error"} {
		t.Run("complete envelope "+value, func(t *testing.T) {
			before := ambiguous.Load()
			out, stderr, err := run("", "record", "--text="+value)
			if (err != nil) != (value == "wire-error") {
				t.Fatalf("%s %s %v", out, stderr, err)
			}
			for _, expected := range []string{`"integer":9007199254740993`, `"positive":9007199254740993`, `"negative":-9007199254740993`, `"nested":[9007199254740993,-9007199254740993]`, `"decimal":1.234567890123456789`, `"tiny":1e-400`} {
				if !strings.Contains(out, expected) {
					t.Fatalf("result corrupted %s: %s", expected, out)
				}
			}
			if ambiguous.Load() != before+1 {
				t.Fatal("result replayed")
			}
		})
	}
	t.Run("fractional schema bound and help", func(t *testing.T) {
		before := count.Load()
		out, stderr, err := run("", "fractional", "--number=1")
		if err == nil || out != "" || stderr == "" || count.Load() != before {
			t.Fatalf("invalid fractional input dispatched: %s %s %v", out, stderr, err)
		}
		out, stderr, err = run("", "fractional", "--help")
		if err != nil || !strings.Contains(out, `"minimum":1.0000000000000001`) {
			t.Fatalf("help rounded schema: %s %s %v", out, stderr, err)
		}
		out, stderr, err = run("", "fractional", "--number=1.0000000000000001")
		if err != nil || count.Load() != before+1 {
			t.Fatalf("exact fractional bound rejected: %s %s %v", out, stderr, err)
		}
	})
}
