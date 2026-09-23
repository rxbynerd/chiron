package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ValidateStrictSchema reports whether schema satisfies the structured-output
// strict-mode rules that OpenAI enforces when a request sets strict: true,
// which Generate always does for a structured request: every object schema,
// at any depth, must set additionalProperties to false and list every one of
// its properties in required. An optional field is expressed as a union with
// null, not by omitting it from required. The error names the JSON Pointer of
// the first offending object.
func ValidateStrictSchema(schema json.RawMessage) error {
	if err := checkStrictSchema(schema); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	return nil
}

// checkStrictSchema is ValidateStrictSchema without the package prefix, so
// FakeServer can embed the message in a provider-shaped error body.
func checkStrictSchema(schema json.RawMessage) error {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("strict schema is not valid JSON: %w", err)
	}
	node, ok := root.(map[string]any)
	if !ok {
		return errors.New("strict schema must be a JSON object")
	}
	return checkStrictNode(node, "")
}

// Keywords whose values hold subschemas: a map of named schemas, a single
// schema or array of schemas, and an array of schemas respectively.
var (
	namedSubschemaKeywords = []string{"properties", "$defs", "definitions"}
	subschemaKeywords      = []string{"items", "not"}
	subschemaListKeywords  = []string{"prefixItems", "anyOf", "oneOf", "allOf"}
)

func checkStrictNode(node map[string]any, pointer string) error {
	if isObjectSchema(node) {
		if err := checkStrictObject(node, pointer); err != nil {
			return err
		}
	}
	for _, kw := range namedSubschemaKeywords {
		named, _ := node[kw].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(named)) {
			if err := checkSubschema(named[name], pointer+"/"+kw+"/"+escapePointerToken(name)); err != nil {
				return err
			}
		}
	}
	for _, kw := range subschemaKeywords {
		if err := checkSubschema(node[kw], pointer+"/"+kw); err != nil {
			return err
		}
	}
	for _, kw := range subschemaListKeywords {
		list, _ := node[kw].([]any)
		for i, sub := range list {
			if err := checkSubschema(sub, fmt.Sprintf("%s/%s/%d", pointer, kw, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSubschema descends into v when it is a schema object or, for the
// tuple form of items, an array of schema objects. Boolean schemas and
// absent keywords carry no object to check.
func checkSubschema(v any, pointer string) error {
	switch sub := v.(type) {
	case map[string]any:
		return checkStrictNode(sub, pointer)
	case []any:
		for i, elem := range sub {
			if err := checkSubschema(elem, fmt.Sprintf("%s/%d", pointer, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkStrictObject(node map[string]any, pointer string) error {
	if closed, ok := node["additionalProperties"].(bool); !ok || closed {
		return fmt.Errorf("strict schema object at %s: additionalProperties must be false", describePointer(pointer))
	}
	required := map[string]bool{}
	list, _ := node["required"].([]any)
	for _, r := range list {
		if name, ok := r.(string); ok {
			required[name] = true
		}
	}
	props, _ := node["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(props)) {
		if !required[name] {
			return fmt.Errorf("strict schema object at %s: property %q must be listed in required", describePointer(pointer), name)
		}
	}
	return nil
}

// isObjectSchema reports whether node describes an object: its type is (or
// includes) "object", or it declares properties.
func isObjectSchema(node map[string]any) bool {
	if _, ok := node["properties"]; ok {
		return true
	}
	switch t := node["type"].(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
	}
	return false
}

func escapePointerToken(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func describePointer(pointer string) string {
	if pointer == "" {
		return "the root"
	}
	return pointer
}
