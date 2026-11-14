package adkconfig

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
)

func TestAddRemoteMCPServerPreservesBindingApproval(t *testing.T) {
	tests := []struct {
		name     string
		protocol v1alpha3.RemoteMCPServerProtocol
		assert   func(*testing.T, *configv1.ADKAgentConfig)
	}{
		{
			name:     "streamable HTTP",
			protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp,
			assert: func(t *testing.T, config *configv1.ADKAgentConfig) {
				require.Len(t, config.HttpTools, 1)
				require.True(t, config.HttpTools[0].RequireApproval)
				require.Nil(t, config.HttpTools[0].Tools)
			},
		},
		{
			name:     "SSE with exposure filter",
			protocol: v1alpha3.RemoteMCPServerProtocolSse,
			assert: func(t *testing.T, config *configv1.ADKAgentConfig) {
				require.Len(t, config.SseTools, 1)
				require.True(t, config.SseTools[0].RequireApproval)
				require.Equal(t, []string{"write"}, config.SseTools[0].Tools)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &configv1.ADKAgentConfig{}
			server := &v1alpha3.RemoteMCPServer{Spec: v1alpha3.RemoteMCPServerSpec{
				URL: "https://mcp.example.test", Protocol: test.protocol,
			}}
			var tools []string
			if test.protocol == v1alpha3.RemoteMCPServerProtocolSse {
				tools = []string{"write"}
			}

			err := (&Builder{}).addRemoteMCPServer(config, server, tools, true, nil)
			require.NoError(t, err)
			test.assert(t, config)
		})
	}
}

func TestAddRemoteMCPServerTokenPassthrough(t *testing.T) {
	config := &configv1.ADKAgentConfig{}
	for _, protocol := range []v1alpha3.RemoteMCPServerProtocol{v1alpha3.RemoteMCPServerProtocolStreamableHttp, v1alpha3.RemoteMCPServerProtocolSse} {
		server := &v1alpha3.RemoteMCPServer{Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://mcp.example.test", Protocol: protocol, TokenPassthrough: new(true),
		}}
		require.NoError(t, (&Builder{}).addRemoteMCPServer(config, server, nil, false, nil))
	}
	require.True(t, config.HttpTools[0].TokenPassthrough)
	require.True(t, config.SseTools[0].TokenPassthrough)
}

func TestAddCLIPreservesGatewayIdentityAndTokenPassthrough(t *testing.T) {
	for _, protocol := range []v1alpha3.RemoteMCPServerProtocol{v1alpha3.RemoteMCPServerProtocolStreamableHttp, v1alpha3.RemoteMCPServerProtocolSse} {
		for _, callerToken := range []bool{false, true} {
			t.Run(string(protocol)+"/"+strconv.FormatBool(callerToken), func(t *testing.T) {
				config := &configv1.ADKAgentConfig{}
				server := &v1alpha3.RemoteMCPServer{Spec: v1alpha3.RemoteMCPServerSpec{
					URL: "https://mcp.example.test", Protocol: protocol,
				}}
				if callerToken {
					server.Spec.TokenPassthrough = new(true)
				} else {
					server.Spec.ActorJWT = &v1alpha3.ActorJWT{Audiences: []string{"fixture"}}
				}
				server.Name = "fixture"
				headers := map[string]string{"X-Fixture": "retained"}
				require.NoError(t, (&Builder{}).addCLI(config, server, v1alpha3.MCPToolBinding{Tools: []string{"read"}}, headers))
				require.Len(t, config.CliTools, 1)
				var compiledHeaders map[string]string
				if protocol == v1alpha3.RemoteMCPServerProtocolSse {
					require.Equal(t, callerToken, config.CliTools[0].Sse.TokenPassthrough)
					require.Equal(t, []string{"read"}, config.CliTools[0].Sse.Tools)
					compiledHeaders = config.CliTools[0].Sse.Params.Headers
				} else {
					require.Equal(t, callerToken, config.CliTools[0].Http.TokenPassthrough)
					require.Equal(t, []string{"read"}, config.CliTools[0].Http.Tools)
					compiledHeaders = config.CliTools[0].Http.Params.Headers
				}
				require.Equal(t, "retained", compiledHeaders["X-Fixture"])
				if callerToken {
					require.NotContains(t, compiledHeaders, "Authorization")
				} else {
					require.Equal(t, "Bearer "+translator.CredentialPlaceholder, compiledHeaders["Authorization"])
				}
				require.Equal(t, map[string]string{"X-Fixture": "retained"}, headers)
				require.Empty(t, config.HttpTools)
				require.Empty(t, config.SseTools)
			})
		}
	}
}
