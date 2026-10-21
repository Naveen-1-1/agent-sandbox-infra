// Package bench measures the platform on real repositories through the
// gateway API, the way an agent harness uses it: a cold first run (clone and
// dependency install on a cache miss), then suspend/wake cycles that each
// edit a file and run a command that needs the dependencies, alternating
// wakes started by a wake-hint and wakes started by the command itself.
package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// Repo is one repository to measure.
type Repo struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Commit  string `json:"commit"`  // full SHA: the run is reproducible
	Profile string `json:"profile"` // the dependency profile it must get: npm, pip or uv
	Image   string `json:"image"`
	// Command needs the dependencies; it runs with sh -c in /workspace.
	Command string `json:"command"`
	// Edit is a tracked file each cycle appends a line to.
	Edit string `json:"edit"`
}

// Config lists the repositories, measured in order.
type Config struct {
	Repos []Repo `json:"repos"`
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// LoadConfig reads and checks a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return errors.New("bench: no repositories")
	}
	seen := map[string]bool{}
	for _, r := range c.Repos {
		switch {
		case r.Name == "" || seen[r.Name]:
			return fmt.Errorf("bench: repository names must be unique and set (%q)", r.Name)
		case r.URL == "" || r.Image == "" || r.Command == "" || r.Edit == "":
			return fmt.Errorf("bench: %s needs url, image, command and edit", r.Name)
		case !fullSHA.MatchString(r.Commit):
			return fmt.Errorf("bench: %s: commit must be a full 40-character SHA", r.Name)
		case r.Profile != "npm" && r.Profile != "pip" && r.Profile != "uv":
			return fmt.Errorf("bench: %s: profile must be npm, pip or uv", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// Only keeps the named repositories, in config order.
func (c *Config) Only(names []string) error {
	if len(names) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var kept []Repo
	for _, r := range c.Repos {
		if want[r.Name] {
			kept = append(kept, r)
			delete(want, r.Name)
		}
	}
	for n := range want {
		return fmt.Errorf("bench: no repository named %q", n)
	}
	c.Repos = kept
	return nil
}
