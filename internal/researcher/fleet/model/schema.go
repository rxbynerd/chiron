package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ValidateStrictSchema checks that a schema stays inside the strict-mode
// subset every supported provider accepts when strict: true is set, as
// Generate does. Every object, at any depth, sets additionalProperties to
// false and lists all its properties in required, and no schema carries a
// length or numeric constraint other than minItems of 0 or 1. The error names
// the first offending schema's JSON Pointer.
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

// Constraint keywords at least one supported provider rejects under strict
// mode. minItems is handled separately: 0 and 1 are accepted everywhere.
var unsupportedConstraintKeywords = []string{
	"maxItems", "uniqueItems",
	"minLength", "maxLength",
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
}

func checkStrictNode(node map[string]any, pointer string) error {
	if err := checkStrictConstraints(node, pointer); err != nil {
		return err
	}
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

func checkStrictConstraints(node map[string]any, pointer string) error {
	for _, kw := range unsupportedConstraintKeywords {
		if _, ok := node[kw]; ok {
			return fmt.Errorf("strict schema at %s: %s is not supported by every provider; enforce it after decoding", describePointer(pointer), kw)
		}
	}
	if v, ok := node["minItems"]; ok {
		if n, isNumber := v.(float64); !isNumber || (n != 0 && n != 1) {
			return fmt.Errorf("strict schema at %s: minItems must be 0 or 1; enforce a larger bound after decoding", describePointer(pointer))
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
