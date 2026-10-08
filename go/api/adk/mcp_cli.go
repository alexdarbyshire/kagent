package adk

import (
	_ "embed"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// The catalog records PATH executables and BusyBox applets from the shipped
// Alpine 3.24 Go image, plus Bash builtins/keywords and the runtime entrypoints.
// Update it when Go image packaging changes; preparation performs no image I/O.
//
//go:embed mcp_cli_commands.txt
var platformCLICommands string

var cliCommandName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// ValidateTransport requires exactly one private connection representation.
// The existing HTTP value remains compatible with already prepared inputs.
func (c MCPCLIConfig) ValidateTransport() error {
	if c.SSE != nil {
		if !reflect.DeepEqual(c.HTTP, HttpMcpServerConfig{}) {
			return fmt.Errorf("MCP CLI binding %q contains ambiguous HTTP and SSE transports", c.Name)
		}
		if c.SSE.Params.Url == "" {
			return fmt.Errorf("MCP CLI binding %q requires an SSE endpoint", c.Name)
		}
		return nil
	}
	if c.HTTP.Params.Url == "" {
		return fmt.Errorf("MCP CLI binding %q requires one HTTP or SSE transport", c.Name)
	}
	return nil
}

// ValidateCLICommands rejects names that shadow shipped commands and duplicates
// within one agent. Different Shared agents have independent command scopes.
func ValidateCLICommands(bindings []MCPCLIConfig) error {
	reserved := strings.Fields(platformCLICommands)
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		if !cliCommandName.MatchString(binding.Name) {
			return fmt.Errorf("MCP CLI binding %q requires a safe command name", binding.Name)
		}
		for _, command := range reserved {
			if binding.Name == command {
				return fmt.Errorf("MCP CLI binding %q conflicts with reserved platform command %q; rename the RemoteMCPServer or retain native MCP presentation", binding.Name, command)
			}
		}
		if seen[binding.Name] {
			return fmt.Errorf("MCP CLI command collision for binding %q; bind each command once per agent", binding.Name)
		}
		seen[binding.Name] = true
	}
	return nil
}
