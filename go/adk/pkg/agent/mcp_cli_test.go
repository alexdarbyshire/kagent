package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	for _, failure := range []string{"agent_construction", "materialization"} {
		t.Run(failure, func(t *testing.T) {
			root := &adk.AgentConfig{
				Model:     &adk.OpenAI{BaseModel: adk.BaseModel{Type: adk.ModelTypeOpenAI, Model: "fixture"}},
				CLITools:  []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: "https://browser.example/mcp"}}}},
				SubAgents: []*adk.AgentConfig{{Name: "missing_model", CLITools: []adk.MCPCLIConfig{{Name: "browser", HTTP: adk.HttpMcpServerConfig{Params: adk.StreamableHTTPConnectionParams{Url: "https://child.example/mcp"}}}}}},
			}
			expectedError := "model configuration is required"
			if failure == "materialization" {
				root.SubAgents[0].Model = root.Model
				root.SubAgents[0].CLITools[0].HTTP.Params.Url = ""
				expectedError = "requires one HTTP or SSE transport"
			}
			_, err := agent.CreateGoogleADKAgent(t.Context(), root, "root", nil)
			require.ErrorContains(t, err, expectedError)
			entries, err := os.ReadDir(temporary)
			require.NoError(t, err)
			for _, entry := range entries {
				require.False(t, strings.HasPrefix(entry.Name(), "kagent-mcp-cli-"), "failed child retained private runtime directory %s", entry.Name())
			}
			// Retry the same root inputs after correcting the failed Shared config. CLI
			// startup never needs the previous partial tree or remote tool side effects.
			root.SubAgents[0].Model = root.Model
			root.SubAgents[0].CLITools[0].HTTP.Params.Url = "https://child.example/mcp"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			created, err := agent.CreateGoogleADKAgent(ctx, root, "root", nil)
			require.NoError(t, err)
			require.Len(t, created.SubAgents(), 1)
			entries, err = os.ReadDir(temporary)
			require.NoError(t, err)
			var tree string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "kagent-mcp-cli-") {
					require.Empty(t, tree, "retry must create exactly one private command tree")
					tree = filepath.Join(temporary, entry.Name())
				}
			}
			require.NotEmpty(t, tree)
			for _, scope := range []string{"agent-0", "agent-1"} {
				info, err := os.Stat(filepath.Join(tree, scope, "bin", "browser"))
				require.NoError(t, err)
				require.NotZero(t, info.Mode()&0o100, "retry must rematerialize executable launchers")
			}
			cancel()
			require.Eventually(t, func() bool { _, err := os.Stat(tree); return os.IsNotExist(err) }, time.Second, 10*time.Millisecond)
		})
	}
}
