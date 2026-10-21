package workspace_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestInspectorFallsBackForDumbHTTP serves a repository the way the
// fixtures server does (static files, no smart protocol): the shallow,
// filtered fetch fails, and the fallback must not leave git fetching
// objects one by one.
func TestInspectorFallsBackForDumbHTTP(t *testing.T) {
	repo := gf.NewRepo(t, map[string]string{".devcontainer/devcontainer.json": `{"image": "node:22"}`, "a.txt": "a"})
	bare := filepath.Join(t.TempDir(), "r.git")
	gf.Git(t, filepath.Dir(bare), "clone", "-q", "--bare", repo, bare)
	gf.Git(t, bare, "update-server-info")
	srv := httptest.NewServer(http.FileServer(http.Dir(bare)))
	defer srv.Close()

	g := workspace.GitInspector{AllowPrivateHosts: true, Timeout: 20 * time.Second}
	src := sbxv1.Source{RepoURL: srv.URL}
	base, err := g.Resolve(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	image, note := g.Devcontainer(context.Background(), src, base)
	if image != "node:22" {
		t.Fatalf("got %q (%q)", image, note)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
}
