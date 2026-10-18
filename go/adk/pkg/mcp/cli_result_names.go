package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Field identities are shared by every offered name. Values remain raw so
// unselected provider metadata is never interpreted or normalized.
type cliResultField struct {
	value        json.RawMessage
	err          error
	contentIndex int
}

type cliResultNames struct {
	literal map[string]*cliResultField
	groups  map[string]map[string]*cliResultField
	arrays  map[string]*cliResultField
	native  map[string][]*cliResultField
}

func (c cliResultNames) selectString(name string) (string, error) {
	field, ok := c.literal[name]
	if !ok {
		matches := len(c.native[name])
		if matches > 0 {
			field = c.native[name][0]
		}
		// Match qualifications without copying each group prefix into every
		// child name. Even a long group name with many children stays linear.
		for group, children := range c.groups {
			if child := children[name]; child != nil {
				field = child
				matches++
			}
			if suffix, ok := strings.CutPrefix(name, group); ok && strings.HasPrefix(suffix, ".") {
				if child := children[suffix[1:]]; child != nil {
					field = child
					matches++
				}
			}
		}
		if matches != 1 {
			return "", errors.New("named output is missing or ambiguous; select exactly one string")
		}
	}
	return cliResultString(field)
}

func cliResultString(field *cliResultField) (string, error) {
	if field == nil {
		return "", errors.New("named output must select one string in every row")
	}
	if field.err != nil {
		return "", field.err
	}
	value := bytes.TrimSpace(field.value)
	if len(value) == 0 || value[0] != '"' {
		return "", errors.New("named output must select one string")
	}
	if err := validateCLIResultString(value); err != nil {
		return "", err
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", errors.New("invalid named output string")
	}
	return text, nil
}

func projectCLIResult(envelope json.RawMessage) (cliResultNames, error) {
	names := cliResultNames{literal: map[string]*cliResultField{}, groups: map[string]map[string]*cliResultField{}, arrays: map[string]*cliResultField{}, native: map[string][]*cliResultField{}}
	root, err := cliResultObject(envelope)
	if err != nil {
		return names, errors.New("invalid MCP result object")
	}
	structured, present := root["structuredContent"]
	if present {
		fields, err := cliResultObject(structured.value)
		if err != nil {
			return names, errors.New("structuredContent must be an object")
		}
		names.literal = fields
		for group, field := range fields {
			field.err = errors.Join(structured.err, field.err)
			if value := bytes.TrimSpace(field.value); len(value) > 0 && value[0] == '[' {
				names.arrays[group] = field
				continue
			}
			children, err := cliResultObject(field.value)
			if err != nil {
				continue
			}
			for _, value := range children {
				value.err = errors.Join(field.err, value.err)
			}
			names.groups[group] = children
		}
	}
	content, ok := root["content"]
	if !ok {
		return names, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content.value, &blocks); err != nil {
		return names, errors.New("invalid MCP content")
	}
	for index, block := range blocks {
		fields, err := cliResultObject(block)
		if err != nil {
			continue
		}
		kind := fields["type"]
		if kind == nil {
			continue
		}
		var contentType string
		if json.Unmarshal(kind.value, &contentType) != nil {
			continue
		}
		var name string
		var value *cliResultField
		switch contentType {
		case "text":
			if present {
				continue
			}
			name, value = "text", fields["text"]
		case "image":
			name, value = "image", fields["data"]
		case "resource":
			resource := fields["resource"]
			if resource == nil {
				continue
			}
			children, err := cliResultObject(resource.value)
			if err != nil {
				continue
			}
			for _, part := range []string{"text", "blob"} {
				if value := children[part]; value != nil {
					value.err = errors.Join(content.err, kind.err, resource.err, value.err)
					value.contentIndex = index
					names.offer("content.resource."+part, value)
					names.offer("resource."+part, value)
				}
			}
		}
		if name != "" && value == nil {
			value = &cliResultField{err: errors.New("named output must select one string")}
		}
		if value != nil {
			value.err = errors.Join(content.err, kind.err, value.err)
			value.contentIndex = index
			names.offer("content."+name, value)
			names.offer(name, value)
		}
	}
	return names, nil
}

func (c cliResultNames) offer(name string, field *cliResultField) {
	c.native[name] = append(c.native[name], field)
}

func cliResultObject(data json.RawMessage) (map[string]*cliResultField, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	fields := map[string]*cliResultField{}
	for decoder.More() {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid JSON member name")
		}
		key := bytes.TrimSpace(data[start:decoder.InputOffset()])
		key = bytes.TrimSpace(bytes.TrimPrefix(key, []byte(",")))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if previous := fields[name]; previous != nil {
			previous.err = errors.New("duplicate JSON member along selected output path")
			continue
		}
		fields[name] = &cliResultField{value: value, err: validateCLIResultString(key), contentIndex: -1}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

// encoding/json replaces malformed Unicode. Check only selected strings and
// their member names before decoding; unrelated provider data stays opaque.
func validateCLIResultString(raw []byte) error {
	invalid := errors.New("invalid Unicode in selected output path")
	if !utf8.Valid(raw) {
		return invalid
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return invalid
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return invalid
		}
		i += 4
		if code < 0xd800 || code > 0xdfff {
			continue
		}
		if code >= 0xdc00 || i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return invalid
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return invalid
		}
		i += 6
	}
	return nil
}
