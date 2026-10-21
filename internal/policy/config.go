package policy

import (
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

// ConfigKey is the ConfigMap key that holds a Config.
const ConfigKey = "policy.yaml"

// Config selects a policy and its parameters, for example
//
//	name: fixed
//	params:
//	  idleTimeout: 10m
type Config struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
}

// Build validates c and builds its policy.
func (c Config) Build() (Policy, error) {
	if c.Name == "" {
		return nil, errors.New("name is required")
	}
	return New(c.Name, c.Params)
}

// ParseConfig reads and validates a Config. Unknown fields are an error.
func ParseConfig(data []byte) (Config, Policy, error) {
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Config{}, nil, fmt.Errorf("parsing %s: %w", ConfigKey, err)
	}
	p, err := c.Build()
	if err != nil {
		return Config{}, nil, err
	}
	return c, p, nil
}
