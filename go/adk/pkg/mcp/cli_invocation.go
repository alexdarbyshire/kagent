package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"go.opentelemetry.io/otel/propagation"
)

// cliInvocationSocket is the private subprocess handoff location. Its value
// carries no credential and is valid only for the lifetime of one Bash call.
const cliInvocationSocket = "KAGENT_MCP_CLI_INVOCATION_SOCKET"

// PrepareCLIEnvironment owns MCP command sessions for the actor's lifetime.
// Each execution hook binds its private socket to the original agent context;
// command execution only installs the returned environment.
func PrepareCLIEnvironment(owner context.Context, bindings []adk.MCPCLIConfig, propagateToken bool, provider DynamicHeaderProvider, lifecycles ...*ClientLifecycle) func(context.Context, context.Context) ([]string, func(), error) {
	var ownerLifecycle *ClientLifecycle
	if len(lifecycles) > 0 {
		ownerLifecycle = lifecycles[0]
	}
	if ownerLifecycle == nil {
		ownerLifecycle = NewClientLifecycle(owner, nil)
	}
	resolvers := make(map[string]*headerRoundTripper, len(bindings))
	for _, binding := range bindings {
		headers, allowed := binding.HTTP.Params.Headers, binding.HTTP.AllowedHeaders
		if binding.SSE != nil {
			headers, allowed = binding.SSE.Params.Headers, binding.SSE.AllowedHeaders
		}
		resolvers[binding.Name] = &headerRoundTripper{headers: headers, allowedHeaders: allowed, propagateToken: propagateToken, headerProvider: provider}
	}
	commands := &cliCommandRuntime{ctx: owner, lifecycle: ownerLifecycle, bindings: make(map[string]adk.MCPCLIConfig), resolvers: resolvers}
	for _, binding := range bindings {
		commands.bindings[binding.Name] = binding
	}
	context.AfterFunc(owner, commands.close)
	return func(invocation, lifetime context.Context) ([]string, func(), error) {
		if err := owner.Err(); err != nil {
			return nil, nil, fmt.Errorf("MCP command runtime stopped: %w", err)
		}
		directory, err := os.MkdirTemp("", "kcli-")
		if err != nil {
			return nil, nil, fmt.Errorf("create MCP invocation directory: %w", err)
		}
		socket := filepath.Join(directory, "headers.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("open MCP invocation socket: %w", err), os.RemoveAll(directory))
		}
		if err := os.Chmod(socket, 0o600); err != nil {
			return nil, nil, errors.Join(err, listener.Close(), os.RemoveAll(directory))
		}
		server := &http.Server{
			ReadHeaderTimeout: time.Second,
			BaseContext:       func(net.Listener) context.Context { return lifetime },
			Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/command/") {
					if lifetime.Err() != nil || invocation.Err() != nil {
						http.Error(w, "MCP invocation ended", http.StatusGone)
						return
					}
					var input cliCommandRequest
					decoder := json.NewDecoder(request.Body)
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(&input); err != nil {
						http.Error(w, "invalid MCP command", http.StatusBadRequest)
						return
					}
					result := commands.run(request.Context(), invocation, strings.TrimPrefix(request.URL.Path, "/command/"), input)
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(result); err != nil {
						logging.FromContext(invocation).DebugContext(invocation, "MCP command handoff ended", "error", err)
					}
					return
				}
				name := strings.TrimPrefix(request.URL.Path, "/headers/")
				resolver, ok := resolvers[name]
				if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/headers/") || !ok {
					http.Error(w, "unknown MCP invocation binding", http.StatusNotFound)
					return
				}
				if lifetime.Err() != nil || invocation.Err() != nil {
					http.Error(w, "MCP invocation ended", http.StatusGone)
					return
				}
				// Do not derive this context with WithTimeout: STS needs the
				// SessionID method on the original ADK invocation context.
				headers := resolver.resolveHeaders(invocation)
				propagation.TraceContext{}.Inject(invocation, propagation.HeaderCarrier(headers))
				if lifetime.Err() != nil || invocation.Err() != nil {
					http.Error(w, "MCP invocation ended", http.StatusGone)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(headers); err != nil {
					logging.FromContext(invocation).DebugContext(invocation, "MCP invocation header handoff ended", "error", err)
				}
			}),
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logging.FromContext(invocation).ErrorContext(invocation, "MCP invocation socket stopped", "error", err)
			}
		}()
		var once sync.Once
		cleanup := func() {
			once.Do(func() {
				closeErr := server.Close()
				<-done
				if err := errors.Join(closeErr, os.RemoveAll(directory)); err != nil {
					logging.FromContext(invocation).ErrorContext(invocation, "failed to remove MCP invocation socket", "error", err)
				}
			})
		}
		stop := context.AfterFunc(lifetime, cleanup)
		stopOwner := context.AfterFunc(owner, cleanup)
		if err := owner.Err(); err != nil {
			stop()
			stopOwner()
			cleanup()
			return nil, nil, fmt.Errorf("MCP command runtime stopped: %w", err)
		}
		return []string{cliInvocationSocket + "=" + socket}, func() { stop(); stopOwner(); cleanup() }, nil
	}
}

// cliInvocationTransport resolves headers from the parent for every remote
// request. Static commands may run without a parent; forwarded credentials may
// never silently fall back to an unauthenticated command.
func cliInvocationTransport(base http.RoundTripper, binding adk.MCPCLIConfig) (http.RoundTripper, error) {
	socket, _ := os.LookupEnv(cliInvocationSocket) //nolint:forbidigo // Private per-process handoff, not a configurable runtime setting.
	required := binding.RequiresInvocationHeaders || len(binding.HTTP.AllowedHeaders) > 0 || (binding.SSE != nil && len(binding.SSE.AllowedHeaders) > 0)
	if socket == "" {
		if required {
			return nil, errors.New("MCP command requires a live invocation header handoff")
		}
		return base, nil
	}
	pool := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &cliInvocationRoundTripper{base: base, client: &http.Client{Transport: pool}, name: binding.Name}, nil
}

type cliInvocationRoundTripper struct {
	base   http.RoundTripper
	client *http.Client
	name   string
}

func (c *cliInvocationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	lookup, err := http.NewRequestWithContext(request.Context(), http.MethodGet, "http://invocation/headers/"+url.PathEscape(c.name), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(lookup)
	if err != nil {
		return nil, fmt.Errorf("MCP invocation header lookup failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP invocation header lookup failed with status %d", response.StatusCode)
	}
	var headers http.Header
	if err := json.NewDecoder(response.Body).Decode(&headers); err != nil {
		return nil, fmt.Errorf("invalid MCP invocation header response: %w", err)
	}
	request = request.Clone(request.Context())
	request = request.WithContext(propagation.TraceContext{}.Extract(request.Context(), propagation.HeaderCarrier(headers)))
	for name, values := range headers {
		request.Header[http.CanonicalHeaderKey(name)] = values
	}
	return c.base.RoundTrip(request)
}
