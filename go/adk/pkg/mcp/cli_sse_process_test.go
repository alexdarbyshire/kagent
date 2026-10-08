package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCLIProcessLegacySSE(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	var calls atomic.Int32
	arguments := make(chan json.RawMessage, 10)
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "legacy", Version: "1"}, &mcpsdk.ServerOptions{PageSize: 1})
	server.AddTool(&mcpsdk.Tool{Name: "aaa", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		t.Error("excluded pagination fixture was invoked")
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
	})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server.AddTool(&mcpsdk.Tool{Name: "block", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-ctx.Done():
		case <-release:
		}
		return nil, fmt.Errorf("fixture canceled")
	})
	server.AddTool(&mcpsdk.Tool{Name: "record", Description: "Record an exact integer", InputSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer","minimum":9007199254740993}},"required":["count"]}`)}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		arguments <- req.Params.Arguments
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}, StructuredContent: map[string]any{"count": int64(9007199254740993)}}, nil
	})
	handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var ambiguous atomic.Int32
	streamClosed := make(chan struct{}, 20)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/gateway/") || r.Header.Get("X-Configured") != "retained" {
			http.Error(w, "wrong route or header", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet {
			go func() {
				<-r.Context().Done()
				streamClosed <- struct{}{}
			}()
		}
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			if bytes.Contains(data, []byte(`"method":"tools/call"`)) && r.Header.Get("X-Delay") == "true" {
				timer := time.NewTimer(1250 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return
				}
			}
			if bytes.Contains(data, []byte(`"method":"tools/call"`)) && bytes.Contains(data, []byte(`"name":"record"`)) && r.Header.Get("X-Ambiguous") == "true" {
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
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	t.Cleanup(func() { close(release) })
	binding := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(binding, []byte(`{"name":"legacy","sse":{"params":{"url":"`+endpoint.URL+`/gateway/sse","headers":{"X-Configured":"retained"},"timeout":10,"sse_read_timeout":15},"tools":["record"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--binding-file", binding}, args...)...)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		err := cmd.Run()
		return out.String(), diagnostic.String(), err
	}
	out, diagnostic, err := run("record", "--help")
	if err != nil || !strings.Contains(out, "9007199254740993") {
		t.Fatalf("exact SSE schema: err=%v stdout=%s stderr=%s", err, out, diagnostic)
	}
	out, diagnostic, err = run("record", "--count", "9007199254740993")
	if err != nil || !strings.Contains(out, `"count":9007199254740993`) || diagnostic != "" {
		t.Fatalf("exact SSE result: err=%v stdout=%s stderr=%s", err, out, diagnostic)
	}
	if got := string(<-arguments); !strings.Contains(got, `"count":9007199254740993`) {
		t.Fatalf("arguments lost precision: %s", got)
	}
	_, diagnostic, err = run("record", "--count", "9007199254740992")
	if err == nil || !strings.Contains(diagnostic, "invalid arguments") || calls.Load() != 1 {
		t.Fatalf("exact SSE validation/call count: err=%v stderr=%s calls=%d", err, diagnostic, calls.Load())
	}
	t.Run("ambiguous POST is not replayed", func(t *testing.T) {
		if err := os.WriteFile(binding, []byte(`{"name":"legacy","sse":{"params":{"url":"`+endpoint.URL+`/gateway/sse","headers":{"X-Configured":"retained","X-Ambiguous":"true","Idempotency-Key":"fixture"},"timeout":2},"tools":["record"]}}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, diagnostic, err := run("record", "--count", "9007199254740993")
		if err == nil || ambiguous.Load() != 1 || !strings.Contains(diagnostic, "not retried") {
			t.Fatalf("ambiguous dispatch: err=%v stderr=%s calls=%d", err, diagnostic, ambiguous.Load())
		}
	})
	t.Run("timeout closes SSE after one call", func(t *testing.T) {
		for len(streamClosed) > 0 {
			<-streamClosed
		}
		if err := os.WriteFile(binding, []byte(`{"name":"legacy","sse":{"params":{"url":"`+endpoint.URL+`/gateway/sse","headers":{"X-Configured":"retained"},"timeout":0.5},"tools":["block"]}}`), 0600); err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		_, diagnostic, err := run("block")
		if err == nil || time.Since(begin) > 3*time.Second || calls.Load() != 2 || !strings.Contains(diagnostic, "not retried") {
			t.Fatalf("bounded timeout: err=%v stderr=%s calls=%d", err, diagnostic, calls.Load())
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("blocking tool was not called")
		}
		select {
		case <-streamClosed:
		case <-time.After(time.Second):
			t.Fatal("SSE connection outlived command timeout")
		}
	})
	t.Run("read timeout retains native larger stream budget", func(t *testing.T) {
		if err := os.WriteFile(binding, []byte(`{"name":"legacy","sse":{"params":{"url":"`+endpoint.URL+`/gateway/sse","headers":{"X-Configured":"retained","X-Delay":"true"},"timeout":1,"sse_read_timeout":3},"tools":["record"]}}`), 0600); err != nil {
			t.Fatal(err)
		}
		out, diagnostic, err := run("record", "--count", "9007199254740993")
		if err != nil || !strings.Contains(out, `"count":9007199254740993`) || calls.Load() != 3 {
			t.Fatalf("SSE timeout fidelity: err=%v stdout=%s stderr=%s calls=%d", err, out, diagnostic, calls.Load())
		}
	})
	t.Run("reject changed origin before credential dispatch", func(t *testing.T) {
		var reached atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}))
		defer other.Close()
		stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if _, err := io.WriteString(w, "event: endpoint\ndata: "+other.URL+"/messages\n\n"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer stream.Close()
		if err := os.WriteFile(binding, []byte(`{"name":"legacy","sse":{"params":{"url":"`+stream.URL+`","headers":{"Authorization":"Bearer fixture"},"timeout":2}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, diagnostic, err := run("--help")
		if err == nil || reached.Load() != 0 || !strings.Contains(diagnostic, "origin") {
			t.Fatalf("cross-origin dispatch: err=%v stderr=%s requests=%d", err, diagnostic, reached.Load())
		}
	})
	for _, test := range []struct {
		name, input, diagnostic string
	}{
		{"ambiguous transports", `"http":{"params":{"url":"http://unused"}},"sse":{"params":{"url":"http://unused"}}`, "ambiguous"},
		{"negative operation timeout", `"sse":{"params":{"url":"http://unused","timeout":-1}}`, "timeout"},
		{"negative read timeout", `"sse":{"params":{"url":"http://unused","sse_read_timeout":-1}}`, "read timeout"},
		{"overflow read timeout", `"sse":{"params":{"url":"http://unused","sse_read_timeout":1e20}}`, "read timeout"},
		{"userinfo endpoint", `"sse":{"params":{"url":"http://user:pass@unused"}}`, "endpoint"},
		{"protected SSE", `"sse":{"params":{"url":"http://unused"},"require_approval":true}`, "require_approval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(binding, []byte(`{"name":"legacy",`+test.input+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			_, diagnostic, err := run("--help")
			if err == nil || !strings.Contains(diagnostic, test.diagnostic) {
				t.Fatalf("invalid input: err=%v stderr=%s", err, diagnostic)
			}
		})
	}
}

func TestCLIProcessSSETLS(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "tls", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "record", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}, StructuredContent: map[string]any{"ok": true}}, nil
	})
	endpoint := httptest.NewTLSServer(mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer endpoint.Close()
	certificate := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options map[string]any
		ok      bool
	}{
		{"untrusted certificate", nil, false},
		{"custom CA without system roots", map[string]any{"tls_ca_cert_path": certificate, "tls_disable_system_cas": true}, true},
		{"custom CA with system roots", map[string]any{"tls_ca_cert_path": certificate}, true},
		{"explicit disable verification", map[string]any{"tls_insecure_skip_verify": true}, true},
		{"disable system roots without CA", map[string]any{"tls_disable_system_cas": true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := map[string]any{"url": endpoint.URL, "timeout": 3}
			maps.Copy(params, test.options)
			input, err := json.Marshal(map[string]any{"name": "tls", "sse": map[string]any{"params": params}})
			if err != nil {
				t.Fatal(err)
			}
			binding := filepath.Join(t.TempDir(), "binding.json")
			if err := os.WriteFile(binding, input, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--binding-file", binding, "record")
			var out, diagnostic bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &diagnostic
			err = cmd.Run()
			if test.ok && (err != nil || !strings.Contains(out.String(), `"ok":true`)) {
				t.Fatalf("TLS invocation failed: %v stdout=%s stderr=%s", err, &out, &diagnostic)
			}
			if !test.ok && err == nil {
				t.Fatalf("unsupported trust succeeded: stdout=%s stderr=%s", &out, &diagnostic)
			}
		})
	}
	if calls.Load() != 3 {
		t.Fatalf("unexpected TLS tool dispatches: %d", calls.Load())
	}
}
