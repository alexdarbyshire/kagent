package mcp

import (
	"context"
	"iter"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

type isolationScopeKey struct{}

type scopedAgent struct{ adkagent.Agent }

// WithCallerScope carries the host's actual isolation value into Google's
// readonly/tool contexts, whose IsolationScope method does not expose it.
// Only immutable scope data travels; no old invocation is retained.
func WithCallerScope(agent adkagent.Agent) adkagent.Agent {
	return &scopedAgent{Agent: agent}
}

func (s *scopedAgent) Run(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return s.Agent.Run(ctx.WithContext(context.WithValue(ctx, isolationScopeKey{}, ctx.IsolationScope())))
}
