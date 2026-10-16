package nativeartifacts

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

type artifactContextKey struct{}

// NewClient intercepts images on the same connection used for ADK tool calls.
func NewClient() *mcp.Client {
	client := mcp.NewClient(&mcp.Implementation{Name: "kagent-artifacts", Version: "1"}, nil)
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			response, ok := result.(*mcp.CallToolResult)
			if err != nil || method != "tools/call" || !ok || response == nil {
				return result, err
			}
			ac, _ := ctx.Value(artifactContextKey{}).(agent.Context)
			data, err := json.Marshal(response)
			if err != nil {
				return nil, err
			}
			data, err = TransformResult(ac, data)
			if err != nil {
				return nil, err
			}
			var adapted mcp.CallToolResult
			if err := json.Unmarshal(data, &adapted); err != nil {
				return nil, err
			}
			return &adapted, nil
		}
	})
	return client
}

// Wrap binds artifact access to each invocation without changing MCP sessions.
func Wrap(ts tool.Toolset) tool.Toolset { return &artifactToolset{Toolset: ts} }

type artifactToolset struct{ tool.Toolset }

func (s *artifactToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := s.Toolset.Tools(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if ft, ok := t.(functionTool); ok {
			wrapped = append(wrapped, &artifactTool{functionTool: ft})
		} else {
			wrapped = append(wrapped, t)
		}
	}
	return wrapped, nil
}

type functionTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}
type artifactTool struct{ functionTool }

func (t *artifactTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, t)
}
func (t *artifactTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	return t.functionTool.Run(&boundContext{Context: ctx}, args)
}

// ADK's tool context deliberately does not implement WithAgentContext. Embed
// it instead: preserve artifact delta tracking, confirmation and cancellation.
type boundContext struct{ agent.Context }

func (c *boundContext) Value(key any) any {
	if _, ok := key.(artifactContextKey); ok {
		return c.Context
	}
	return c.Context.Value(key)
}
