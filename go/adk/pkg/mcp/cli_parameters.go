package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	schemaValidator "github.com/santhosh-tekuri/jsonschema/v6"
)

type cliParameter struct {
	path         []string
	mode         cliParseMode
	declarations []*schemaValidator.Schema
}

type cliParseMode int

const (
	cliJSONValue cliParseMode = iota
	cliLiteralText
	cliJSONOrText
)

func cliParameters(schema *schemaValidator.Schema) map[string]cliParameter {
	properties, _ := cliDeclaredProperties([]*schemaValidator.Schema{schema})
	literals := map[string]cliParameter{}
	groups := map[string]map[string][]*schemaValidator.Schema{}
	for name, declarations := range properties {
		children, complete := cliDeclaredProperties(declarations)
		if complete && len(children) > 0 && cliPathHasType(schema, []string{name}, "object") {
			groups[name] = children
		} else {
			literals[name] = cliNewParameter(schema, []string{name}, declarations)
		}
	}
	wrappers := map[string]bool{}
	history := []map[string]bool{wrappers}
	for {
		reserved := maps.Clone(literals)
		for name := range wrappers {
			reserved[name] = cliNewParameter(schema, []string{name}, properties[name])
		}
		parameters, missing := cliProjectParameters(schema, groups, reserved)
		if maps.Equal(wrappers, missing) {
			return parameters
		}
		cycle := slices.IndexFunc(history, func(previous map[string]bool) bool {
			return maps.Equal(previous, missing)
		})
		if cycle >= 0 {
			// A wrapper can qualify its own children and make itself redundant.
			// Groups with no stable spelling retain JSON input; every cycle removes
			// at least one such group, while unaffected groups remain projectable.
			for name := range groups {
				for _, previous := range history[cycle:] {
					if previous[name] != missing[name] {
						literals[name] = cliNewParameter(schema, []string{name}, properties[name])
						delete(groups, name)
						break
					}
				}
			}
			wrappers = map[string]bool{}
			history = []map[string]bool{wrappers}
			continue
		}
		// Replace reservations together: a fully represented wrapper releases
		// its name, including for children projected in later passes.
		wrappers = missing
		history = append(history, wrappers)
	}
}

func cliProjectParameters(schema *schemaValidator.Schema, groups map[string]map[string][]*schemaValidator.Schema, literals map[string]cliParameter) (map[string]cliParameter, map[string]bool) {
	parameters := make(map[string]cliParameter, len(literals))
	for name, parameter := range literals {
		if cliFlagName(name) && !cliReservedFlag(name) {
			parameters[name] = parameter
		}
	}
	counts := map[string]int{}
	for _, children := range groups {
		for child := range children {
			counts[child]++
		}
	}
	candidates := map[string][]cliParameter{}
	for group, children := range groups {
		for child, declarations := range children {
			name := child
			_, reserved := literals[name]
			if counts[child] > 1 || reserved || cliReservedFlag(name) {
				name = group + "." + child
			}
			candidates[name] = append(candidates[name], cliNewParameter(schema, []string{group, child}, declarations))
		}
	}
	missing := map[string]bool{}
	for name, candidates := range candidates {
		_, reserved := literals[name]
		if len(candidates) == 1 && !reserved && cliFlagName(name) && !cliReservedFlag(name) {
			parameters[name] = candidates[0]
			continue
		}
		for _, candidate := range candidates {
			missing[candidate.path[0]] = true
		}
	}
	return parameters, missing
}

func cliFlagName(name string) bool {
	return name != "" && !strings.Contains(name, "=")
}

func cliNewParameter(schema *schemaValidator.Schema, path []string, declarations []*schemaValidator.Schema) cliParameter {
	mode := cliJSONValue
	if cliPathHasType(schema, path, "string") {
		mode = cliLiteralText
	} else {
		excluded := cliPathType(schema, path, cliExcludesString, map[cliSchemaPosition]bool{})
		if !excluded.matches && !excluded.uniform {
			mode = cliJSONOrText
		}
	}
	return cliParameter{path: path, mode: mode, declarations: declarations}
}

// Discovery unions declared names; it does not decide which branch can validate.
// The compiled schema already resolves local references and enforces draft rules.
func cliDeclaredProperties(declarations []*schemaValidator.Schema) (map[string][]*schemaValidator.Schema, bool) {
	properties := map[string][]*schemaValidator.Schema{}
	complete := true
	for _, declaration := range declarations {
		cliWalkSchema(declaration, map[*schemaValidator.Schema]bool{}, func(schema *schemaValidator.Schema) {
			for name, property := range schema.Properties {
				if !slices.Contains(properties[name], property) {
					properties[name] = append(properties[name], property)
				}
			}
		}, &complete)
	}
	return properties, complete
}

func cliWalkSchema(schema *schemaValidator.Schema, visiting map[*schemaValidator.Schema]bool, visit func(*schemaValidator.Schema), complete *bool) {
	if schema == nil {
		return
	}
	if visiting[schema] || schema.DynamicRef != nil || schema.RecursiveRef != nil {
		*complete = false
		return
	}
	visiting[schema] = true
	defer delete(visiting, schema)
	visit(schema)
	for _, child := range []*schemaValidator.Schema{schema.Ref, schema.If, schema.Then, schema.Else} {
		cliWalkSchema(child, visiting, visit, complete)
	}
	for _, branches := range [][]*schemaValidator.Schema{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, child := range branches {
			cliWalkSchema(child, visiting, visit, complete)
		}
	}
}

type cliSchemaPosition struct {
	schema    *schemaValidator.Schema
	remaining int
}

type cliDeclaredType struct {
	declared bool
	matches  bool // An unconditional assertion establishes the queried type fact.
	uniform  bool // Every declaration agrees, including conditional branches.
}

func cliPathHasType(schema *schemaValidator.Schema, path []string, name string) bool {
	result := cliPathType(schema, path, func(node *schemaValidator.Schema) bool {
		return cliExplicitType(node, name)
	}, map[cliSchemaPosition]bool{})
	return result.matches || result.uniform
}

// Conjunctions retain positive type assertions; alternatives must agree across
// their declarations. An absent property is different from an untyped declaration:
// branch-only fields retain their declared types without treating an explicitly
// untyped alternative as string-only. Full-schema validation stays authoritative.
func cliPathType(schema *schemaValidator.Schema, path []string, matches func(*schemaValidator.Schema) bool, visiting map[cliSchemaPosition]bool) cliDeclaredType {
	if schema == nil {
		return cliDeclaredType{}
	}
	position := cliSchemaPosition{schema: schema, remaining: len(path)}
	if visiting[position] || schema.DynamicRef != nil || schema.RecursiveRef != nil {
		return cliDeclaredType{declared: true}
	}
	visiting[position] = true
	defer delete(visiting, position)
	var result cliDeclaredType
	if len(path) == 0 {
		matched := matches(schema)
		result = cliDeclaredType{declared: true, matches: matched, uniform: matched}
	} else {
		result = cliPathType(schema.Properties[path[0]], path[1:], matches, visiting)
	}
	ref := cliPathType(schema.Ref, path, matches, visiting)
	result = cliConjoinedType(result, ref)
	for _, child := range schema.AllOf {
		constraint := cliPathType(child, path, matches, visiting)
		result = cliConjoinedType(result, constraint)
	}
	for _, branches := range [][]*schemaValidator.Schema{schema.AnyOf, schema.OneOf} {
		result = cliConjoinedType(result, cliAlternativeType(branches, path, matches, visiting))
	}
	// If is a predicate, not a positive assertion. Then/else declarations may
	// agree on a branch-only field, but cannot narrow an unconditional mixed type.
	conditional := cliAlternativeType([]*schemaValidator.Schema{schema.Then, schema.Else}, path, matches, visiting)
	conditional.matches = false
	return cliConjoinedType(result, conditional)
}

func cliConjoinedType(left, right cliDeclaredType) cliDeclaredType {
	if !left.declared {
		return right
	}
	if !right.declared {
		return left
	}
	return cliDeclaredType{
		declared: true,
		matches:  left.matches || right.matches,
		uniform:  (left.matches || left.uniform) && (right.matches || right.uniform),
	}
}

func cliAlternativeType(branches []*schemaValidator.Schema, path []string, matches func(*schemaValidator.Schema) bool, visiting map[cliSchemaPosition]bool) cliDeclaredType {
	result := cliDeclaredType{matches: true, uniform: true}
	for _, child := range branches {
		alternative := cliPathType(child, path, matches, visiting)
		if alternative.declared {
			result.declared = true
			result.matches = result.matches && alternative.matches
			result.uniform = result.uniform && (alternative.matches || alternative.uniform)
		}
	}
	if !result.declared {
		return cliDeclaredType{}
	}
	return result
}

func cliExplicitType(schema *schemaValidator.Schema, name string) bool {
	if schema.Types != nil && slices.Equal(schema.Types.ToStrings(), []string{name}) {
		return true
	}
	if name != "string" {
		return false
	}
	if schema.Const != nil {
		if _, ok := (*schema.Const).(string); ok {
			return true
		}
	}
	if schema.Enum != nil && len(schema.Enum.Values) > 0 {
		for _, value := range schema.Enum.Values {
			if _, ok := value.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

func cliExcludesString(schema *schemaValidator.Schema) bool {
	if schema.Types != nil && !slices.Contains(schema.Types.ToStrings(), "string") {
		return true
	}
	if schema.Const != nil {
		if _, ok := (*schema.Const).(string); !ok {
			return true
		}
	}
	if schema.Enum != nil {
		for _, value := range schema.Enum.Values {
			if _, ok := value.(string); ok {
				return false
			}
		}
		return true
	}
	return false
}

func cliOperationHelpData(command, tool, description string, schema *schemaValidator.Schema, parameters map[string]cliParameter) (cliOperationHelp, error) {
	help := cliOperationHelp{Command: command, Tool: tool, Description: description}
	for _, name := range slices.Sorted(maps.Keys(parameters)) {
		parameter := parameters[name]
		mode := "JSON"
		switch parameter.mode {
		case cliLiteralText:
			mode = "literal text"
		case cliJSONOrText:
			mode = "JSON or literal text"
		}
		var types, descriptions, defaults []string
		var annotationErr error
		for _, declaration := range parameter.declarations {
			complete := true
			cliWalkSchema(declaration, map[*schemaValidator.Schema]bool{}, func(node *schemaValidator.Schema) {
				if node.Types != nil {
					types = append(types, node.Types.ToStrings()...)
				}
				if node.Description != "" {
					descriptions = append(descriptions, strings.Join(strings.Fields(node.Description), " "))
				}
				if node.Default != nil {
					data, err := json.Marshal(*node.Default)
					if err != nil {
						annotationErr = err
						return
					}
					defaults = append(defaults, string(data))
				}
			}, &complete)
		}
		if annotationErr != nil {
			return cliOperationHelp{}, fmt.Errorf("failed to describe --%s default: %w", name, annotationErr)
		}
		slices.Sort(types)
		types = slices.Compact(types)
		if len(types) == 0 {
			types = []string{"schema-defined"}
		}
		help.Parameters = append(help.Parameters, cliParameterDescription{
			Name: name, Mode: mode, Types: types,
			Requirement:  cliParameterRequirement(schema, parameter.path),
			Descriptions: slices.Compact(descriptions), Defaults: slices.Compact(defaults),
		})
	}
	return help, nil
}

func cliParameterRequirement(schema *schemaValidator.Schema, path []string) string {
	parents := []*schemaValidator.Schema{schema}
	required := true
	possiblyRequired := false
	conditional := false
	for _, name := range path {
		levelRequired := len(parents) > 0
		properties, _ := cliDeclaredProperties(parents)
		for _, parent := range parents {
			levelRequired = cliRequiredProperty(parent, name, map[*schemaValidator.Schema]bool{}) && levelRequired
			complete := true
			cliWalkSchema(parent, map[*schemaValidator.Schema]bool{}, func(node *schemaValidator.Schema) {
				possiblyRequired = possiblyRequired || slices.Contains(node.Required, name)
				conditional = conditional || len(node.AnyOf) > 0 || len(node.OneOf) > 0 || node.If != nil
			}, &complete)
		}
		required = required && levelRequired
		parents = properties[name]
	}
	if required && !conditional {
		return "required"
	}
	if possiblyRequired {
		return "conditional requirement; see schema (including parent presence)"
	}
	return "optional; schema conditions may apply"
}

func cliRequiredProperty(schema *schemaValidator.Schema, name string, visiting map[*schemaValidator.Schema]bool) bool {
	if schema == nil || visiting[schema] {
		return false
	}
	visiting[schema] = true
	defer delete(visiting, schema)
	if slices.Contains(schema.Required, name) || cliRequiredProperty(schema.Ref, name, visiting) {
		return true
	}
	for _, child := range schema.AllOf {
		if cliRequiredProperty(child, name, visiting) {
			return true
		}
	}
	return false
}
