package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

type cliOutcomeKind string

const (
	cliCatalogKind   cliOutcomeKind = "catalog_help"
	cliOperationKind cliOutcomeKind = "operation_help"
	cliSchemaKind    cliOutcomeKind = "schema_help"
	cliResultKind    cliOutcomeKind = "result"
	cliFailureKind   cliOutcomeKind = "failure"
)

type cliFailureCategory string

const (
	cliUsageFailure       cliFailureCategory = "usage"
	cliOperationalFailure cliFailureCategory = "operational"
)

// The wire union is checked before constructing an internal outcome or writing
// anything. In particular, help must never become an output-file payload.
type cliCommandResponse struct {
	Kind      cliOutcomeKind    `json:"kind"`
	Catalog   *cliCatalogHelp   `json:"catalog,omitempty"`
	Operation *cliOperationHelp `json:"operation,omitempty"`
	Schema    json.RawMessage   `json:"schema,omitempty"`
	Result    json.RawMessage   `json:"result,omitempty"`
	Failure   *cliFailureStatus `json:"failure,omitempty"`
}

type cliFailureStatus struct {
	Category   cliFailureCategory `json:"category"`
	Diagnostic string             `json:"diagnostic"`
}

func decodeCLIResponse(data []byte) (cliCommandResponse, error) {
	var response cliCommandResponse
	if err := validateCLIResponseObject(data, []string{"kind", "catalog", "operation", "schema", "result", "failure"}); err != nil {
		return response, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, err
	}
	return response, nil
}

// Check the fixed adapter-owned shapes, including exact field names. The JSON
// decoder otherwise accepts case aliases that can overwrite a failure category.
// Provider schemas and result envelopes remain opaque under SDK semantics.
func validateCLIResponseObject(data []byte, fields []string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("MCP command response data must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name := token.(string)
		if !slices.Contains(fields, name) {
			return fmt.Errorf("unknown MCP command response field %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate MCP command response field %q", name)
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		var children []string
		switch name {
		case "catalog":
			children = []string{"command", "tools"}
		case "operation":
			children = []string{"command", "tool", "description", "parameters"}
		case "failure":
			children = []string{"category", "diagnostic"}
		case "tools":
			children = []string{"name", "description"}
		case "parameters":
			children = []string{"name", "mode", "types", "requirement", "descriptions", "defaults"}
		}
		if children == nil {
			continue
		}
		if name == "tools" || name == "parameters" {
			var entries []json.RawMessage
			if err := json.Unmarshal(value, &entries); err != nil {
				return err
			}
			for _, entry := range entries {
				if err := validateCLIResponseObject(entry, children); err != nil {
					return err
				}
			}
		} else if err := validateCLIResponseObject(value, children); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("MCP command response contains trailing data")
	}
	return nil
}

func cliOutcomeError(outcome cliOutcome) error {
	switch value := outcome.(type) {
	case cliFailure:
		return value.Err
	case cliResult:
		return value.Err
	default:
		return nil
	}
}

func cliResponse(outcome cliOutcome) cliCommandResponse {
	var response cliCommandResponse
	switch value := outcome.(type) {
	case cliCatalogHelp:
		response.Kind, response.Catalog = cliCatalogKind, &value
	case cliOperationHelp:
		response.Kind, response.Operation = cliOperationKind, &value
	case cliSchemaHelp:
		response.Kind, response.Schema = cliSchemaKind, value.Schema
	case cliResult:
		response.Kind, response.Result = cliResultKind, value.Envelope
	case cliFailure:
		response.Kind = cliFailureKind
	}
	if err := cliOutcomeError(outcome); err != nil {
		category := cliOperationalFailure
		if CLIExitCode(err) == 2 {
			category = cliUsageFailure
		}
		response.Failure = &cliFailureStatus{Category: category, Diagnostic: err.Error()}
	}
	return response
}

func (c cliCommandResponse) outcome(command string, request cliCommandRequest) (cliOutcome, error) {
	invalid := errors.New("invalid MCP command outcome (operation not replayed)")
	var failure error
	if c.Failure != nil {
		if c.Failure.Diagnostic == "" {
			return nil, invalid
		}
		switch c.Failure.Category {
		case cliUsageFailure:
			failure = cliUsageErrorf("%s", c.Failure.Diagnostic)
		case cliOperationalFailure:
			failure = errors.New(c.Failure.Diagnostic)
		default:
			return nil, invalid
		}
	}
	fields := 0
	for _, present := range []bool{c.Catalog != nil, c.Operation != nil, c.Schema != nil, c.Result != nil} {
		if present {
			fields++
		}
	}
	if c.Kind == cliFailureKind {
		if fields != 0 || failure == nil {
			return nil, invalid
		}
		return cliFailure{Err: failure}, nil
	}
	if fields != 1 || c.Kind != request.Kind {
		return nil, invalid
	}
	if c.Kind != cliResultKind && failure != nil {
		return nil, invalid
	}
	switch c.Kind {
	case cliCatalogKind:
		if c.Catalog != nil && c.Catalog.Command == command {
			for _, tool := range c.Catalog.Tools {
				if tool.Name == "" {
					return nil, invalid
				}
			}
			return *c.Catalog, nil
		}
	case cliOperationKind:
		if c.Operation != nil && c.Operation.Command == command && c.Operation.Tool == request.Tool {
			for _, parameter := range c.Operation.Parameters {
				if parameter.Name == "" || parameter.Requirement == "" || len(parameter.Types) == 0 || slices.Contains(parameter.Types, "") {
					return nil, invalid
				}
				switch parameter.Mode {
				case "JSON", "literal text", "JSON or literal text":
				default:
					return nil, invalid
				}
			}
			return *c.Operation, nil
		}
	case cliSchemaKind:
		data := bytes.TrimSpace(c.Schema)
		if len(data) > 0 && json.Valid(data) && (data[0] == '{' || bytes.Equal(data, []byte("true")) || bytes.Equal(data, []byte("false"))) {
			return cliSchemaHelp{Schema: c.Schema}, nil
		}
	case cliResultKind:
		data := bytes.TrimSpace(c.Result)
		if len(data) > 0 && data[0] == '{' && json.Valid(data) && CLIExitCode(failure) != 2 {
			return cliResult{Envelope: c.Result, Err: failure}, nil
		}
	}
	return nil, invalid
}
