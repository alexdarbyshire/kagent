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
	"strings"
	"sync"

	"github.com/kagent-dev/kagent/go/api/adk"
	"go.opentelemetry.io/otel/propagation"
)

// Integrated commands send only arguments; binding and authority stay in the actor.
type cliCommandRequest struct {
	Args  []string `json:"args"`
	Input []byte   `json:"input,omitempty"`
}

type cliCommandResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

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
	ctx, cancel := context.WithCancel(ctx)
	if transform, ok := c.ctx.Value(resultTransformerKey{}).(ResultTransformer); ok {
		ctx = WithResultTransformer(ctx, transform)
	}
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	binding, ok := c.bindings[name]
	if !ok {
		return cliCommandResponse{Error: "unknown MCP invocation binding"}
	}
	key, err := c.lifecycle.scopeKey(invocation, name+bindingIdentity(binding), "command", c.resolvers[name])
	if err != nil {
		return cliCommandResponse{Error: err.Error()}
	}
	session, err := c.lifecycle.relationship(key)
	if err != nil {
		return cliCommandResponse{Error: err.Error()}
	}
	release, err := c.lifecycle.acquire(ctx, session)
	if err != nil {
		return cliCommandResponse{Error: err.Error()}
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
	var output bytes.Buffer
	if err == nil {
		// Reset captured wire state between commands, without changing session identity.
		session.command.lifecycle.reset()
		err = runCLICommand(context.WithValue(context.WithValue(ctx, operationKey{}, &clientOperation{}), cliCommandContextKey{}, invocation), binding, request.Args, io.NopCloser(bytes.NewReader(request.Input)), &output, session.command)
	}
	response := cliCommandResponse{Output: output.String()}
	if err != nil {
		response.Error = err.Error()
		if session.command != nil && session.command.managed.stateLost() {
			_ = session.command.Close()
			session.command = nil
			response.Error = "MCP relationship state lost; operation not replayed: " + response.Error
		}
	}
	return response
}

func (c *cliCommandRuntime) close() {
	c.lifecycle.Close()
}

func cliCommandSocket() string {
	socket, _ := os.LookupEnv(cliInvocationSocket) //nolint:forbidigo // Private per-Bash runtime handoff, not a user setting.
	return socket
}

func runActorCLI(ctx context.Context, socket, name string, args []string, stdin io.Reader, stdout io.Writer) error {
	command := cliCommandRequest{Args: append([]string(nil), args...)}
	// Files belong to the subprocess working directory, not the actor's cwd.
	for i := 1; i < len(command.Args); i++ {
		flag, value, inline := strings.Cut(command.Args[i], "=")
		if flag != "--input-file" {
			if !inline {
				i++
			}
			continue
		}
		if !inline {
			if i+1 >= len(command.Args) {
				return errors.New("missing value for --input-file")
			}
			value = command.Args[i+1]
		}
		if i != 1 || (inline && len(command.Args) != 2) || (!inline && len(command.Args) != 3) {
			return errors.New("--input-file cannot be combined with argument flags")
		}
		var err error
		if value == "-" {
			reader, ok := stdin.(io.ReadCloser)
			if !ok {
				return errors.New("MCP CLI stdin must support cancellation by closing")
			}
			command.Input, err = readCLIInput(ctx, reader)
		} else {
			command.Input, err = readCLIFile(ctx, value)
		}
		if err != nil {
			return fmt.Errorf("failed to read command input: %w", err)
		}
		if inline {
			command.Args[i] = "--input-file=-"
		} else {
			command.Args[i+1] = "-"
			i++
		}
	}
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
	var result cliCommandResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode MCP command result: %w", err)
	}
	if _, err := io.WriteString(stdout, result.Output); err != nil {
		return fmt.Errorf("failed to write MCP command result: %w", err)
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
