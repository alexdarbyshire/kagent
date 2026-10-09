package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/adk/v2/tool"
)

// AuthorityIdentity is an optional provider guarantee that credential rotation
// retains the same authority subject. Opaque credentials cannot make that claim.
type AuthorityIdentity interface {
	Identity() string
}

type callerScope interface {
	context.Context
	AppName() string
	UserID() string
	SessionID() string
	AgentName() string
	Branch() string
}

type relationshipKey struct {
	app, user, session, agent, branch, isolation string
	binding, authority, presentation             string
}

type clientRelationship struct {
	gate        chan struct{}
	native      tool.Toolset
	command     *cliRemoteSession
	headers     *cliCommandHeaders
	connections []*ownedConnection
}

func (c *ClientLifecycle) releaseRelationship(relationship *clientRelationship) {
	c.mu.Lock()
	connections := relationship.connections
	relationship.connections = nil
	c.mu.Unlock()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, owned := range connections {
		owned.release(shutdown)
	}
}

func bindingIdentity(binding any) string {
	data, _ := json.Marshal(binding)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (c *ClientLifecycle) scopeKey(ctx context.Context, binding, presentation string, resolver *headerRoundTripper) (relationshipKey, error) {
	ctx = currentInvocation(ctx)
	key := relationshipKey{binding: binding, presentation: presentation}
	if scope, ok := ctx.(callerScope); ok {
		key.app, key.user, key.session = scope.AppName(), scope.UserID(), scope.SessionID()
		key.agent, key.branch = scope.AgentName(), scope.Branch()
		if isolation, ok := ctx.Value(isolationScopeKey{}).(string); ok {
			key.isolation = isolation
		} else if isolation, ok := ctx.(interface{ IsolationScope() string }); ok {
			key.isolation = isolation.IsolationScope()
		}
		if key.app == "" || key.user == "" || key.session == "" || key.agent == "" {
			return key, errors.New("MCP client requires the original scoped agent context")
		}
	} else if presentation == "command" {
		return key, errors.New("MCP command requires the original scoped agent context")
	}
	// Static binding authority wins over a provider, matching header precedence.
	if c.provider != nil && resolver.resolveStaticAuthorization() == "" {
		if identity, ok := c.provider(ctx).(AuthorityIdentity); ok {
			key.authority = identity.Identity()
		}
	}
	if key.authority == "" {
		key.authority = bindingIdentity(resolver.resolveHeaders(ctx))
	}
	return key, nil
}

func (rt *headerRoundTripper) resolveStaticAuthorization() string {
	for name, value := range rt.headers {
		if strings.EqualFold(name, "Authorization") {
			return value
		}
	}
	return ""
}

func (c *ClientLifecycle) relationship(key relationshipKey) (*clientRelationship, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("MCP client lifecycle stopped")
	}
	if c.relationships == nil {
		c.relationships = make(map[relationshipKey]*clientRelationship)
	}
	owned := c.relationships[key]
	if owned == nil {
		owned = &clientRelationship{gate: make(chan struct{}, 1)}
		c.relationships[key] = owned
	}
	return owned, nil
}

func (c *ClientLifecycle) acquire(ctx context.Context, relationship *clientRelationship) (func(), error) {
	select {
	case relationship.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-relationship.gate
			return nil, err
		}
		if c.running.Err() != nil {
			<-relationship.gate
			return nil, errors.New("MCP client lifecycle stopped")
		}
		return func() { <-relationship.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.running.Done():
		return nil, errors.New("MCP client lifecycle stopped")
	}
}

// One operation survives SDK-internal retries but never a deliberate new call.
type operationKey struct{}
type clientOperation struct {
	mu         sync.Mutex
	dispatched bool
	lost       bool
}

func (o *clientOperation) dispatch() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dispatched {
		o.lost = true
		return errors.New("MCP relationship state lost; ambiguous operation was not replayed")
	}
	o.dispatched = true
	return nil
}

func (o *clientOperation) stateLost() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lost
}
