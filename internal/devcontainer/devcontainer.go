// Package devcontainer reads the part of a repository's devcontainer.json
// the platform uses: which image to run. Building images (a Dockerfile,
// features or Docker Compose) isn't supported; such a configuration falls
// back to the platform's default image, and says so.
package devcontainer

import (
	"encoding/json"
	"fmt"

	"github.com/tailscale/hujson"
)

// Paths are where devcontainer.json is looked for, in order.
var Paths = []string{".devcontainer/devcontainer.json", ".devcontainer.json"}

// MaxBytes bounds how much of a devcontainer.json is read.
const MaxBytes = 256 << 10

// Config is what the platform takes from a devcontainer.json.
type Config struct {
	Name  string
	Image string
	// Unsupported names a setting that needs an image build, which the
	// platform doesn't do; Image is then ignored.
	Unsupported string
}

// Parse reads a devcontainer.json, which is JSON with comments and
// trailing commas.
func Parse(data []byte) (Config, error) {
	std, err := hujson.Standardize(data)
	if err != nil {
		return Config{}, fmt.Errorf("devcontainer.json: %w", err)
	}
	var raw struct {
		Name              string          `json:"name"`
		Image             string          `json:"image"`
		Build             json.RawMessage `json:"build"`
		DockerFile        string          `json:"dockerFile"`
		DockerComposeFile json.RawMessage `json:"dockerComposeFile"`
		Features          json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(std, &raw); err != nil {
		return Config{}, fmt.Errorf("devcontainer.json: %w", err)
	}
	c := Config{Name: raw.Name, Image: raw.Image}
	switch {
	case len(raw.DockerComposeFile) > 0:
		c.Unsupported = "dockerComposeFile"
	case len(raw.Build) > 0 || raw.DockerFile != "":
		c.Unsupported = "build"
	case len(raw.Features) > 0 && string(raw.Features) != "{}":
		c.Unsupported = "features"
	}
	if c.Unsupported != "" {
		c.Image = ""
	}
	return c, nil
}
