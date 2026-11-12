package config

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unboundFields lists the ResearchConfig leaf fields, by dotted Go path, that
// no flag sets, each with the reason. A field belongs here only when it is
// deliberately not settable by flag.
var unboundFields = map[string]string{}

// TestRegisterFlags_EveryFlagHasAConfigBinding fails for any registered flag
// that ApplyFlags does not copy onto the config, so a flag cannot be parsed,
// listed in --help and then silently ignored.
func TestRegisterFlags_EveryFlagHasAConfigBinding(t *testing.T) {
	for _, name := range registeredFlagNames(t) {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			changed, err := fieldsChangedBySetting(name)
			must.NoError(err)
			must.NotEmpty(changed, "--%s was set but ApplyFlags left the config unchanged", name)
		})
	}
}

// TestRegisterFlags_EveryConfigFieldHasAFlag fails for any leaf field of
// ResearchConfig, including Fleet and Telemetry, that no flag changes and
// that unboundFields does not list.
func TestRegisterFlags_EveryConfigFieldHasAFlag(t *testing.T) {
	must := require.New(t)
	covered := map[string]bool{}
	for _, name := range registeredFlagNames(t) {
		changed, err := fieldsChangedBySetting(name)
		must.NoError(err)
		for _, field := range changed {
			covered[field] = true
		}
	}

	for field := range leafFields(reflect.ValueOf(Default()), "") {
		t.Run(field, func(t *testing.T) {
			check := assert.New(t)
			if reason, ok := unboundFields[field]; ok {
				check.NotEmpty(reason, "an unboundFields entry must say why %s has no flag", field)
				check.False(covered[field], "%s is listed in unboundFields but a flag sets it", field)
				return
			}
			check.True(covered[field], "config field %s has no flag", field)
		})
	}
}

// registeredFlagNames returns the name of every flag RegisterFlags adds.
func registeredFlagNames(t *testing.T) []string {
	t.Helper()
	var names []string
	newFlagSet(t).VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	return names
}

// fieldsChangedBySetting sets the named flag to a value that differs from its
// default, applies the flags to Default() and returns the dotted paths of the
// leaf fields that changed.
func fieldsChangedBySetting(name string) ([]string, error) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	f := fs.Lookup(name)
	value, err := valueDifferingFromDefault(f)
	if err != nil {
		return nil, err
	}
	if err := fs.Set(name, value); err != nil {
		return nil, fmt.Errorf("set --%s=%q: %w", name, value, err)
	}

	before := Default()
	after := before
	if err := ApplyFlags(&after, fs); err != nil {
		return nil, fmt.Errorf("apply --%s=%q: %w", name, value, err)
	}

	afterFields := leafFields(reflect.ValueOf(after), "")
	var changed []string
	for field, was := range leafFields(reflect.ValueOf(before), "") {
		if !assert.ObjectsAreEqual(was, afterFields[field]) {
			changed = append(changed, field)
		}
	}
	return changed, nil
}

// valueDifferingFromDefault returns a value for f that cannot equal its
// default. It errors on a flag type it does not know, so a new flag type
// forces this helper to be extended.
func valueDifferingFromDefault(f *pflag.Flag) (string, error) {
	switch f.Value.Type() {
	case "bool":
		def, err := strconv.ParseBool(f.DefValue)
		if err != nil {
			return "", err
		}
		return strconv.FormatBool(!def), nil
	case "string":
		return "chiron-test-" + f.Name, nil
	case "int":
		def, err := strconv.Atoi(f.DefValue)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(def + 7), nil
	case "float64":
		def, err := strconv.ParseFloat(f.DefValue, 64)
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(def+1.5, 'f', -1, 64), nil
	case "duration":
		def, err := time.ParseDuration(f.DefValue)
		if err != nil {
			return "", err
		}
		return (def + time.Minute).String(), nil
	case "stringSlice", "stringArray":
		// The name=value form is the one the mcp flag requires; the other
		// list flags accept any element.
		return "chiron-test=value", nil
	}
	return "", fmt.Errorf("--%s has flag type %q, which valueDifferingFromDefault does not handle", f.Name, f.Value.Type())
}

// leafFields maps the dotted path of every leaf field under v to its value.
// Structs are descended into; every other kind, including Duration, slices
// and maps, is a leaf.
func leafFields(v reflect.Value, prefix string) map[string]any {
	out := map[string]any{}
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		path := prefix + field.Name
		if field.Type.Kind() == reflect.Struct {
			for p, value := range leafFields(v.Field(i), path+".") {
				out[p] = value
			}
			continue
		}
		out[path] = v.Field(i).Interface()
	}
	return out
}
