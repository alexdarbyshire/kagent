package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCLICollectionManifestLimitThroughRunCLI(t *testing.T) {
	// The public runner can receive names larger than the operating system's
	// per-argument executable limit. The manifest still has its own bound.
	group := strings.Repeat("x", 1<<20)
	var calls atomic.Int32
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "manifest", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "result", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}, StructuredContent: map[string]any{group: []any{}}}, nil
	})
	remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer remote.Close()
	directory := t.TempDir()
	binding := filepath.Join(directory, "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"manifest","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
	rawPath, artifactPath := filepath.Join(directory, "raw.json"), filepath.Join(directory, "artifacts")
	var stdout bytes.Buffer
	err := RunCLI(t.Context(), []string{"--binding-file", binding, "result", "--save-text-dir", group + ".text=" + artifactPath, "--output-file", rawPath}, strings.NewReader(""), &stdout)
	require.ErrorContains(t, err, "manifest exceeds 1 MiB")
	require.Equal(t, int32(1), calls.Load())
	require.Empty(t, stdout.String())
	raw, err := os.ReadFile(rawPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), group)
	_, err = os.Stat(artifactPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}
