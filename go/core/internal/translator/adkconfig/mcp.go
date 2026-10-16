package adkconfig

import (
	"net/url"
	"slices"

	"google.golang.org/protobuf/types/known/durationpb"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
)

func (c *Builder) addCLI(config *configv1.ADKAgentConfig, server *v1alpha3.RemoteMCPServer, binding v1alpha3.MCPToolBinding, headers map[string]string) error {
	isSSE := server.Spec.Protocol == v1alpha3.RemoteMCPServerProtocolSse
	if server.Spec.Protocol != "" && server.Spec.Protocol != v1alpha3.RemoteMCPServerProtocolStreamableHttp && !isSSE {
		return v2translator.NewValidationError("MCP CLI binding %q requires Streamable HTTP or SSE", server.Name)
	}
	if !isSSE && server.Spec.SseReadTimeout != nil {
		return v2translator.NewValidationError("MCP CLI Streamable HTTP binding %q does not support sseReadTimeout", server.Name)
	}
	if !isSSE && server.Spec.TerminateOnClose != nil && !*server.Spec.TerminateOnClose {
		return v2translator.NewValidationError("MCP CLI binding %q requires terminateOnClose: true", server.Name)
	}
	endpoint, err := url.Parse(server.Spec.URL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return v2translator.NewValidationError("MCP CLI binding %q requires an absolute HTTP(S) endpoint without user information or fragment", server.Name)
	}
	if server.Spec.Timeout != nil && server.Spec.Timeout.Duration <= 0 {
		return v2translator.NewValidationError("MCP CLI binding %q requires a positive timeout", server.Name)
	}
	if isSSE && server.Spec.SseReadTimeout != nil && server.Spec.SseReadTimeout.Duration <= 0 {
		return v2translator.NewValidationError("MCP CLI binding %q requires a positive sseReadTimeout", server.Name)
	}
	// Reuse the native projection so HTTP settings retain one source of truth.
	native := &configv1.ADKAgentConfig{}
	if err := c.addRemoteMCPServer(native, server, binding.Tools, binding.RequireApproval, headers); err != nil {
		return err
	}
	cli := &configv1.ADKMCPCLIConfig{Name: server.Name, Description: server.Spec.Description}
	if isSSE {
		cli.Sse = native.SseTools[0]
		cli.Sse.Tools = slices.Clone(binding.Tools)
	} else {
		cli.Http = native.HttpTools[0]
		cli.Http.Tools = slices.Clone(binding.Tools)
	}
	config.CliTools = append(config.CliTools, cli)
	return nil
}

// addRemoteMCPServer translates the two remote protocols supported by the ADK.
// This path intentionally has no proxy URL or egress-gateway indirection.
func (c *Builder) addRemoteMCPServer(config *configv1.ADKAgentConfig, server *v1alpha3.RemoteMCPServer, tools []string, requireApproval bool, headers map[string]string) error {
	targetURL := server.Spec.URL

	switch server.Spec.Protocol {
	case v1alpha3.RemoteMCPServerProtocolSse:
		params := &configv1.ADKSSEConnectionParams{Url: targetURL, Headers: headers}
		if server.Spec.Timeout != nil {
			params.Timeout = durationpb.New(server.Spec.Timeout.Duration)
		}
		if server.Spec.SseReadTimeout != nil {
			params.SseReadTimeout = durationpb.New(server.Spec.SseReadTimeout.Duration)
		}
		params.TlsInsecureSkipVerify = tlsInsecureSkipVerify(server.Spec.TLS)
		config.SseTools = append(config.SseTools, &configv1.ADKSSEMCPServerConfig{
			Params: params, Tools: tools, RequireApproval: requireApproval,
		})
	default:
		params := &configv1.ADKHTTPConnectionParams{Url: targetURL, Headers: headers}
		if server.Spec.Timeout != nil {
			params.Timeout = durationpb.New(server.Spec.Timeout.Duration)
		}
		if server.Spec.SseReadTimeout != nil {
			params.SseReadTimeout = durationpb.New(server.Spec.SseReadTimeout.Duration)
		}
		if server.Spec.TerminateOnClose != nil {
			params.TerminateOnClose = server.Spec.TerminateOnClose
		}
		params.TlsInsecureSkipVerify = tlsInsecureSkipVerify(server.Spec.TLS)
		config.HttpTools = append(config.HttpTools, &configv1.ADKHTTPMCPServerConfig{
			Params: params, Tools: tools, RequireApproval: requireApproval,
		})
	}

	return nil
}
