package adk

import (
	"google.golang.org/protobuf/types/known/durationpb"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

// CLIBindings projects runtime configuration into the standalone bridge contract.
func CLIBindings(config *configv1.ADKAgentConfig) []MCPCLIConfig {
	var result []MCPCLIConfig
	for _, binding := range config.GetCliTools() {
		cli := MCPCLIConfig{Name: binding.Name, Description: binding.Description}
		if h := binding.Http; h != nil {
			p := h.GetParams()
			cli.HTTP = HttpMcpServerConfig{Tools: h.Tools, AllowedHeaders: h.AllowedHeaders, RequireApproval: h.RequireApproval,
				Params: StreamableHTTPConnectionParams{Url: p.GetUrl(), Headers: p.GetHeaders(), Timeout: seconds(p.GetTimeout()), SseReadTimeout: seconds(p.GetSseReadTimeout()), TerminateOnClose: p.TerminateOnClose, TLSInsecureSkipVerify: p.TlsInsecureSkipVerify, TLSCACertPath: p.TlsCaCertPath, TLSDisableSystemCAs: p.TlsDisableSystemCas}}
		}
		if h := binding.Sse; h != nil {
			p := h.GetParams()
			cli.SSE = &SseMcpServerConfig{Tools: h.Tools, AllowedHeaders: h.AllowedHeaders, RequireApproval: h.RequireApproval,
				Params: SseConnectionParams{Url: p.GetUrl(), Headers: p.GetHeaders(), Timeout: seconds(p.GetTimeout()), SseReadTimeout: seconds(p.GetSseReadTimeout()), TLSInsecureSkipVerify: p.TlsInsecureSkipVerify, TLSCACertPath: p.TlsCaCertPath, TLSDisableSystemCAs: p.TlsDisableSystemCas}}
		}
		result = append(result, cli)
	}
	return result
}

func seconds(value *durationpb.Duration) *float64 {
	if value == nil {
		return nil
	}
	result := value.AsDuration().Seconds()
	return &result
}
