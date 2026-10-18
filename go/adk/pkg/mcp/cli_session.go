package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"

	"go.opentelemetry.io/otel/propagation"

	"github.com/kagent-dev/kagent/go/api/adk"
)

type cliCommandContextKey struct{}

type cliCommandRuntime struct {
	ctx       context.Context
	lifecycle *ClientLifecycle
	bindings  map[string]adk.MCPCLIConfig
	resolvers map[string]*headerRoundTripper
}

// cliCommandHeaders holds the current invocation only during a serialized command.
// It never stores credentials or a previous caller's context in a retained session.
type cliCommandHeaders struct {
	base       http.RoundTripper
	resolver   *headerRoundTripper
	mu         sync.Mutex
	invocation context.Context
	lifecycle  *ClientLifecycle
}

var _ http.RoundTripper = (*cliCommandHeaders)(nil)

func (c *cliCommandHeaders) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodDelete {
		return c.base.RoundTrip(request)
	}
	invocation, ok := request.Context().Value(cliCommandContextKey{}).(context.Context)
	if !ok {
		// Initialization has the actor context so a retained SSE stream cannot
		// capture the first invocation; ordinary requests carry their own caller.
		c.mu.Lock()
		invocation = c.invocation
		c.mu.Unlock()
	}
	request = request.Clone(request.Context())
	if invocation != nil {
		if err := invocation.Err(); err != nil {
			return nil, err
		}
		headers := c.resolver.resolveHeaders(invocation)
		propagation.TraceContext{}.Inject(invocation, propagation.HeaderCarrier(headers))
		for name, values := range headers {
			request.Header[http.CanonicalHeaderKey(name)] = values
		}
	}
	return c.base.RoundTrip(request)
}

func (c *cliCommandHeaders) bind(invocation context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invocation = invocation
}

func (c *cliCommandHeaders) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invocation = nil
}

func (c *cliCommandRuntime) run(ctx, invocation context.Context, name string, request cliCommandRequest) cliCommandResponse {
	if err := request.validate(); err != nil {
		return cliResponse(cliFailure{Err: err})
	}
	ctx, cancel := context.WithCancel(ctx)
	if transform, ok := c.ctx.Value(resultTransformerKey{}).(ResultTransformer); ok {
		ctx = WithResultTransformer(ctx, transform)
	}
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	binding, ok := c.bindings[name]
	if !ok {
		return cliResponse(cliFailure{Err: errors.New("unknown MCP invocation binding")})
	}
	key, err := c.lifecycle.scopeKey(invocation, name+bindingIdentity(binding), "command", c.resolvers[name])
	if err != nil {
		return cliResponse(cliFailure{Err: err})
	}
	session, err := c.lifecycle.relationship(key)
	if err != nil {
		return cliResponse(cliFailure{Err: err})
	}
	release, err := c.lifecycle.acquire(ctx, session)
	if err != nil {
		return cliResponse(cliFailure{Err: err})
	}
	defer release()
	if session.headers == nil {
		session.headers = &cliCommandHeaders{resolver: c.resolvers[name], lifecycle: c.lifecycle}
	}
	session.headers.bind(invocation)
	defer session.headers.release()
	if session.command == nil {
		// Cancel incomplete initialization with this call, then detach the healthy
		// connection from the invocation without retaining its context.
		connection, cancelConnection := context.WithCancel(c.ctx)
		stopInitialization := context.AfterFunc(ctx, cancelConnection)
		session.command, err = openCLISession(connection, binding, session.headers)
		stopInitialization()
		if err != nil {
			cancelConnection()
		} else {
			cancelRemote := session.command.cancel
			session.command.cancel = func() { cancelRemote(); cancelConnection() }
		}
	}
	var outcome cliOutcome = cliFailure{Err: err}
	if err == nil {
		// Reset captured wire state between commands, without changing session identity.
		session.command.lifecycle.reset()
		outcome = runCLICommand(context.WithValue(context.WithValue(ctx, operationKey{}, &clientOperation{}), cliCommandContextKey{}, invocation), binding, request, session.command)
	}
	if err := cliOutcomeError(outcome); err != nil && session.command != nil && session.command.managed.stateLost() {
		failure := fmt.Errorf("MCP relationship state lost; operation not replayed: %w", errors.Join(err, session.command.Close()))
		session.command = nil
		if result, ok := outcome.(cliResult); ok {
			result.Err = failure
			outcome = result
		} else {
			outcome = cliFailure{Err: failure}
		}
	}
	response := cliResponse(outcome)
	return response
}

func (c *cliCommandRuntime) close() {
	c.lifecycle.Close()
}

func cliCommandSocket() string {
	socket, _ := os.LookupEnv(cliInvocationSocket) //nolint:forbidigo // Private per-Bash runtime handoff, not a user setting.
	return socket
}

func runActorCLI(ctx context.Context, socket, name string, command cliCommandRequest, stdout io.Writer) error {
	data, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("failed to encode MCP command: %w", err)
	}
	pool := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer pool.CloseIdleConnections()
	client := &http.Client{Transport: pool}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://invocation/command/"+url.PathEscape(name), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create MCP command request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("MCP invocation command failed (not retried): %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP invocation command failed with status %d", response.StatusCode)
	}
	data, err = io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read MCP command result: %w", err)
	}
	result, err := decodeCLIResponse(data)
	if err != nil {
		return fmt.Errorf("failed to decode MCP command result: %w", err)
	}
	outcome, err := result.outcome(name, command)
	if err != nil {
		return err
	}
	return renderCLIOutcome(stdout, outcome)
}
