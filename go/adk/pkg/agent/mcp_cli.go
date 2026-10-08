package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// One runtime owns the complete tree so a failed child cannot leave orphaned
// command directories. Success transfers cleanup to the runtime context.
type cliScopes struct {
	directory string
	agents    map[*adk.AgentConfig]string
}

func materializeCLI(root *adk.AgentConfig) (_ *cliScopes, err error) {
	scopes := &cliScopes{agents: map[*adk.AgentConfig]string{}}
	var agents []*adk.AgentConfig
	var visit func(*adk.AgentConfig) error
	visit = func(config *adk.AgentConfig) error {
		if config == nil {
			return errors.New("agent config is required")
		}
		if err := adk.ValidateCLICommands(config.CLITools); err != nil {
			return err
		}
		if len(config.CLITools) > 0 {
			agents = append(agents, config)
		}
		for _, child := range config.SubAgents {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return scopes, nil
	}
	bridge, err := exec.LookPath("mcp-cli")
	if err != nil {
		return nil, fmt.Errorf("failed to find packaged MCP CLI bridge: %w", err)
	}
	bridge, err = filepath.Abs(bridge)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve MCP CLI bridge: %w", err)
	}
	scopes.directory, err = os.MkdirTemp("", "kagent-mcp-cli-")
	if err != nil {
		return nil, fmt.Errorf("failed to create private MCP CLI directory: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, scopes.close())
		}
	}()
	for i, config := range agents {
		private := filepath.Join(scopes.directory, fmt.Sprintf("agent-%d", i))
		bin := filepath.Join(private, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create MCP command directory: %w", err)
		}
		for _, binding := range config.CLITools {
			if binding.HTTP.RequireApproval {
				return nil, fmt.Errorf("MCP CLI binding %q does not support require_approval: true", binding.Name)
			}
			file := filepath.Join(private, binding.Name+".json")
			data, err := json.Marshal(binding)
			if err != nil {
				return nil, fmt.Errorf("failed to encode MCP CLI binding %q: %w", binding.Name, err)
			}
			if err := os.WriteFile(file, data, 0o600); err != nil {
				return nil, fmt.Errorf("failed to write MCP CLI binding %q: %w", binding.Name, err)
			}
			launcher := "#!/bin/sh\nexec " + shellQuote(bridge) + " --binding-file " + shellQuote(file) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, binding.Name), []byte(launcher), 0o700); err != nil {
				return nil, fmt.Errorf("failed to write MCP CLI command %q: %w", binding.Name, err)
			}
		}
		scopes.agents[config] = bin
	}
	return scopes, nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func (c *cliScopes) close() error {
	if c.directory == "" {
		return nil
	}
	return os.RemoveAll(c.directory)
}

func cliDiscovery(bindings []adk.MCPCLIConfig) string {
	if len(bindings) == 0 {
		return ""
	}
	var guidance strings.Builder
	guidance.WriteString("\nMCP commands available through bash:\n")
	for _, binding := range bindings {
		fmt.Fprintf(&guidance, "- %s: %s\n", binding.Name, binding.Description)
	}
	guidance.WriteString("Use COMMAND --help to discover selected tools, then COMMAND TOOL --help for arguments. Use --input-file PATH (or - for stdin) for JSON input. Results are JSON on stdout; failures have nonzero exit status and diagnostics on stderr.\n")
	return guidance.String()
}

func retainCLI(ctx context.Context, scopes *cliScopes) {
	context.AfterFunc(ctx, func() {
		if err := scopes.close(); err != nil {
			logging.FromContext(ctx).ErrorContext(ctx, "failed to remove MCP CLI command directories", "error", err)
		}
	})
}
