package adk

type StreamableHTTPConnectionParams struct {
	Url              string            `json:"url"`
	Headers          map[string]string `json:"headers"`
	Timeout          *float64          `json:"timeout,omitempty"`
	SseReadTimeout   *float64          `json:"sse_read_timeout,omitempty"`
	TerminateOnClose *bool             `json:"terminate_on_close,omitempty"`
	// TLS configuration for self-signed certificates
	TLSInsecureSkipVerify *bool   `json:"tls_insecure_skip_verify,omitempty"`
	TLSCACertPath         *string `json:"tls_ca_cert_path,omitempty"`
	TLSDisableSystemCAs   *bool   `json:"tls_disable_system_cas,omitempty"`
}

type HttpMcpServerConfig struct {
	Params          StreamableHTTPConnectionParams `json:"params"`
	Tools           []string                       `json:"tools,omitempty"`
	AllowedHeaders  []string                       `json:"allowed_headers,omitempty"`
	RequireApproval bool                           `json:"require_approval,omitempty"`
}

// MCPCLIConfig is a private, named command binding consumed by the Go runtime
// and bridge. It preserves the bridge's HTTP input contract.
type MCPCLIConfig struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	HTTP        HttpMcpServerConfig `json:"http"`
	SSE         *SseMcpServerConfig `json:"sse,omitempty"`
	// RequiresInvocationHeaders is set only in the runtime's private binding
	// file when invocation-scoped authentication must be obtained from its parent.
	RequiresInvocationHeaders bool `json:"requires_invocation_headers,omitempty"`
}

type SseConnectionParams struct {
	Url            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	Timeout        *float64          `json:"timeout,omitempty"`
	SseReadTimeout *float64          `json:"sse_read_timeout,omitempty"`
	// TLS configuration for self-signed certificates
	TLSInsecureSkipVerify *bool   `json:"tls_insecure_skip_verify,omitempty"`
	TLSCACertPath         *string `json:"tls_ca_cert_path,omitempty"`
	TLSDisableSystemCAs   *bool   `json:"tls_disable_system_cas,omitempty"`
}

type SseMcpServerConfig struct {
	Params          SseConnectionParams `json:"params"`
	Tools           []string            `json:"tools,omitempty"`
	AllowedHeaders  []string            `json:"allowed_headers,omitempty"`
	RequireApproval bool                `json:"require_approval,omitempty"`
}
