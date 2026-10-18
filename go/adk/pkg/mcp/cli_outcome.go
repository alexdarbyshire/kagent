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
	help.WriteString("Use one --save-text NAME=PATH or --save-base64 NAME=PATH with --output-file to save one named string, with empty stdout. Both --OPTION value and --OPTION=value are accepted; paths may contain equals signs. Names are actual structuredContent properties, group.child immediate children, or unique child shortcuts; literal names take precedence. Native names are content.text, content.image, content.resource.text and content.resource.blob, with unambiguous shortcuts text, image, resource.text and resource.blob. Structured results suppress compatibility text; text containing JSON stays literal. Multiple matching blocks, missing/ambiguous names and non-string values fail. Text preserves UTF-8 and newlines; base64 must be standard, padded and canonical. Empty strings create empty files. Optional --verify-sha256 NAME checks a selected 64-hex-digit digest of the saved bytes; no digest is inferred. Limits: 32 MiB selected string/text, 24 MiB decoded binary, within the 32 MiB source envelope. Destinations must be distinct new files; no parent directories are created. Raw JSON is finalized first and retained if extraction fails; incomplete artifacts are removed. Use JSON input for properties named save-text, save-base64 or verify-sha256.\n")
	help.WriteString("Alternatively, --save-text-dir GROUP.FIELD=DIR or --save-base64-dir GROUP.FIELD=DIR saves every row of a top-level array of objects, or every matching native content block, into a new 0700 directory. One save request per command, always with --output-file outside the directory. Every selected member must be a string; empty arrays succeed without observing FIELD. Array names require qualification; literal properties take precedence and never fall back to arrays. Collection --verify-sha256 GROUP.FIELD must select a digest in the same array row; native groups may use a singular named digest. Files are 0600 and named 0001, 0002, ... in response order, never provider filenames. manifest.json is finalized last with version, target, conversion, exact raw-file SHA-256 and each file's name, zero-based source index, length and SHA-256; a directory without a complete manifest is unfinished. Limits: 100 files, 32 MiB aggregate payload, 1 MiB manifest, plus existing per-file and raw limits. Failures remove owned incomplete collections, preserve replacements and retain finalized raw JSON. Saving does not fetch URLs, poll or consume downloads. Use JSON input for properties named save-text-dir or save-base64-dir.\n")
	help.WriteString(cliStatusHelp)
	help.WriteString("Recover from retained JSON without another tool call: mcp-cli extract --from-file RESPONSE.json --save-text NAME=PATH (or --save-base64, --save-text-dir, --save-base64-dir). Use mcp-cli extract --help for local recovery.\n")
	return help.String()
}
