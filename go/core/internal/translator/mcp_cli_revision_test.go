package translator_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPCLIBindingEditsProduceImmutableRevisions(t *testing.T) {
	for _, change := range []string{"selection", "presentation", "removal", "rename", "endpoint", "description"} {
		t.Run(change, func(t *testing.T) {
			harness, template := cliConfiguration()
			server := remoteMCPServer("browser", "http://browser.example/mcp")
			compiler := cliCompiler(t, harness, modelConfig(), server, remoteMCPServer("renamed", "http://browser.example/mcp"))
			old, err := compiler.CompileAgent(t.Context(), inlineAgent(harness, template))
			require.NoError(t, err)
			oldID, err := old.Digest()
			require.NoError(t, err)
			frozen := append([]byte(nil), old.ConfigJSON...)
			switch change {
			case "selection":
				template.Spec.Tools[0].MCP.Tools = []string{"fetch"}
			case "presentation":
				template.Spec.Tools[0].MCP.ExposeAsCLI = new(false)
			case "removal":
				template.Spec.Tools = nil
			case "rename":
				template.Spec.Tools[0].MCP.Server.Name = "renamed"
			case "endpoint":
				server.Spec.URL = "http://browser.example/new-route"
				compiler = cliCompiler(t, harness, modelConfig(), server)
			case "description":
				server.Spec.Description = "Updated command guidance"
				compiler = cliCompiler(t, harness, modelConfig(), server)
			}
			next, err := compiler.CompileAgent(t.Context(), inlineAgent(harness, template))
			require.NoError(t, err)
			nextID, err := next.Digest()
			require.NoError(t, err)
			require.NotEqual(t, oldID, nextID)
			require.Equal(t, frozen, old.ConfigJSON, "compilation must not mutate previously prepared inputs")
			retainedID, err := old.Digest()
			require.NoError(t, err)
			require.Equal(t, oldID, retainedID)
			runtime := decodeCLIConfig(t, next.ConfigJSON)
			switch change {
			case "selection":
				require.Equal(t, []string{"fetch"}, runtime.CliTools[0].Http.Tools)
			case "presentation":
				require.Empty(t, runtime.CliTools)
				require.Len(t, runtime.HttpTools, 1)
			case "removal":
				require.Empty(t, runtime.CliTools)
				require.Empty(t, runtime.HttpTools)
			case "rename":
				require.Equal(t, "renamed", runtime.CliTools[0].Name)
			case "endpoint":
				require.Equal(t, server.Spec.URL, runtime.CliTools[0].Http.Params.Url)
			case "description":
				require.Equal(t, server.Spec.Description, runtime.CliTools[0].Description)
			}
		})
	}
}
