package mcp

import (
	"errors"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

// Google owns conversion and confirmation; the host selects its scoped client.
type scopedNativeToolset struct {
	params    mcpServerParams
	predicate tool.Predicate
	binding   string
}

func (n *scopedNativeToolset) Name() string { return "mcp_tool_set" }

func (n *scopedNativeToolset) selectRelationship(ctx adkagent.ReadonlyContext) (*clientRelationship, func(), error) {
	resolver := &headerRoundTripper{headers: n.params.Headers, allowedHeaders: n.params.AllowedHeaders, propagateToken: n.params.PropagateToken, headerProvider: n.params.HeaderProvider}
	key, err := n.params.Lifecycle.scopeKey(ctx, n.binding, "native", resolver)
	if err != nil {
		return nil, nil, err
	}
	owned, err := n.params.Lifecycle.relationship(key)
	if err != nil {
		return nil, nil, err
	}
	release, err := n.params.Lifecycle.acquire(ctx, owned)
	if err != nil {
		return nil, nil, err
	}
	if owned.native == nil {
		params := n.params
		params.relationship = owned
		transport, err := createManagedTransport(ctx, params)
		if err == nil {
			owned.native, err = mcptoolset.New(mcptoolset.Config{Transport: transport, Client: nativeResultClient(n.params.Lifecycle.running)})
		}
		if err != nil {
			release()
			return nil, nil, err
		}
		if n.predicate != nil {
			owned.native = tool.FilterToolset(owned.native, n.predicate)
		}
	}
	return owned, release, nil
}

func (n *scopedNativeToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	owned, release, err := n.selectRelationship(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	tools, err := owned.native.Tools(nativeReadonlyContext{ReadonlyContext: ctx, operation: &clientOperation{}})
	if err != nil {
		n.params.Lifecycle.releaseRelationship(owned)
		owned.native = nil
		return nil, err
	}
	for i, candidate := range tools {
		if runnable, ok := candidate.(nativeRunnableTool); ok {
			tools[i] = &nativeInvocationTool{nativeRunnableTool: runnable, scoped: n}
		}
	}
	return tools, nil
}

func (n *scopedNativeToolset) run(ctx adkagent.Context, name string, args any) (map[string]any, error) {
	owned, release, err := n.selectRelationship(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	operation := &clientOperation{}
	invocation := nativeInvocationContext{Context: ctx, operation: operation}
	tools, err := owned.native.Tools(nativeReadonlyContext{ReadonlyContext: invocation, operation: operation})
	if err != nil {
		n.params.Lifecycle.releaseRelationship(owned)
		owned.native = nil
		return nil, fmt.Errorf("MCP relationship state lost: %w", err)
	}
	for _, candidate := range tools {
		if candidate.Name() != name {
			continue
		}
		runnable, ok := candidate.(nativeRunnableTool)
		if !ok {
			return nil, errors.New("MCP native tool is not runnable")
		}
		result, err := runnable.Run(invocation, args)
		if err != nil && operation.stateLost() {
			// A deliberate subsequent call selects a fresh relationship. Never
			// hide the failed action behind Google's automatic reconnection.
			n.params.Lifecycle.releaseRelationship(owned)
			owned.native = nil
			return nil, fmt.Errorf("MCP relationship state lost; operation not replayed: %w", err)
		}
		return result, err
	}
	return nil, fmt.Errorf("unknown or excluded MCP tool %q", name)
}
