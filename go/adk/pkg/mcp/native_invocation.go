package mcp

import (
	"context"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// invocationContextKey preserves the original context for current request
// authority when SDK timeout/cancellation wrappers erase its SessionID methods.
// It is carried only by active operations, never the retained connection.
type invocationContextKey struct{}

type nativeReadonlyContext struct{ adkagent.ReadonlyContext }

var _ adkagent.ReadonlyContext = nativeReadonlyContext{}

func (c nativeReadonlyContext) Value(key any) any {
	if _, ok := key.(invocationContextKey); ok {
		return c.ReadonlyContext
	}
	return c.ReadonlyContext.Value(key)
}

type nativeInvocationContext struct{ adkagent.Context }

var _ adkagent.Context = nativeInvocationContext{}

func (c nativeInvocationContext) Value(key any) any {
	if _, ok := key.(invocationContextKey); ok {
		return c.Context
	}
	return c.Context.Value(key)
}

type nativeInvocationToolset struct{ inner tool.Toolset }

var _ tool.Toolset = (*nativeInvocationToolset)(nil)

func (n *nativeInvocationToolset) Name() string { return n.inner.Name() }

func (n *nativeInvocationToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := n.inner.Tools(nativeReadonlyContext{ctx})
	if err != nil {
		return nil, err
	}
	for index, candidate := range tools {
		if runnable, ok := candidate.(nativeRunnableTool); ok {
			tools[index] = &nativeInvocationTool{nativeRunnableTool: runnable}
		}
	}
	return tools, nil
}

type nativeRunnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(adkagent.Context, any) (map[string]any, error)
}

type nativeInvocationTool struct{ nativeRunnableTool }

var _ tool.Tool = (*nativeInvocationTool)(nil)
var _ nativeRunnableTool = (*nativeInvocationTool)(nil)

func (n *nativeInvocationTool) Run(ctx adkagent.Context, arguments any) (map[string]any, error) {
	return n.nativeRunnableTool.Run(nativeInvocationContext{ctx}, arguments)
}

func (n *nativeInvocationTool) ProcessRequest(ctx adkagent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, n)
}

func currentInvocation(ctx context.Context) context.Context {
	if invocation, ok := ctx.Value(invocationContextKey{}).(context.Context); ok {
		return invocation
	}
	if invocation, ok := ctx.Value(cliCommandContextKey{}).(context.Context); ok {
		return invocation
	}
	return ctx
}
