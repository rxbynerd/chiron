package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that serialises as a human-readable string
// ("30m") in both JSON and YAML, instead of nanosecond integers.
type Duration time.Duration

// String returns the standard duration form, e.g. "30m0s".
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts a duration string ("30m") or a nanosecond number.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return d.set(v)
}

// MarshalYAML encodes the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML accepts a duration string ("30m") or a nanosecond number.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var v any
	if err := node.Decode(&v); err != nil {
		return err
	}
	return d.set(v)
}

func (d *Duration) set(v any) error {
	switch v := v.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		*d = Duration(parsed)
		return nil
	case float64: // encoding/json numbers
		*d = Duration(time.Duration(v))
		return nil
	case int: // yaml.v3 integers
		*d = Duration(time.Duration(v))
		return nil
	default:
		return fmt.Errorf("duration: cannot parse %T (want a string like %q)", v, "30m")
	}
}
