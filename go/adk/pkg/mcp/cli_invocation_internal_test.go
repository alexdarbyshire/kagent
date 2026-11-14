package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/api/adk"
)

type cliSessionContext struct {
	context.Context
	id string
}

func (c cliSessionContext) SessionID() string      { return c.id }
func (c cliSessionContext) AppName() string        { return "cli-test" }
func (c cliSessionContext) UserID() string         { return c.id }
func (c cliSessionContext) AgentName() string      { return "cli-test" }
func (c cliSessionContext) Branch() string         { return "" }
func (c cliSessionContext) IsolationScope() string { return "" }

func TestCLIInvocationRejectsOtherBindingAndClosesOnCancellation(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer remote.Close()
	ctx, cancel := context.WithCancel(t.Context())
	binding := adk.MCPCLIConfig{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: remote.URL}}}
	environment, cleanup, err := PrepareCLIEnvironment(t.Context(), []adk.MCPCLIConfig{binding}, false, nil)(ctx, ctx)
	require.NoError(t, err)
	defer cleanup()
	socket := strings.TrimPrefix(environment[0], cliInvocationSocket+"=")
	info, err := os.Stat(socket)
	require.NoError(t, err)
	require.EqualValues(t, 0o600, info.Mode().Perm())
	info, err = os.Stat(filepath.Dir(socket))
	require.NoError(t, err)
	require.EqualValues(t, 0o700, info.Mode().Perm())
	t.Setenv(cliInvocationSocket, socket)
	binding.Name = "foreign"
	transport, err := cliInvocationTransport(http.DefaultTransport, binding)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, remote.URL, nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.ErrorContains(t, err, "status 404")
	require.Zero(t, requests.Load())
	cancel()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Dir(socket)); return os.IsNotExist(err) }, time.Second, time.Millisecond)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	require.Zero(t, requests.Load())
}
