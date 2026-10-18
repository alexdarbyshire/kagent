package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	schemaValidator "github.com/santhosh-tekuri/jsonschema/v6"
)

// A plan belongs to one selected tool in one live discovery snapshot. Its
// schema, projection, validator and help data are read-only after compilation.
type cliCommandPlan struct {
	command    string
	tool       string
	schema     json.RawMessage
	validator  *schemaValidator.Schema
	parameters map[string]cliParameter
	help       cliOperationHelp
}

func compileCLIPlan(command, tool, description string, schema json.RawMessage) (*cliCommandPlan, error) {
	schema = bytes.Clone(schema)
	value, err := decodeJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("unsupported tool schema: %w", err)
	}
	compiler := schemaValidator.NewCompiler()
	compiler.UseLoader(cliSchemaLoader{})
	if err := compiler.AddResource("urn:kagent:mcp-tool", value); err != nil {
		return nil, fmt.Errorf("unsupported tool schema: %w", err)
	}
	validator, err := compiler.Compile("urn:kagent:mcp-tool")
	if err != nil {
		return nil, fmt.Errorf("unsupported tool schema: %w", err)
	}
	parameters := cliParameters(validator)
	help, err := cliOperationHelpData(command, tool, description, validator, parameters)
	if err != nil {
		return nil, err
	}
	return &cliCommandPlan{command: command, tool: tool, schema: schema, validator: validator, parameters: parameters, help: help}, nil
}

func (p *cliCommandPlan) prepare(request cliCommandRequest) (cliOutcome, map[string]any, error) {
	switch request.Kind {
	case cliOperationKind:
		return p.help, nil, nil
	case cliSchemaKind:
		return cliSchemaHelp{Schema: p.schema}, nil, nil
	}
	arguments, err := p.arguments(request)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid arguments for %s %s: %w", p.command, p.tool, err)
	}
	fileBacked := slices.ContainsFunc(request.Assignments, func(a cliAssignment) bool { return a.Literal })
	if fileBacked {
		data, err := json.Marshal(arguments)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to encode file-backed arguments: %w", err)
		}
		if len(data) > cliRequestLimit {
			return nil, nil, cliUsageErrorf("assembled file-backed request exceeds 24 MiB")
		}
	}
	if err := p.validator.Validate(arguments); err != nil {
		if fileBacked {
			err = cliFileValidationError(err)
		}
		return nil, nil, cliUsageErrorf("invalid arguments for %s %s: %w", p.command, p.tool, err)
	}
	return nil, arguments, nil
}

func (p *cliCommandPlan) arguments(request cliCommandRequest) (map[string]any, error) {
	if request.Input != nil {
		object, err := decodeJSON(request.Input.Data)
		if err != nil {
			return nil, cliUsageErrorf("invalid JSON input: %w", err)
		}
		mapped, ok := object.(map[string]any)
		if !ok {
			return nil, cliUsageErrorf("JSON input must be an object")
		}
		return mapped, nil
	}
	result := map[string]any{}
	var assigned [][]string
	for _, assignment := range request.Assignments {
		name, value := assignment.Name, assignment.Value
		parameter, ok := p.parameters[name]
		if !ok {
			return nil, cliUsageErrorf("unknown argument --%s", name)
		}
		for _, path := range assigned {
			common := min(len(path), len(parameter.path))
			if slices.Equal(path[:common], parameter.path[:common]) {
				return nil, cliUsageErrorf("duplicate or overlapping argument --%s", name)
			}
		}
		assigned = append(assigned, parameter.path)
		target := result
		for _, parent := range parameter.path[:len(parameter.path)-1] {
			if target[parent] == nil {
				target[parent] = map[string]any{}
			}
			var ok bool
			target, ok = target[parent].(map[string]any)
			if !ok {
				return nil, cliUsageErrorf("overlapping argument --%s", name)
			}
		}
		key := parameter.path[len(parameter.path)-1]
		if _, ok := target[key]; ok {
			return nil, cliUsageErrorf("duplicate argument --%s", name)
		}
		if assignment.Literal || parameter.mode == cliLiteralText {
			target[key] = value
			continue
		}
		parsed, err := decodeJSON([]byte(value))
		if err != nil {
			if parameter.mode == cliJSONOrText && !json.Valid([]byte(value)) {
				target[key] = value
				continue
			}
			return nil, cliUsageErrorf("invalid --%s JSON value: %w", name, err)
		}
		target[key] = parsed
	}
	return result, nil
}
