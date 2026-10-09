package sts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/kagent-dev/kagent/go/adk/pkg/mcp"
	"github.com/kagent-dev/kagent/go/adk/pkg/models"
)

var errLifecycleAuthorityUnavailable = errors.New("provider-held lifecycle authority unavailable")

// lifecycleAuthority holds only a provider custody identity, never an invocation
// context or credential. Eviction and replacement invalidate that identity.
type lifecycleAuthority struct {
	provider  *TokenPropagationPlugin
	key       cacheKey
	custodyID uint64
}

var _ mcp.LifecycleAuthority = (*lifecycleAuthority)(nil)

// LifecycleAuthority references authority already held by this provider. It
// performs no exchange and does not renew cache eviction or credential expiry.
func (p *TokenPropagationPlugin) LifecycleAuthority(ctx context.Context) mcp.LifecycleAuthority {
	if ctx == nil {
		return nil
	}
	key := cacheKey{sessionID: sessionIDFromContext(ctx), subject: subjectKey(models.BearerTokenFromContext(ctx))}
	if key.sessionID == "" || key.subject == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry := p.tokenCache[key]
	if entry == nil || entry.HasExpired(p.bufferSeconds) || entry.evictable(p.bufferSeconds) {
		return nil
	}
	return &lifecycleAuthority{provider: p, key: key, custodyID: entry.custodyID}
}

func (a *lifecycleAuthority) Headers(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := a.provider
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry := p.tokenCache[a.key]
	if entry == nil || entry.custodyID != a.custodyID || entry.HasExpired(p.bufferSeconds) || entry.evictable(p.bufferSeconds) {
		return nil, errLifecycleAuthorityUnavailable
	}
	return map[string]string{"Authorization": "Bearer " + entry.Token}, nil
}

// Identity describes the provider-authenticated subject rather than its token.
func (a *lifecycleAuthority) Identity() string {
	return fmt.Sprintf("sts:%x", sha256.Sum256([]byte(a.key.sessionID+"\x00"+a.key.subject)))
}
