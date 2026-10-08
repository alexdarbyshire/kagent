package agent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/adk/pkg/agent"
	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/stretchr/testify/require"
)

func TestCreateGoogleADKAgentCLIFailureRemovesPartialTree(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("KAGENT_SKILLS_FOLDER", "")
	t.Setenv("KAGENT_PROPAGATE_TOKEN", "")
	t.Setenv("KAGENT_STS_WELL_KNOWN_URI", "")
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	root := &adk.AgentConfig{
		Model:     &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}},
		CLITools:  []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: "https://browser.example/mcp"}}}},
		SubAgents: []*adk.AgentConfig{{Name: "missing_model", CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: "https://child.example/mcp"}}}}}},
	}
	_, err = agent.CreateGoogleADKAgent(t.Context(), root, "root", nil)
	require.ErrorContains(t, err, "model configuration is required")
	entries, err := os.ReadDir(temporary)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), "kagent-mcp-cli-"), "failed child retained private runtime directory %s", entry.Name())
	}
}
