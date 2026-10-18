package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// cliCommandRequest is the path-free, already parsed command. The actor supplies
// binding and authority; these values never select a connection or caller.
type cliCommandRequest struct {
	Kind        cliOutcomeKind  `json:"kind"`
	Tool        string          `json:"tool,omitempty"`
	Assignments []cliAssignment `json:"assignments,omitempty"`
	Input       *cliJSONInput   `json:"input,omitempty"`
}

type cliJSONInput struct {
	Data []byte `json:"data"`
}

type cliAssignment struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Literal bool   `json:"literal,omitempty"`
}

func (c cliCommandRequest) validate() error {
	invalid := errors.New("invalid MCP command request")
	switch c.Kind {
	case cliCatalogKind:
		if c.Tool != "" || c.Input != nil || len(c.Assignments) != 0 {
			return invalid
		}
	case cliOperationKind, cliSchemaKind:
		if c.Input != nil || len(c.Assignments) != 0 {
			return invalid
		}
	case cliResultKind:
		if c.Input != nil && len(c.Assignments) != 0 {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

type cliLocalCommand struct {
	request    cliCommandRequest
	inputPath  string
	outputPath string
	files      []cliLocalFile
}

type cliLocalFile struct {
	name string
	path string
	mode string
}

// Parse once before any local I/O. Values that resemble options are consumed as
// values here and cannot become options again across the managed handoff.
func parseCLICommand(args []string) (cliLocalCommand, error) {
	command := cliLocalCommand{request: cliCommandRequest{Kind: cliCatalogKind}}
	if len(args) == 0 || (len(args) == 1 && args[0] == "--help") {
		return command, nil
	}
	if args[0] == "--help" {
		return command, cliUsageErrorf("--help must be used alone")
	}
	command.request.Tool = args[0]
	if len(args) == 2 && args[1] == "--help" {
		command.request.Kind = cliOperationKind
		return command, nil
	}
	if len(args) == 3 && args[1] == "--help" && args[2] == "schema" {
		command.request.Kind = cliSchemaKind
		return command, nil
	}
	command.request.Kind = cliResultKind
	options, err := cliOptions(args[1:])
	if err != nil {
		return command, err
	}
	inputs := 0
	for _, option := range options {
		switch option.name {
		case "output-file":
			if command.outputPath != "" {
				return command, cliUsageErrorf("duplicate --output-file")
			}
			if option.value == "" || option.value == "-" {
				return command, cliUsageErrorf("--output-file requires a new file path, not empty or -")
			}
			command.outputPath = option.value
		case "input-file":
			inputs++
			command.request.Input = &cliJSONInput{}
			command.inputPath = option.value
		case "text-file", "base64-file":
			name, path, ok := strings.Cut(option.value, "=")
			if !ok || !cliFlagName(name) || strings.HasPrefix(name, "-") || path == "" || path == "-" {
				return command, cliUsageErrorf("--%s requires NAME=PATH with an exposed parameter name and a regular file path", option.name)
			}
			command.files = append(command.files, cliLocalFile{name: name, path: path, mode: option.name})
		default:
			command.request.Assignments = append(command.request.Assignments, cliAssignment{Name: option.name, Value: option.value})
		}
	}
	if inputs > 0 && (inputs != 1 || len(command.files) > 0 || len(command.request.Assignments) > 0) {
		return command, cliUsageErrorf("--input-file cannot be combined with argument flags")
	}
	return command, nil
}

func (c cliLocalCommand) prepare(ctx context.Context, stdin io.Reader) (cliCommandRequest, error) {
	request := c.request
	if request.Input != nil {
		var data []byte
		var err error
		if c.inputPath == "-" {
			reader, ok := stdin.(io.ReadCloser)
			if !ok {
				return request, errors.New("MCP CLI stdin must support cancellation by closing")
			}
			data, err = readCLIInput(ctx, reader)
		} else {
			data, err = readCLIFile(ctx, c.inputPath)
		}
		if err != nil {
			return request, fmt.Errorf("failed to read JSON input: %w", err)
		}
		request.Input = &cliJSONInput{Data: data}
	}
	files, err := prepareCLIFileValues(ctx, c.files)
	if err != nil {
		return request, err
	}
	request.Assignments = append(request.Assignments, files...)
	return request, nil
}

type cliOption struct{ name, value string }

func cliOptions(args []string) ([]cliOption, error) {
	var options []cliOption
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			return nil, cliUsageErrorf("expected argument flag, got %q", args[i])
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if name == "help" {
			return nil, cliUsageErrorf("--help must be used alone; use JSON input for the help property")
		}
		if !inline {
			i++
			if i >= len(args) {
				return nil, cliUsageErrorf("missing value for --%s", name)
			}
			value = args[i]
		}
		options = append(options, cliOption{name: name, value: value})
	}
	return options, nil
}

func cliReservedFlag(name string) bool {
	switch name {
	case "help", "input-file", "base64-file", "text-file", "output-file":
		return true
	default:
		return false
	}
}
