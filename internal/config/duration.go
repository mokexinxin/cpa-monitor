package config

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration keeps configuration durations strongly typed while exposing the
// underlying time.Duration directly to runtime code.
type Duration struct {
	time.Duration
}

// UnmarshalYAML accepts Go duration strings such as "60s" and "1500ms".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string such as 60s")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML writes the same human-readable syntax accepted by UnmarshalYAML.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}
