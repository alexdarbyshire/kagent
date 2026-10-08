package translator

import (
	"regexp"
	"slices"
	"strings"

	"github.com/kagent-dev/kagent/go/api/adk"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
)

var cliImageReference = regexp.MustCompile(`^[^[:space:]@]+@sha256:[a-f0-9]{64}$`)

// ValidateCLIExposure verifies runtime support before a harness can compile CLI bindings.
// An empty registered image list rejects CLI presentation while retaining native MCP.
func ValidateCLIExposure(input *HarnessInput, registeredGoImages []string) error {
	if input == nil || input.Harness == nil || input.Root == nil {
		return NewValidationError("MCP CLI validation requires resolved Harness and agent inputs")
	}
	var validate func(*AgentInput) error
	validate = func(agent *AgentInput) error {
		if agent == nil {
			return nil
		}
		var commands []adk.MCPCLIConfig
		for _, binding := range agent.MCPTools {
			if binding.Binding.ExposeAsCLI == nil || !*binding.Binding.ExposeAsCLI {
				continue
			}
			if harnessType(input.Harness) != HarnessTypeKagent {
				return NewValidationError("MCP CLI binding %q requires the CLI-enabled kagent Go runtime; %s is unsupported", binding.Server.Name, harnessType(input.Harness))
			}
			image := input.Harness.Spec.Workload.Image
			for _, registered := range registeredGoImages {
				if !cliImageReference.MatchString(registered) {
					return NewValidationError("invalid KAGENT_MCP_CLI_GO_IMAGES entry %q: expected an exact sha256 digest-pinned image reference", registered)
				}
			}
			if !cliImageReference.MatchString(image) || !slices.Contains(registeredGoImages, image) {
				return NewValidationError("MCP CLI binding %q requires an exact digest-pinned Go image registered in KAGENT_MCP_CLI_GO_IMAGES", binding.Server.Name)
			}
			command := input.Harness.Spec.Workload.Command
			if len(command) > 0 && !slices.Equal(command, []string{"/app"}) {
				return NewValidationError("MCP CLI binding %q requires the image entrypoint or command [/app]", binding.Server.Name)
			}
			if binding.Binding.RequireApproval {
				return NewValidationError("MCP CLI binding %q does not support requireApproval: true; retain native MCP presentation", binding.Server.Name)
			}
			for _, variable := range input.Harness.Spec.Env {
				if (variable.Name == env.KagentPropagateToken.Name() && strings.EqualFold(strings.TrimSpace(variable.Value), "true")) ||
					(variable.Name == env.StsWellKnownURI.Name() && strings.TrimSpace(variable.Value) != "") {
					return NewValidationError("MCP CLI binding %q does not support %s; invocation-scoped authentication is pending CLI-3", binding.Server.Name, variable.Name)
				}
			}
			commands = append(commands, adk.MCPCLIConfig{Name: binding.Server.Name})
		}
		if err := adk.ValidateCLICommands(commands); err != nil {
			return NewValidationError("%v", err)
		}
		for _, child := range agent.Shared {
			if err := validate(child.Agent); err != nil {
				return err
			}
		}
		return nil
	}
	return validate(input.Root)
}
