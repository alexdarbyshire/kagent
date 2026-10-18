package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const cliExtractHelp = `Usage: mcp-cli extract --from-file RESPONSE.json --save-text NAME=PATH
Use one --save-text, --save-base64, --save-text-dir or --save-base64-dir NAME=PATH.
Optional --verify-sha256 NAME verifies the saved bytes. Both --option value and --option=value work.
Names are real structuredContent fields, group.child immediate children or unique child shortcuts.
Native names: content.text, content.image, content.resource.text, content.resource.blob;
unambiguous shortcuts: text, image, resource.text, resource.blob. Collections use GROUP.FIELD.
Recovery reads one regular MCP result envelope (32 MiB maximum), without binding, socket,
discovery, transformation or provider calls. It never replays the operation or changes the source.
Destinations must be new; stdout remains empty during extraction. No --output-file or stdin source.
Exit 2: malformed syntax or envelope. Exit 1: I/O, materialization or embedded MCP failure.
Exit 0: mechanical success; it cannot reconstruct the original transport or teardown status.
`

func runCLIExtract(ctx context.Context, args []string, stdout io.Writer) (err error) {
	if len(args) == 2 && args[1] == "--help" {
		_, err := io.WriteString(stdout, cliExtractHelp)
		return err
	}
	if len(args) < 2 {
		return cliUsageErrorf("extract requires --from-file and one save request; use extract --help")
	}
	command, err := parseCLILocalCommand(args, true)
	if err != nil {
		return err
	}
	data, err := readCLIBoundedFile(ctx, command.sourcePath, cliOutputLimit, errors.New("saved MCP result exceeds 32 MiB"))
	if err != nil {
		return fmt.Errorf("failed to read saved MCP result: %w", err)
	}
	result, err := savedCLIResult(data)
	if err != nil {
		return err
	}
	outputs, err := reserveCLIOutputs(ctx, command)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, outputs.finish(ctx)) }()
	return outputs.materialize(ctx, result, *command.save, fmt.Sprintf("%x", sha256.Sum256(data)))
}

// Validate envelope-owned fields without normalizing or recursively decoding
// provider values. Selected paths are checked by the shared materializer.
func savedCLIResult(data []byte) (cliResult, error) {
	invalid := cliUsageErrorf("invalid saved MCP result envelope")
	if !json.Valid(data) {
		return cliResult{}, invalid
	}
	fields, err := cliResultObject(data)
	if err != nil {
		return cliResult{}, invalid
	}
	if fields["jsonrpc"] != nil {
		return cliResult{}, invalid
	}
	content := fields["content"]
	if content == nil || content.err != nil || !bytes.HasPrefix(bytes.TrimSpace(content.value), []byte("[")) {
		return cliResult{}, invalid
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content.value, &blocks) != nil {
		return cliResult{}, invalid
	}
	for _, block := range blocks {
		members, err := cliResultObject(block)
		if err != nil {
			return cliResult{}, invalid
		}
		kind, err := cliResultString(members["type"])
		if err != nil || kind == "" {
			return cliResult{}, invalid
		}
	}
	validationContent, err := cliContentValidationCopy(content.value, true)
	if err != nil {
		return cliResult{}, invalid
	}
	var decoded mcpsdk.CallToolResult
	if err := json.Unmarshal(append(append([]byte(`{"content":`), validationContent...), '}'), &decoded); err != nil {
		return cliResult{}, invalid
	}
	result := cliResult{Envelope: data}
	if status := fields["isError"]; status != nil {
		var isError bool
		if status.err != nil || string(bytes.TrimSpace(status.value)) == "null" || json.Unmarshal(status.value, &isError) != nil {
			return cliResult{}, invalid
		}
		if isError {
			result.Err = errors.New("saved MCP result returned isError")
		}
	}
	if status := fields["resultType"]; status != nil {
		kind, err := cliResultString(status)
		if err != nil {
			return cliResult{}, invalid
		}
		if kind != "" && kind != "complete" {
			result.Err = errors.Join(result.Err, errors.New("saved MCP result type is unsupported"))
		}
	}
	return result, nil
}

// The SDK owns native variant and field-type validation. Its decoder also
// converts binary strings and generic numbers, so mask those values in a
// disposable copy; the original bytes still feed the shared materializer.
func cliContentValidationCopy(data json.RawMessage, nested bool) (json.RawMessage, error) {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, err
	}
	for _, block := range blocks {
		maskCLIContentValues(block)
		if resource := block["resource"]; bytes.HasPrefix(bytes.TrimSpace(resource), []byte("{")) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(resource, &fields); err != nil {
				return nil, err
			}
			maskCLIContentValues(fields)
			encoded, err := json.Marshal(fields)
			if err != nil {
				return nil, err
			}
			block["resource"] = encoded
		}
		// The SDK permits one nested content list in tool_result blocks.
		if content := block["content"]; nested && bytes.HasPrefix(bytes.TrimSpace(content), []byte("[")) {
			encoded, err := cliContentValidationCopy(content, false)
			if err != nil {
				return nil, err
			}
			block["content"] = encoded
		}
	}
	return json.Marshal(blocks)
}

func maskCLIContentValues(fields map[string]json.RawMessage) {
	delete(fields, "structuredContent")
	for _, name := range []string{"_meta", "input"} {
		if value := fields[name]; bytes.HasPrefix(bytes.TrimSpace(value), []byte("{")) {
			fields[name] = json.RawMessage(`{}`)
		}
	}
	for _, name := range []string{"text", "data", "blob"} {
		if value := fields[name]; bytes.HasPrefix(bytes.TrimSpace(value), []byte(`"`)) {
			fields[name] = json.RawMessage(`""`)
		}
	}
}
