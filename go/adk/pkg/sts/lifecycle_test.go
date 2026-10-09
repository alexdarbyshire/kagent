package sts

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/adk/pkg/models"
)

func TestLifecycleAuthorityProviderCustody(t *testing.T) {
	for _, exchange := range []bool{false, true} {
		t.Run(map[bool]string{false: "propagate", true: "exchange"}[exchange], func(t *testing.T) {
			var exchanges atomic.Int32
			var integration *STSIntegration
			if exchange {
				integration = newSTSIntegration(t, nil, func(_ *http.Request) map[string]any {
					exchanges.Add(1)
					return map[string]any{"access_token": "delegated", "token_type": "Bearer", "expires_in": 3600}
				})
			}
			p := NewTokenPropagationPlugin(integration, slog.New(slog.DiscardHandler), nil, nil)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), models.BearerTokenKey, "alice"))
			invocation := &fakeInvocationContext{Context: ctx, sessionID: "session"}
			request := fakeSessionContext{Context: ctx, sessionID: "session"}
			if p.LifecycleAuthority(request) != nil {
				t.Fatal("context-only authority acquired provider custody")
			}
			if _, err := p.BeforeRunCallback(invocation); err != nil {
				t.Fatal(err)
			}
			authority := p.LifecycleAuthority(request)
			if authority == nil {
				t.Fatal("provider-held authority unavailable")
			}
			cancel()
			p.AfterRunCallback(invocation)
			headers, err := authority.Headers(context.Background())
			want := "Bearer alice"
			if exchange {
				want = "Bearer delegated"
			}
			if err != nil || headers["Authorization"] != want {
				t.Fatalf("headers=%v err=%v", headers, err)
			}
			other := fakeSessionContext{Context: context.WithValue(context.Background(), models.BearerTokenKey, "bob"), sessionID: "session"}
			if p.LifecycleAuthority(other) != nil {
				t.Fatal("another subject obtained custody")
			}
			p.ClearCache()
			if headers, err := authority.Headers(context.Background()); err == nil || len(headers) != 0 {
				t.Fatal("evicted authority remained usable")
			}
			p.setCachedToken("session", subjectKey("alice"), "replacement", 0)
			if _, err := authority.Headers(context.Background()); err == nil {
				t.Fatal("stale reference revived after eviction")
			}
			wantExchanges := int32(0)
			if exchange {
				wantExchanges = 1
			}
			if exchanges.Load() != wantExchanges {
				t.Fatal("lifecycle resolution initiated a token exchange")
			}
		})
	}
}

func TestLifecycleAuthorityDoesNotRenewOrIgnoreExpiry(t *testing.T) {
	for _, deadline := range []string{"expiry", "idle"} {
		t.Run(deadline, func(t *testing.T) {
			p := NewTokenPropagationPlugin(nil, slog.New(slog.DiscardHandler), nil, nil)
			p.setCachedToken("session", subjectKey("alice"), "held", 0)
			ctx := fakeSessionContext{Context: context.WithValue(context.Background(), models.BearerTokenKey, "alice"), sessionID: "session"}
			authority := p.LifecycleAuthority(ctx)
			p.mu.Lock()
			entry := p.tokenCache[cacheKey{"session", subjectKey("alice")}]
			seq := entry.useSeq
			if deadline == "expiry" {
				entry.Expiry = time.Now().Unix() - 1
			} else {
				entry.evictAfter = time.Now().Unix() - 1
			}
			p.mu.Unlock()
			if _, err := authority.Headers(context.Background()); err == nil {
				t.Fatal("ended authority resolved")
			}
			if p.LifecycleAuthority(ctx) != nil {
				t.Fatal("ended authority captured")
			}
			p.mu.RLock()
			defer p.mu.RUnlock()
			if entry.useSeq != seq {
				t.Fatal("lifecycle resolution renewed cache use")
			}
		})
	}
}

func TestLifecycleAuthorityRetainsCallerExpiry(t *testing.T) {
	for _, exchange := range []bool{false, true} {
		t.Run(map[bool]string{false: "propagate", true: "exchange"}[exchange], func(t *testing.T) {
			var integration *STSIntegration
			if exchange {
				integration = newSTSIntegration(t, nil, func(*http.Request) map[string]any {
					return map[string]any{"access_token": "long-lived", "token_type": "Bearer", "expires_in": 3600}
				})
			}
			p := NewTokenPropagationPlugin(integration, slog.New(slog.DiscardHandler), nil, nil)
			bearer := signedTokenExpiringIn(t, "alice", 30*time.Second)
			ctx := context.WithValue(context.Background(), models.BearerTokenKey, bearer)
			if _, err := p.BeforeRunCallback(&fakeInvocationContext{Context: ctx, sessionID: "session"}); err != nil {
				t.Fatal(err)
			}
			authority := p.LifecycleAuthority(fakeSessionContext{Context: ctx, sessionID: "session"})
			if authority == nil {
				t.Fatal("valid caller authority unavailable")
			}
			p.mu.Lock()
			entry := p.tokenCache[cacheKey{"session", subjectKey(bearer)}]
			if entry.Expiry != extractJWTExpiry(bearer) {
				p.mu.Unlock()
				t.Fatal("provider custody outlived caller credential")
			}
			entry.Expiry = time.Now().Unix() - 1
			p.mu.Unlock()
			if _, err := authority.Headers(context.Background()); err == nil {
				t.Fatal("expired caller authorized lifecycle request")
			}
		})
	}
}
