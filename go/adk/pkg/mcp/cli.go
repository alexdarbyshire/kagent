package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	schemaValidator "github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// cliBinding is a private runtime input, not a model-authored connection API.
type cliBinding = adk.MCPCLIConfig

// RunCLI executes the MCP command interface using a private binding file.
// The executable owns signal handling and stderr diagnostics.
func RunCLI(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (err error) {
	if len(args) > 0 && args[0] == "extract" {
		return runCLIExtract(ctx, args, stdout)
	}
	if len(args) < 2 || args[0] != "--binding-file" {
		return cliUsageErrorf("usage: mcp-cli --binding-file PATH [tool [--property value | --input-file PATH|-] | --help]")
	}
	data, err := readCLIFile(ctx, args[1])
	if err != nil {
		return fmt.Errorf("failed to read MCP binding: %w", err)
	}
	if _, err := decodeJSON(data); err != nil {
		return fmt.Errorf("invalid MCP binding: %w", err)
	}
	var binding cliBinding
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return fmt.Errorf("invalid MCP binding: %w", err)
	}
	if binding.Name == "" {
		return errors.New("MCP binding requires a command name")
	}
	command, err := parseCLICommand(args[2:])
	if err != nil {
		return err
	}
	socket := cliCommandSocket()
	if socket == "" {
		if err := binding.ValidateTransport(); err != nil {
			return err
		}
		params, _, _ := cliConnection(binding)
		timeout, err := cliTimeout(params)
		if err != nil {
			return err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := command.prepare(ctx, stdin)
	if err != nil {
		return err
	}
	outputs, err := reserveCLIOutputs(ctx, command)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, outputs.finish(ctx)) }()
	if socket != "" {
		outcome, err := runActorCLI(ctx, socket, binding.Name, request)
		if err != nil {
			return err
		}
		return outputs.handle(ctx, stdout, outcome, command.save)
	}
	remote, err := openCLISession(ctx, binding, nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, remote.Close()) }()
	return outputs.handle(ctx, stdout, runCLICommand(ctx, binding, request, remote), command.save)
}

// cliRemoteSession owns transport resources independently of a command process.
type cliRemoteSession struct {
	session   *mcpsdk.ClientSession
	lifecycle *cliHTTPTransport
	pool      *http.Transport
	cancel    context.CancelFunc
	timeout   time.Duration
	managed   *managedHTTPTransport
}

func (c *cliRemoteSession) Close() error {
	var err error
	if c.session != nil {
		err = c.session.Close()
	}
	c.cancel()
	c.pool.CloseIdleConnections()
	if c.managed != nil {
		if err != nil {
			c.managed.recordFailure(err)
		}
		outcome := c.managed.outcome()
		if outcome.Status == "not_attempted" {
			outcome.Status = "no_session"
			if c.managed.legacySSE {
				outcome.Status = "unsupported_transport"
			}
		}
		if outcome.Status != "accepted" && outcome.Status != "no_session" && outcome.Status != "unsupported_transport" {
			logging.FromContext(context.Background()).Warn("MCP command termination outcome", "endpoint", outcome.Endpoint, "status", outcome.Status, "http_status", outcome.HTTPStatus)
		}
	}
	return err
}

func openCLISession(ctx context.Context, binding cliBinding, headers *cliCommandHeaders) (_ *cliRemoteSession, err error) {
	if err := binding.ValidateTransport(); err != nil {
		return nil, err
	}
	params, _, requireApproval := cliConnection(binding)
	if requireApproval {
		return nil, errors.New("MCP CLI does not support require_approval: true; retain native MCP exposure")
	}
	endpoint, err := url.Parse(params.URL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("MCP binding requires an HTTP endpoint")
	}
	if params.ServerType == "http" && params.SseReadTimeout != nil {
		return nil, errors.New("MCP CLI HTTP sse_read_timeout is unsupported")
	}
	if binding.SSE == nil && binding.HTTP.Params.TerminateOnClose != nil && !*binding.HTTP.Params.TerminateOnClose {
		return nil, errors.New("MCP CLI requires terminate_on_close to close session resources")
	}
	if params.TLSDisableSystemCAs != nil && *params.TLSDisableSystemCAs && (params.TLSCACertPath == nil || *params.TLSCACertPath == "") {
		return nil, errors.New("tls_disable_system_cas requires tls_ca_cert_path")
	}
	timeout, err := cliTimeout(params)
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	if headers == nil {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		// A retained SSE stream belongs to the actor, not its first command's deadline.
		ctx, cancel = context.WithCancel(ctx)
	}

	pool := &http.Transport{}
	remoteSession := &cliRemoteSession{pool: pool, cancel: cancel, timeout: timeout}
	defer func() {
		if err != nil {
			err = errors.Join(err, remoteSession.Close())
		}
	}()

	params.HTTPTransport = pool
	transport, err := createTransport(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP transport: %w", err)
	}
	var httpClient *http.Client
	switch remote := transport.(type) {
	case *mcpsdk.StreamableClientTransport:
		// This command must never resume or replay an ambiguous tool call.
		remote.MaxRetries = -1
		remote.DisableStandaloneSSE = true
		httpClient = remote.HTTPClient
	case *mcpsdk.SSEClientTransport:
		httpClient = remote.HTTPClient
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	invocation := httpClient.Transport
	if headers != nil {
		headers.base = invocation
		invocation = headers
	} else {
		invocation, err = cliInvocationTransport(invocation, binding)
		if err != nil {
			return nil, err
		}
	}
	lifecycle := &cliHTTPTransport{base: invocation, endpoint: endpoint, legacySSE: binding.SSE != nil}
	httpClient.Transport = lifecycle
	resolver := &headerRoundTripper{headers: params.Headers, allowedHeaders: params.AllowedHeaders}
	var provider LifecycleAuthorityProvider
	if headers != nil {
		resolver = headers.resolver
		provider = headers.lifecycle.provider
	}
	managed := &managedHTTPTransport{base: lifecycle, resolver: resolver, provider: provider, endpoint: params.URL, legacySSE: binding.SSE != nil, commandHeaders: headers}
	if headers != nil {
		headers.mu.Lock()
		managed.capture(headers.invocation)
		headers.mu.Unlock()
	}
	httpClient.Transport = managed
	remoteSession.managed = managed
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "kagent-mcp-cli", Version: "1"}, &mcpsdk.ClientOptions{Capabilities: &mcpsdk.ClientCapabilities{}, MultiRoundTrip: &mcpsdk.MultiRoundTripOptions{Disabled: true}})
	if headers != nil {
		transport = &registeredTransport{inner: transport, owner: headers.lifecycle, managed: managed, pool: pool, cancel: cancel}
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect MCP command %s: %w", binding.Name, lifecycle.connectionError(err))
	}

	remoteSession.session, remoteSession.lifecycle = session, lifecycle
	return remoteSession, nil
}

func runCLICommand(ctx context.Context, binding cliBinding, request cliCommandRequest, remote *cliRemoteSession) cliOutcome {
	ctx, cancel := context.WithTimeout(ctx, remote.timeout)
	defer cancel()
	session, lifecycle := remote.session, remote.lifecycle
	_, selectedTools, _ := cliConnection(binding)
	var tools []*mcpsdk.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return cliFailure{Err: fmt.Errorf("failed to discover MCP tools: %w", lifecycle.connectionError(err))}
		}
		if len(selectedTools) == 0 || slices.Contains(selectedTools, tool.Name) {
			tools = append(tools, tool)
		}
	}
	if request.Kind == cliCatalogKind {
		help := cliCatalogHelp{Command: binding.Name}
		for _, tool := range tools {
			help.Tools = append(help.Tools, cliToolDescription{Name: tool.Name, Description: tool.Description})
		}
		return help
	}
	var selected *mcpsdk.Tool
	for _, tool := range tools {
		if tool.Name == request.Tool {
			selected = tool
			break
		}
	}
	if selected == nil {
		return cliFailure{Err: cliUsageErrorf("MCP command %s: unknown or excluded tool %q", binding.Name, request.Tool)}
	}
	schemaData, err := lifecycle.schemaJSON(selected.Name)
	if err != nil {
		return cliFailure{Err: fmt.Errorf("failed to preserve tool schema: %w", err)}
	}
	plan, err := compileCLIPlan(binding.Name, selected.Name, selected.Description, schemaData)
	if err != nil {
		return cliFailure{Err: err}
	}
	help, arguments, err := plan.prepare(request)
	if err != nil {
		return cliFailure{Err: err}
	}
	if help != nil {
		return help
	}
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: plan.tool, Arguments: arguments})
	if err != nil {
		return cliFailure{Err: fmt.Errorf("failed to call MCP %s %s (not retried): %w", binding.Name, selected.Name, lifecycle.connectionError(err))}
	}
	resultData, err := lifecycle.resultJSON()
	if err != nil {
		return cliFailure{Err: fmt.Errorf("failed to preserve MCP result: %w", err)}
	}
	resultData, err = transformResult(ctx, resultData)
	if err != nil {
		return cliFailure{Err: fmt.Errorf("adapt MCP result (operation not replayed): %w", err)}
	}
	// The SDK keeps resultType private; inspect the original wire envelope to
	// reject future result semantics rather than silently report success.
	var envelope struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(resultData, &envelope); err != nil {
		return cliResult{Envelope: resultData, Err: fmt.Errorf("failed to inspect MCP result: %w", err)}
	}
	switch envelope.ResultType {
	case "", "complete":
	case "input_required":
		return cliResult{Envelope: resultData, Err: errors.New("MCP input-required interaction is unsupported")}
	default:
		return cliResult{Envelope: resultData, Err: fmt.Errorf("MCP result type %q is unsupported", envelope.ResultType)}
	}
	if result.IsError {
		return cliResult{Envelope: resultData, Err: fmt.Errorf("MCP tool %s %s returned isError", binding.Name, selected.Name)}
	}
	return cliResult{Envelope: resultData}
}

func cliTimeout(params mcpServerParams) (time.Duration, error) {
	if params.SseReadTimeout != nil && (params.SseReadTimeout.CheckValid() != nil || params.SseReadTimeout.AsDuration() <= 0) {
		return 0, errors.New("MCP SSE read timeout must be positive and finite")
	}
	timeout := defaultTimeout
	if params.Timeout != nil {
		if params.Timeout.CheckValid() != nil || params.Timeout.AsDuration() <= 0 {
			return 0, errors.New("MCP timeout must be positive")
		}
		timeout = params.Timeout.AsDuration()
	}
	if params.ServerType == "sse" && params.SseReadTimeout != nil {
		// Native SSE keeps its GET alive for the larger configured timeout.
		// The command budget must not cut that stream off earlier.
		timeout = max(timeout, params.SseReadTimeout.AsDuration())
	}
	return timeout, nil
}

func cliConnection(binding cliBinding) (mcpServerParams, []string, bool) {
	if binding.SSE != nil {
		p := binding.SSE.Params
		return mcpServerParams{URL: p.Url, ServerType: "sse", Headers: p.Headers, Timeout: cliDuration(p.Timeout), SseReadTimeout: cliDuration(p.SseReadTimeout), TLSInsecureSkipVerify: p.TLSInsecureSkipVerify, TLSCACertPath: p.TLSCACertPath, TLSDisableSystemCAs: p.TLSDisableSystemCAs}, binding.SSE.Tools, binding.SSE.RequireApproval
	}
	p := binding.HTTP.Params
	return mcpServerParams{URL: p.Url, ServerType: "http", Headers: p.Headers, Timeout: cliDuration(p.Timeout), SseReadTimeout: cliDuration(p.SseReadTimeout), TLSInsecureSkipVerify: p.TLSInsecureSkipVerify, TLSCACertPath: p.TLSCACertPath, TLSDisableSystemCAs: p.TLSDisableSystemCAs}, binding.HTTP.Tools, binding.HTTP.RequireApproval
}

// Token decoding rejects duplicate keys at every depth and retains JSON numbers.
func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := jsonValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON input contains trailing data")
	}
	return value, nil
}

func jsonValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name := token.(string)
			if _, ok := result[name]; ok {
				return nil, fmt.Errorf("duplicate JSON key %q", name)
			}
			value, err := jsonValue(decoder)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return result, nil
	case '[':
		result := []any{}
		for decoder.More() {
			value, err := jsonValue(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// Remote schema references would add unconfigured network access to a command.
type cliSchemaLoader struct{}

var _ schemaValidator.URLLoader = cliSchemaLoader{}

func (cliSchemaLoader) Load(location string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is unsupported", location)
}

// cliHTTPTransport bounds command teardown without changing native MCP clients.
type cliHTTPTransport struct {
	base          http.RoundTripper
	endpoint      *url.URL
	legacySSE     bool
	mu            sync.Mutex
	resultBody    *cliResponseBody
	catalogBodies []*cliResponseBody
	streamBody    *cliResponseBody
	resultID      json.RawMessage
	catalogIDs    []json.RawMessage
	transportErr  error
}

var _ http.RoundTripper = (*cliHTTPTransport)(nil)

func (c *cliHTTPTransport) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resultBody = nil
	c.catalogBodies = nil
	c.resultID = nil
	c.catalogIDs = nil
	c.transportErr = nil
	if c.streamBody != nil {
		c.streamBody.mu.Lock()
		c.streamBody.data.Reset()
		c.streamBody.mu.Unlock()
	}
}

func (c *cliHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if c.legacySSE && (request.URL.Scheme != c.endpoint.Scheme || !strings.EqualFold(request.URL.Host, c.endpoint.Host) || request.URL.User != nil) {
		err := errors.New("MCP SSE message endpoint changed configured origin")
		c.recordError(err)
		return nil, err
	}
	var call struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if request.Method == http.MethodPost {
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			decodeErr := json.NewDecoder(body).Decode(&call)
			if err := errors.Join(decodeErr, body.Close()); err != nil {
				return nil, fmt.Errorf("failed to inspect MCP request: %w", err)
			}
		}
		// A credential header such as Idempotency-Key must not opt a mutating
		// MCP POST into net/http's automatic replay on a reused connection.
		request = request.Clone(request.Context())
		request.GetBody = nil
	}
	// Legacy SSE responses arrive on the GET stream and can precede the POST
	// acknowledgement. Register their request IDs before dispatch.
	if c.legacySSE {
		c.mu.Lock()
		switch call.Method {
		case "tools/call":
			c.resultID = bytes.Clone(call.ID)
		case "tools/list":
			c.catalogIDs = append(c.catalogIDs, bytes.Clone(call.ID))
		}
		c.mu.Unlock()
	}
	if request.Method == http.MethodDelete {
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		request = request.Clone(ctx)
	}
	response, err := c.base.RoundTrip(request)
	if err != nil {
		c.recordError(err)
		return nil, err
	}
	if c.legacySSE && request.Method == http.MethodGet {
		body := &cliResponseBody{ReadCloser: response.Body, contentType: response.Header.Get("Content-Type")}
		response.Body = body
		c.mu.Lock()
		c.streamBody = body
		c.mu.Unlock()
	} else if !c.legacySSE && (call.Method == "tools/call" || call.Method == "tools/list") {
		body := &cliResponseBody{ReadCloser: response.Body, requestID: call.ID, contentType: response.Header.Get("Content-Type")}
		response.Body = body
		c.mu.Lock()
		if call.Method == "tools/call" {
			c.resultBody = body
		} else {
			c.catalogBodies = append(c.catalogBodies, body)
		}
		c.mu.Unlock()
	}
	return response, nil
}

// The SDK may close SSE after a failed write and surface EOF instead of the
// write error. Retain its cause, including origin or invocation lookup failures.
func (c *cliHTTPTransport) connectionError(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(err, c.transportErr)
}

func (c *cliHTTPTransport) recordError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.transportErr == nil {
		c.transportErr = err
	}
}

// Capture HTTP response bytes before the SDK decodes generic result fields
// into float64. The SDK still owns protocol processing and content validation.
type cliResponseBody struct {
	io.ReadCloser
	requestID   json.RawMessage
	contentType string
	mu          sync.Mutex
	data        bytes.Buffer
}

var _ io.ReadCloser = (*cliResponseBody)(nil)

func (c *cliResponseBody) Read(destination []byte) (int, error) {
	n, err := c.ReadCloser.Read(destination)
	c.mu.Lock()
	c.data.Write(destination[:n])
	c.mu.Unlock()
	return n, err
}

func (c *cliHTTPTransport) resultJSON() (json.RawMessage, error) {
	c.mu.Lock()
	body := c.resultBody
	if c.legacySSE {
		body = c.streamBody
	}
	requestID := bytes.Clone(c.resultID)
	c.mu.Unlock()
	if body == nil {
		return nil, errors.New("MCP tool response was not captured")
	}
	if c.legacySSE {
		return body.resultForID(requestID)
	}
	return body.resultJSON()
}

func (c *cliResponseBody) resultJSON() (json.RawMessage, error) {
	return c.resultForID(c.requestID)
}

func (c *cliResponseBody) resultForID(requestID json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	data := bytes.Clone(c.data.Bytes())
	c.mu.Unlock()
	mediaType, _, err := mime.ParseMediaType(c.contentType)
	if err != nil {
		return nil, err
	}
	if mediaType == "application/json" {
		return resultEnvelope(data, requestID)
	}
	if mediaType != "text/event-stream" {
		return nil, fmt.Errorf("unsupported MCP response content type %q", mediaType)
	}
	// Extract only the matching JSON-RPC response from SSE data fields. Preserve
	// its raw result; notifications and other response IDs cannot become stdout.
	normalized := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	var event strings.Builder
	eventName := ""
	for line := range strings.SplitSeq(normalized, "\n") {
		if line == "" {
			if event.Len() > 0 && (eventName == "" || eventName == "message") {
				if result, err := resultEnvelope([]byte(event.String()), requestID); err == nil {
					return result, nil
				}
			}
			event.Reset()
			eventName = ""
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "event" {
			eventName = strings.TrimPrefix(value, " ")
		}
		if field == "data" {
			event.WriteString(strings.TrimPrefix(value, " "))
			event.WriteByte('\n')
		}
	}
	if event.Len() > 0 && (eventName == "" || eventName == "message") {
		if result, err := resultEnvelope([]byte(event.String()), requestID); err == nil {
			return result, nil
		}
	}
	return nil, errors.New("MCP stream did not contain the tool result envelope")
}

func resultEnvelope(data, requestID []byte) (json.RawMessage, error) {
	message, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		return nil, errors.New("MCP message is not a response")
	}
	var identifier any
	if err := json.Unmarshal(requestID, &identifier); err != nil {
		return nil, err
	}
	expected, err := jsonrpc.MakeID(identifier)
	if err != nil {
		return nil, err
	}
	if response.ID != expected || response.Error != nil || len(response.Result) == 0 {
		return nil, errors.New("JSON-RPC response does not match tool result")
	}
	return response.Result, nil
}

func (c *cliHTTPTransport) schemaJSON(name string) (json.RawMessage, error) {
	catalogs, err := c.catalogJSON()
	if err != nil {
		return nil, err
	}
	for _, data := range catalogs {
		var catalog struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(data, &catalog); err != nil {
			return nil, err
		}
		for _, tool := range catalog.Tools {
			if tool.Name == name && len(tool.InputSchema) > 0 {
				return tool.InputSchema, nil
			}
		}
	}
	return nil, fmt.Errorf("original schema for tool %q was not captured", name)
}

func (c *cliHTTPTransport) catalogJSON() ([]json.RawMessage, error) {
	c.mu.Lock()
	catalogs := slices.Clone(c.catalogBodies)
	stream := c.streamBody
	identifiers := slices.Clone(c.catalogIDs)
	c.mu.Unlock()
	var results []json.RawMessage
	if c.legacySSE {
		if stream == nil {
			return nil, errors.New("MCP SSE catalog stream was not captured")
		}
		for _, id := range identifiers {
			data, err := stream.resultForID(id)
			if err != nil {
				return nil, err
			}
			results = append(results, data)
		}
		return results, nil
	}
	for _, body := range catalogs {
		data, err := body.resultJSON()
		if err != nil {
			return nil, err
		}
		results = append(results, data)
	}
	return results, nil
}

func readCLIFile(ctx context.Context, path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("MCP CLI input %q must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := readCLIInput(ctx, file)
	return data, errors.Join(readErr, file.Close())
}

func readCLIInput(ctx context.Context, reader io.ReadCloser) ([]byte, error) {
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- reader.Close() })
	data, err := io.ReadAll(reader)
	if !stop() {
		err = errors.Join(err, ctx.Err(), <-closed)
	}
	return data, err
}

func cliDuration(value *float64) *durationpb.Duration {
	if value == nil {
		return nil
	}
	if *value <= 0 || math.IsNaN(*value) || math.IsInf(*value, 0) || *value >= float64(math.MaxInt64)/float64(time.Second) {
		return durationpb.New(-time.Second)
	}
	return durationpb.New(time.Duration(*value * float64(time.Second)))
}
