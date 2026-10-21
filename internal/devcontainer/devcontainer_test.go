package devcontainer_test

import (
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/devcontainer"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, in    string
		image, skip string
	}{
		{"image with comments and a trailing comma", `{
			// the editor's image
			"name": "app",
			"image": "mcr.microsoft.com/devcontainers/javascript-node:22", /* pinned */
		}`, "mcr.microsoft.com/devcontainers/javascript-node:22", ""},
		{"no image", `{"name": "app"}`, "", ""},
		{"dockerfile build", `{"build": {"dockerfile": "Dockerfile"}, "image": "ignored"}`, "", "build"},
		{"legacy dockerFile", `{"dockerFile": "Dockerfile"}`, "", "build"},
		{"compose", `{"dockerComposeFile": ["a.yml"], "service": "app"}`, "", "dockerComposeFile"},
		{"features", `{"image": "debian", "features": {"ghcr.io/devcontainers/features/go:1": {}}}`, "", "features"},
		{"empty features", `{"image": "debian", "features": {}}`, "debian", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := devcontainer.Parse([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if got.Image != c.image || got.Unsupported != c.skip {
				t.Fatalf("got %+v, want image %q unsupported %q", got, c.image, c.skip)
			}
		})
	}
	if _, err := devcontainer.Parse([]byte(`{"image": `)); err == nil {
		t.Error("a truncated file parsed")
	}
}
