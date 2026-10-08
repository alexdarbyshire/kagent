package adkconfig

import (
	"net/url"
	"slices"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
)

func (c *Builder) addCLI(config *adk.AgentConfig, server *v1alpha3.RemoteMCPServer, binding v1alpha3.MCPToolBinding, headers map[string]string) error {
	if server.Spec.Protocol != "" && server.Spec.Protocol != v1alpha3.RemoteMCPServerProtocolStreamableHttp {
		return v2translator.NewValidationError("MCP CLI binding %q requires Streamable HTTP; SSE is pending CLI-3", server.Name)
	}
	if server.Spec.SseReadTimeout != nil {
		return v2translator.NewValidationError("MCP CLI binding %q does not support sseReadTimeout; pending CLI-3", server.Name)
	}
	if server.Spec.TerminateOnClose != nil && !*server.Spec.TerminateOnClose {
		return v2translator.NewValidationError("MCP CLI binding %q requires terminateOnClose: true", server.Name)
	}
	endpoint, err := url.Parse(server.Spec.URL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return v2translator.NewValidationError("MCP CLI binding %q requires an absolute HTTP(S) endpoint without user information or fragment", server.Name)
	}
	if server.Spec.Timeout != nil && server.Spec.Timeout.Duration <= 0 {
		return v2translator.NewValidationError("MCP CLI binding %q requires a positive timeout", server.Name)
	}
	// Reuse the native projection so HTTP settings retain one source of truth.
	native := &adk.AgentConfig{}
	if err := c.addRemoteMCPServer(native, server, binding.Tools, binding.RequireApproval, headers); err != nil {
		return err
	}
	config.CLITools = append(config.CLITools, adk.MCPCLIConfig{Name: server.Name, Description: server.Spec.Description, HTTP: native.HttpTools[0]})
	config.CLITools[len(config.CLITools)-1].HTTP.Tools = slices.Clone(binding.Tools)
	return nil
}

// addRemoteMCPServer translates the two remote protocols supported by the ADK.
// This path intentionally has no proxy URL or egress-gateway indirection.
func (c *Builder) addRemoteMCPServer(config *adk.AgentConfig, server *v1alpha3.RemoteMCPServer, tools []string, requireApproval bool, headers map[string]string) error {
	targetURL := server.Spec.URL

	switch server.Spec.Protocol {
	case v1alpha3.RemoteMCPServerProtocolSse:
		params := adk.SseConnectionParams{Url: targetURL, Headers: headers}
		if server.Spec.Timeout != nil {
			params.Timeout = new(server.Spec.Timeout.Seconds())
		}
		if server.Spec.SseReadTimeout != nil {
			params.SseReadTimeout = new(server.Spec.SseReadTimeout.Seconds())
		}
		params.TLSInsecureSkipVerify = tlsInsecureSkipVerify(server.Spec.TLS)
		config.SseTools = append(config.SseTools, adk.SseMcpServerConfig{
			Params: params, Tools: tools, RequireApproval: requireApproval,
		})
	default:
		params := adk.StreamableHTTPConnectionParams{Url: targetURL, Headers: headers}
		if server.Spec.Timeout != nil {
			params.Timeout = new(server.Spec.Timeout.Seconds())
		}
		if server.Spec.SseReadTimeout != nil {
			params.SseReadTimeout = new(server.Spec.SseReadTimeout.Seconds())
		}
		if server.Spec.TerminateOnClose != nil {
			params.TerminateOnClose = server.Spec.TerminateOnClose
		}
		params.TLSInsecureSkipVerify = tlsInsecureSkipVerify(server.Spec.TLS)
		config.HttpTools = append(config.HttpTools, adk.HttpMcpServerConfig{
			Params: params, Tools: tools, RequireApproval: requireApproval,
		})
	}

	return nil
}
