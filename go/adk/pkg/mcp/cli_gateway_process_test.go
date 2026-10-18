package mcp

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// This opt-in characterization uses local binaries and a synthetic HTTP origin.
// It never needs an estate deployment or provider credentials.
func TestCLIPinnedGatewayProjection(t *testing.T) {
	gateway, cli := os.Getenv("KAGENT_TEST_AGENTGATEWAY"), os.Getenv("KAGENT_TEST_MCP_CLI")
	if gateway == "" || cli == "" {
		t.Skip("set KAGENT_TEST_AGENTGATEWAY to agentgateway 1.6.0 and KAGENT_TEST_MCP_CLI to the candidate executable; requires python3")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err, "explicit gateway characterization requires python3")
	output, err := exec.CommandContext(t.Context(), python, "testdata/cli-results/pinned_gateway.py", "--gateway", gateway, "--cli", cli).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Logf("%s", output)
}
