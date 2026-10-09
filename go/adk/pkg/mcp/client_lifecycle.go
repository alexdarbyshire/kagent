package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/pkg/logging"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// LifecycleAuthority resolves credentials already held by their provider without
// retaining an invocation or extending credential custody.
type LifecycleAuthority interface {
	Headers(context.Context) (map[string]string, error)
}

// LifecycleAuthorityProvider derives an opaque reference from a trusted caller.
type LifecycleAuthorityProvider func(context.Context) LifecycleAuthority

// TerminationOutcome separates observed remote termination from local release.
// Sessionless and SSE relationships have no HTTP DELETE termination operation.
type TerminationOutcome struct {
	Endpoint      string
	Status        string
	HTTPStatus    int
	LocalReleased bool
}

const shutdownTimeout = time.Second

// ClientLifecycle owns SDK connections and their local pools for a host lifetime.
// It holds provider references, never invocation contexts or credential snapshots.
type ClientLifecycle struct {
	provider      LifecycleAuthorityProvider
	mu            sync.Mutex
	connections   []*ownedConnection
	relationships map[relationshipKey]*clientRelationship
	closed        bool
	once          sync.Once
	outcomes      []TerminationOutcome
	running       context.Context
	stop          context.CancelFunc
}

type ownedConnection struct {
	connection   mcpsdk.Connection
	transport    *managedHTTPTransport
	pool         *http.Transport
	cancel       context.CancelFunc
	relationship *clientRelationship
	releaseOnce  sync.Once
}

// NewClientLifecycle attaches bounded shared teardown to the host's lifetime.
func NewClientLifecycle(host context.Context, provider LifecycleAuthorityProvider) *ClientLifecycle {
	running, stop := context.WithCancel(context.Background())
	owner := &ClientLifecycle{provider: provider, running: running, stop: stop}
	context.AfterFunc(host, func() {
		for _, outcome := range owner.Close() {
			logging.FromContext(host).InfoContext(host, "MCP termination outcome", "endpoint", outcome.Endpoint, "status", outcome.Status, "http_status", outcome.HTTPStatus)
		}
	})
	return owner
}

// Close releases all local connections concurrently within one total deadline.
// Returned evidence describes HTTP responses, not deletion of domain resources.
func (c *ClientLifecycle) Close() []TerminationOutcome {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		connections := c.connections
		c.connections = nil
		c.relationships = nil
		c.mu.Unlock()
		var wg sync.WaitGroup
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		for _, owned := range connections {
			owned.transport.mu.Lock()
			owned.transport.shutdown = shutdown
			owned.transport.mu.Unlock()
		}
		c.stop()
		for _, owned := range connections {
			wg.Go(func() {
				// SDK Close cancels streams after attempting DELETE. The HTTP
				// wrapper gives every attempt the same finite maximum budget.
				owned.release(shutdown)
			})
		}
		wg.Wait()
		for _, owned := range connections {
			outcome := owned.transport.outcome()
			if outcome.Status == "not_attempted" && owned.transport.legacySSE {
				outcome.Status = "unsupported_transport"
			}
			if outcome.Status == "not_attempted" && owned.connection.SessionID() == "" {
				outcome.Status = "no_session"
			}
			outcome.LocalReleased = true
			c.outcomes = append(c.outcomes, outcome)
		}
	})
	return append([]TerminationOutcome(nil), c.outcomes...)
}

func (c *ClientLifecycle) register(owned *ownedConnection) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.connections = append(c.connections, owned)
	if owned.relationship != nil {
		owned.relationship.connections = append(owned.relationship.connections, owned)
	}
	return true
}

type lifecycleHeadersKey struct{}

type managedHTTPTransport struct {
	base           http.RoundTripper
	resolver       *headerRoundTripper
	provider       LifecycleAuthorityProvider
	endpoint       string
	mu             sync.Mutex
	authority      LifecycleAuthority
	termination    TerminationOutcome
	shutdown       context.Context
	legacySSE      bool
	initialization context.Context
	commandHeaders *cliCommandHeaders
	lost           bool
}

var _ http.RoundTripper = (*managedHTTPTransport)(nil)

func (m *managedHTTPTransport) capture(ctx context.Context) {
	if m.provider == nil {
		return
	}
	reference := m.provider(currentInvocation(ctx))
	m.mu.Lock()
	m.authority = reference
	m.mu.Unlock()
}

func (m *managedHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodDelete {
		invocation := currentInvocation(request.Context())
		if m.commandHeaders != nil {
			m.commandHeaders.mu.Lock()
			if m.commandHeaders.invocation != nil {
				invocation = m.commandHeaders.invocation
			}
			m.commandHeaders.mu.Unlock()
		} else {
			m.mu.Lock()
			if m.initialization != nil {
				invocation = m.initialization
			}
			m.mu.Unlock()
		}
		if scoped, ok := invocation.(interface{ SessionID() string }); ok && scoped.SessionID() != "" {
			m.capture(invocation)
			if request.Method == http.MethodGet {
				headers := m.resolver.resolveHeaders(invocation)
				request = request.Clone(context.WithValue(request.Context(), lifecycleHeadersKey{}, true))
				maps.Copy(request.Header, headers)
			} else {
				request = request.Clone(context.WithValue(request.Context(), invocationContextKey{}, invocation))
			}
		}
		// SDK-owned streams and notifications have no retained invocation.
		// Their authority comes from existing provider custody as well.
		if _, scoped := invocation.(interface{ SessionID() string }); !scoped {
			headers, err := m.lifecycleHeaders(request.Context())
			if err != nil {
				return nil, err
			}
			request = request.Clone(context.WithValue(request.Context(), lifecycleHeadersKey{}, true))
			maps.Copy(request.Header, headers)
		}
		return m.dispatch(request)
	}
	// SDK Close may use a cancelled or detached connection context. Resolve
	// existing provider custody using a new bounded context with no invocation.
	m.mu.Lock()
	shutdown := m.shutdown
	m.mu.Unlock()
	if shutdown == nil {
		shutdown = context.Background()
	}
	ctx, cancel := context.WithTimeout(shutdown, shutdownTimeout)
	defer cancel()
	headers, err := m.lifecycleHeaders(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			m.recordFailure(err)
		} else {
			m.record("authority_unavailable", 0)
		}
		return nil, err
	}
	request = request.Clone(context.WithValue(ctx, lifecycleHeadersKey{}, true))
	maps.Copy(request.Header, headers)
	response, err := m.base.RoundTrip(request)
	if err != nil {
		m.recordFailure(err)
		return nil, err
	}
	status := "remote_refused"
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		status = "accepted"
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		status = "already_missing"
	case response.StatusCode == http.StatusMethodNotAllowed:
		status = "unsupported"
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		status = "auth_refused"
	}
	m.record(status, response.StatusCode)
	return response, nil
}

func (m *managedHTTPTransport) lifecycleHeaders(ctx context.Context) (http.Header, error) {
	m.mu.Lock()
	reference := m.authority
	m.mu.Unlock()
	headers := make(http.Header)
	var authorityErr error
	if reference != nil {
		var dynamic map[string]string
		dynamic, authorityErr = reference.Headers(ctx)
		for name, value := range dynamic {
			headers.Set(name, value)
		}
	}
	for name, value := range m.resolver.headers {
		headers.Set(name, value)
	}
	// Static Authorization is independently configured authority and wins over
	// dynamic credentials just as it does for current invocations.
	staticAuthority := http.Header{}
	for name, value := range m.resolver.headers {
		staticAuthority.Set(name, value)
	}
	needsAuthority := m.resolver.propagateToken || m.resolver.headerProvider != nil
	for _, name := range m.resolver.allowedHeaders {
		if http.CanonicalHeaderKey(name) == "Authorization" {
			needsAuthority = true
		}
	}
	if staticAuthority.Get("Authorization") == "" && (authorityErr != nil || (needsAuthority && reference == nil)) {
		return nil, errors.New("MCP lifecycle authority unavailable")
	}
	return headers, nil
}

func (m *managedHTTPTransport) record(status string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.termination = TerminationOutcome{Endpoint: m.endpoint, Status: status, HTTPStatus: code}
}

func (m *managedHTTPTransport) recordFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.termination.Status != "" {
		return
	}
	status := "transport_failed"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "timed_out"
	}
	m.termination = TerminationOutcome{Endpoint: m.endpoint, Status: status}
}

func (m *managedHTTPTransport) outcome() TerminationOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.termination.Status == "" {
		return TerminationOutcome{Endpoint: m.endpoint, Status: "not_attempted"}
	}
	return m.termination
}

type managedTransport struct {
	inner        mcpsdk.Transport
	owner        *ClientLifecycle
	resolver     *headerRoundTripper
	mu           sync.Mutex
	connected    bool
	relationship *clientRelationship
}

type registeredTransport struct {
	inner   mcpsdk.Transport
	owner   *ClientLifecycle
	managed *managedHTTPTransport
	pool    *http.Transport
	cancel  context.CancelFunc
}

var _ mcpsdk.Transport = (*registeredTransport)(nil)

func (r *registeredTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	connection, err := r.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if !r.owner.register(&ownedConnection{connection: connection, transport: r.managed, pool: r.pool, cancel: r.cancel}) {
		r.cancel()
		if err := connection.Close(); err != nil {
			r.managed.recordFailure(err)
		}
		r.pool.CloseIdleConnections()
		return nil, errors.New("MCP client lifecycle stopped")
	}
	return connection, nil
}

var _ mcpsdk.Transport = (*managedTransport)(nil)

func (m *managedTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	m.mu.Lock()
	replacing := m.connected
	m.mu.Unlock()
	if operation, _ := ctx.Value(operationKey{}).(*clientOperation); replacing && operation != nil {
		operation.mu.Lock()
		operation.lost = true
		operation.mu.Unlock()
		return nil, errors.New("MCP relationship state lost; establish fresh custody with a subsequent deliberate operation")
	}
	if operation, _ := ctx.Value(operationKey{}).(*clientOperation); operation != nil && operation.stateLost() {
		return nil, errors.New("MCP relationship state lost; operation not replayed")
	}
	pool := m.resolver.base.(*http.Transport).Clone()
	copyResolver := *m.resolver
	copyResolver.base = pool
	managed := &managedHTTPTransport{base: &copyResolver, resolver: &copyResolver, provider: m.owner.provider}
	managed.capture(ctx)
	var transport mcpsdk.Transport
	switch inner := m.inner.(type) {
	case *mcpsdk.StreamableClientTransport:
		copyTransport, copyClient := *inner, *inner.HTTPClient
		copyTransport.MaxRetries = -1
		copyClient.CheckRedirect = terminationRedirectPolicy(copyClient.CheckRedirect)
		managed.endpoint = inner.Endpoint
		if _, err := managed.lifecycleHeaders(context.Background()); err != nil {
			// Optional connection-owned GET cannot use context-only credentials.
			// Keep call-associated streams under current invocation authority.
			copyTransport.DisableStandaloneSSE = true
			logging.FromContext(ctx).WarnContext(ctx, "MCP standalone stream unavailable without lifecycle authority", "endpoint", inner.Endpoint)
		}
		copyClient.Transport = otelhttp.NewTransport(managed)
		copyTransport.HTTPClient = &copyClient
		transport = &copyTransport
	case *mcpsdk.SSEClientTransport:
		copyTransport, copyClient := *inner, *inner.HTTPClient
		managed.endpoint = inner.Endpoint
		managed.legacySSE = true
		managed.initialization = ctx
		copyClient.Transport = otelhttp.NewTransport(managed)
		copyTransport.HTTPClient = &copyClient
		transport = &copyTransport
	default:
		pool.CloseIdleConnections()
		return nil, errors.New("unsupported managed MCP transport")
	}
	// Only the provider reference crosses into connection-owned background work.
	connectionContext, cancel := context.WithCancel(m.owner.running)
	stopInitialization := context.AfterFunc(ctx, cancel)
	connection, err := transport.Connect(connectionContext)
	stopInitialization()
	managed.mu.Lock()
	managed.initialization = nil
	managed.mu.Unlock()
	if err != nil {
		cancel()
		pool.CloseIdleConnections()
		return nil, err
	}
	owned := &ownedConnection{connection: connection, transport: managed, pool: pool, cancel: cancel, relationship: m.relationship}
	if !m.owner.register(owned) {
		cancel()
		pool.CloseIdleConnections()
		if err := connection.Close(); err != nil {
			managed.recordFailure(err)
		}
		return nil, errors.New("MCP client lifecycle stopped")
	}
	m.mu.Lock()
	m.connected = true
	m.mu.Unlock()
	return connection, nil
}

func resolvedLifecycleHeaders(ctx context.Context) bool {
	resolved, _ := ctx.Value(lifecycleHeadersKey{}).(bool)
	return resolved
}

func createManagedTransport(ctx context.Context, params mcpServerParams) (mcpsdk.Transport, error) {
	if params.Lifecycle == nil || (params.ServerType != "http" && params.ServerType != "sse") {
		return createTransport(ctx, params)
	}
	if params.HTTPTransport == nil {
		params.HTTPTransport = &http.Transport{}
	}
	transport, err := createTransport(ctx, params)
	if err != nil {
		return nil, err
	}
	resolver := &headerRoundTripper{base: params.HTTPTransport, headers: params.Headers, allowedHeaders: params.AllowedHeaders, propagateToken: params.PropagateToken, headerProvider: params.HeaderProvider}
	return &managedTransport{inner: transport, owner: params.Lifecycle, resolver: resolver, relationship: params.relationship}, nil
}

func terminationRedirectPolicy(original func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, previous []*http.Request) error {
		// Termination authority belongs to the immutable configured binding.
		// A redirect's success cannot establish acceptance by that peer.
		if len(previous) > 0 && previous[0].Method == http.MethodDelete {
			return http.ErrUseLastResponse
		}
		if original != nil {
			return original(request, previous)
		}
		if len(previous) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

// The SDK remains the protocol owner. This boundary prevents a presentation's
// hidden refresher and net/http's reused-connection replay from repeating writes.
func (m *managedHTTPTransport) dispatch(request *http.Request) (*http.Response, error) {
	var call struct {
		Method string `json:"method"`
	}
	if request.Method == http.MethodPost && request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		err = errors.Join(json.NewDecoder(body).Decode(&call), body.Close())
		if err != nil {
			return nil, err
		}
		request = request.Clone(request.Context())
		if _, commandCapture := m.base.(*cliHTTPTransport); !commandCapture {
			request.GetBody = nil
		}
	}
	operation, _ := request.Context().Value(operationKey{}).(*clientOperation)
	if call.Method == "tools/call" && operation != nil {
		if err := operation.dispatch(); err != nil {
			return nil, err
		}
	}
	response, err := m.base.RoundTrip(request)
	lost := err != nil || (response != nil && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone))
	if lost {
		m.mu.Lock()
		m.lost = true
		m.mu.Unlock()
		if operation != nil {
			operation.mu.Lock()
			operation.lost = true
			operation.mu.Unlock()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("MCP relationship state lost; operation not replayed: %w", err)
	}
	if lost {
		_ = response.Body.Close()
		return nil, errors.New("MCP relationship state lost; previous remote state is unavailable")
	}
	return response, nil
}

func (m *managedHTTPTransport) stateLost() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lost
}

func (o *ownedConnection) release(shutdown context.Context) {
	o.releaseOnce.Do(func() {
		o.transport.mu.Lock()
		o.transport.shutdown = shutdown
		o.transport.mu.Unlock()
		if err := o.connection.Close(); err != nil {
			o.transport.recordFailure(err)
		}
		o.cancel()
		o.pool.CloseIdleConnections()
	})
}
