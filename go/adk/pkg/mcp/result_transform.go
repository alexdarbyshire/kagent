package mcp

import (
	"context"
	"encoding/json"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResultTransformer adapts a result using the current trusted invocation.
// Implementations must preserve non-image fields and return errors without replay.
type ResultTransformer func(context.Context, json.RawMessage) (json.RawMessage, error)

type resultTransformerKey struct{}

// WithResultTransformer configures a host-owned result adapter before construction.
func WithResultTransformer(ctx context.Context, transform ResultTransformer) context.Context {
	return context.WithValue(ctx, resultTransformerKey{}, transform)
}

func transformResult(ctx context.Context, result json.RawMessage) (json.RawMessage, error) {
	if transform, ok := ctx.Value(resultTransformerKey{}).(ResultTransformer); ok {
		return transform(currentInvocation(ctx), result)
	}
	return result, nil
}

func nativeResultClient(owner context.Context) *mcpsdk.Client {
	transform, _ := owner.Value(resultTransformerKey{}).(ResultTransformer)
	if transform == nil {
		return nil
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "kagent-adk", Version: "1"}, nil)
	client.AddSendingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			result, err := next(ctx, method, req)
			response, ok := result.(*mcpsdk.CallToolResult)
			if err != nil || method != "tools/call" || !ok || response == nil {
				return result, err
			}
			data, err := json.Marshal(response)
			if err != nil {
				return nil, err
			}
			data, err = transform(currentInvocation(ctx), data)
			if err != nil {
				return nil, err
			}
			var adapted mcpsdk.CallToolResult
			if err := json.Unmarshal(data, &adapted); err != nil {
				return nil, err
			}
			return &adapted, nil
		}
	})
	return client
}
