package workspace_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/workspace"
	gf "github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/gitfixture"
)

func TestInspectorReadsTheDevcontainerImage(t *testing.T) {
	g := workspace.GitInspector{AllowPrivateHosts: true}
	for _, c := range []struct {
		name  string
		files map[string]string
		image string
		note  string
	}{
		{"devcontainer folder", map[string]string{".devcontainer/devcontainer.json": "{\n  // node\n  \"image\": \"node:22\",\n}\n"}, "node:22", "image from .devcontainer/devcontainer.json"},
		{"root file", map[string]string{".devcontainer.json": `{"image": "python:3.13"}`}, "python:3.13", "image from .devcontainer.json"},
		{"needs a build", map[string]string{".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile"}}`}, "", "needs an image build"},
		{"none", map[string]string{"README.md": "hi"}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo := gf.NewRepo(t, c.files)
			src := sbxv1.Source{RepoURL: "file://" + repo}
			base, err := g.Resolve(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			image, note := g.Devcontainer(context.Background(), src, base)
			if image != c.image || !strings.Contains(note, c.note) || (c.note == "" && note != "") {
				t.Fatalf("got %q (%q), want %q (%q)", image, note, c.image, c.note)
			}
		})
	}
}

func TestInspectorReportsAnUnreadableRepository(t *testing.T) {
	g := workspace.GitInspector{AllowPrivateHosts: true}
	src := sbxv1.Source{RepoURL: "file://" + filepath.Join(t.TempDir(), "missing")}
	image, note := g.Devcontainer(context.Background(), src, workspace.Base{SHA: strings.Repeat("a", 40)})
	if image != "" || !strings.Contains(note, "couldn't read devcontainer.json") {
		t.Fatalf("got %q (%q)", image, note)
	}
}
