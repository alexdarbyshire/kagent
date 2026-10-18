package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Outcomes contain semantic help or a captured result, never rendered output.
type cliOutcome interface{ cliOutcome() }

type cliCatalogHelp struct {
	Command string               `json:"command"`
	Tools   []cliToolDescription `json:"tools"`
}

type cliToolDescription struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type cliOperationHelp struct {
	Command     string                    `json:"command"`
	Tool        string                    `json:"tool"`
	Description string                    `json:"description"`
	Parameters  []cliParameterDescription `json:"parameters"`
}

type cliParameterDescription struct {
	Name         string   `json:"name"`
	Mode         string   `json:"mode"`
	Types        []string `json:"types"`
	Requirement  string   `json:"requirement"`
	Descriptions []string `json:"descriptions"`
	Defaults     []string `json:"defaults"`
}

type cliSchemaHelp struct{ Schema json.RawMessage }
type cliResult struct {
	Envelope json.RawMessage
	Err      error
}
type cliFailure struct{ Err error }

func (cliCatalogHelp) cliOutcome()   {}
func (cliOperationHelp) cliOutcome() {}
func (cliSchemaHelp) cliOutcome()    {}
func (cliResult) cliOutcome()        {}
func (cliFailure) cliOutcome()       {}

var (
	_ cliOutcome = cliCatalogHelp{}
	_ cliOutcome = cliOperationHelp{}
	_ cliOutcome = cliSchemaHelp{}
	_ cliOutcome = cliResult{}
	_ cliOutcome = cliFailure{}
)

func renderCLIOutcome(stdout io.Writer, outcome cliOutcome) error {
	switch value := outcome.(type) {
	case cliFailure:
		return value.Err
	case cliResult:
		writeErr := json.NewEncoder(stdout).Encode(value.Envelope)
		if writeErr != nil {
			writeErr = fmt.Errorf("failed to write MCP result (operation not replayed): %w", writeErr)
		}
		return errors.Join(value.Err, writeErr)
	case cliSchemaHelp:
		_, err := fmt.Fprintf(stdout, "%s\n", value.Schema)
		return err
	case cliCatalogHelp:
		var help strings.Builder
		fmt.Fprintf(&help, "%s: MCP tools\nUsage: %s TOOL --property value | --input-file PATH|-\n", value.Command, value.Command)
		for _, tool := range value.Tools {
			fmt.Fprintf(&help, "  %s\t%s\n", tool.Name, tool.Description)
		}
		help.WriteString(cliStatusHelp)
		_, err := io.WriteString(stdout, help.String())
		return err
	case cliOperationHelp:
		_, err := io.WriteString(stdout, renderCLIOperationHelp(value))
		return err
	default:
		return errors.New("invalid MCP command outcome (operation not replayed)")
	}
}

const cliStatusHelp = "Exit status: 0 for help or successful execution; 2 for invalid usage before tool dispatch; 1 for operational failure. Exit 1 never authorizes a retry.\n"

func renderCLIOperationHelp(data cliOperationHelp) string {
	var help strings.Builder
	fmt.Fprintf(&help, "%s %s: %s\nArguments: --property value or --property=value.\n", data.Command, data.Tool, data.Description)
	for _, parameter := range data.Parameters {
		fmt.Fprintf(&help, "  --%s <%s> (%s; %s)", parameter.Name, parameter.Mode, strings.Join(parameter.Types, "|"), parameter.Requirement)
		if len(parameter.Descriptions) > 0 {
			fmt.Fprintf(&help, " %s", strings.Join(parameter.Descriptions, "; "))
		}
		if len(parameter.Defaults) > 0 {
			fmt.Fprintf(&help, " Default (documentation only): %s", strings.Join(parameter.Defaults, "; "))
		}
		help.WriteByte('\n')
	}
	fmt.Fprintf(&help, "For JSON or literal text, valid JSON keeps its type; non-JSON text is a string. Quote numeric-looking strings as JSON (for example, --value='\"123\"'). Duplicate JSON keys are rejected.\nFull-schema validation applies, including conditional requirements and combinations.\nUse %s %s --help schema for the original schema.\nUse --input-file PATH or --input-file - for one JSON object in the original shape, including empty objects, dynamic keys, unprojected fields, and reserved help/input-file/base64-file/text-file properties. Do not combine input modes.\n", data.Command, data.Tool)
	help.WriteString("Use repeatable --base64-file NAME=PATH (standard padded base64) or --text-file NAME=PATH (literal UTF-8, preserving newlines) to supply strings at exposed parameter names. The --OPTION=NAME=PATH form is also accepted. Files must be regular: 8 MiB each, 16 MiB total raw bytes, 24 MiB assembled JSON; provider constraints may be smaller. File options mix with ordinary flags, never --input-file or duplicate/overlapping assignments. No nested target paths or stdin file values.\n")
	help.WriteString("Use --output-file PATH to save the raw MCP result JSON instead of stdout (32 MiB maximum). The path belongs to this command's working directory and must not exist. Tool errors still exit 1 and retain the response; local save failures never replay the operation. This saves the envelope, not decoded documents. Use JSON input for a literal output-file property.\n")
	help.WriteString(cliStatusHelp)
	return help.String()
}
