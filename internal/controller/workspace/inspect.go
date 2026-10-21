package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/devcontainer"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

// GitInspector resolves sources with `git ls-remote`.
type GitInspector struct {
	// AllowPrivateHosts permits repositories on private, loopback or
	// link-local addresses (in-cluster fixtures). Off by default so a
	// workspace can't make the controller probe internal services.
	AllowPrivateHosts bool
	Timeout           time.Duration
}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Resolve returns the commit and branch a workspace starts from: the
// remote's default branch, or spec.source.ref (a branch, tag or commit).
func (g GitInspector) Resolve(ctx context.Context, src sbxv1.Source) (Base, error) {
	if err := g.checkHost(ctx, src.RepoURL); err != nil {
		return Base{}, err
	}
	if hexSHA.MatchString(src.Ref) {
		return Base{SHA: src.Ref, Branch: "main"}, nil
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if src.Ref == "" {
		out, err := lsRemote(ctx, "--symref", src.RepoURL, "HEAD")
		if err != nil {
			return Base{}, err
		}
		var base Base
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				base.Branch, _, _ = strings.Cut(rest, "\t")
			} else if sha, ref, ok := strings.Cut(line, "\t"); ok && ref == "HEAD" {
				base.SHA = sha
			}
		}
		if base.SHA == "" {
			return Base{}, PermanentError{errors.New("repository has no default branch")}
		}
		if base.Branch == "" {
			base.Branch = "main"
		}
		return base, nil
	}

	out, err := lsRemote(ctx, src.RepoURL, "refs/heads/"+src.Ref, "refs/tags/"+src.Ref, "refs/tags/"+src.Ref+"^{}")
	if err != nil {
		return Base{}, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok {
			refs[ref] = sha
		}
	}
	switch {
	case refs["refs/heads/"+src.Ref] != "":
		return Base{SHA: refs["refs/heads/"+src.Ref], Branch: src.Ref}, nil
	case refs["refs/tags/"+src.Ref+"^{}"] != "": // annotated tag: use the commit
		return Base{SHA: refs["refs/tags/"+src.Ref+"^{}"], Branch: "main"}, nil
	case refs["refs/tags/"+src.Ref] != "":
		return Base{SHA: refs["refs/tags/"+src.Ref], Branch: "main"}, nil
	}
	return Base{}, PermanentError{fmt.Errorf("ref %q not found in %s", src.Ref, snapshot.StripCredentials(src.RepoURL))}
}

// Devcontainer reads the image from devcontainer.json at the base commit.
// A missing file means the default image; a file that needs a build, or
// that can't be read, also falls back to it, and note says why.
func (g GitInspector) Devcontainer(ctx context.Context, src sbxv1.Source, base Base) (image, note string) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// One commit's trees, without file contents, is all that's needed. A
	// dumb HTTP server can't do that, so fall back to fetching the ref, in a
	// fresh repository: the failed attempt leaves partial-clone settings
	// behind that would send every lookup back to the server.
	ref := src.Ref
	if ref == "" {
		ref = "HEAD"
	}
	dir, err := fetchInto(ctx, "--depth", "1", "--filter=blob:none", src.RepoURL, base.SHA)
	if err != nil {
		if dir, err = fetchInto(ctx, src.RepoURL, ref); err != nil {
			return "", "couldn't read devcontainer.json: " + err.Error()
		}
	}
	defer os.RemoveAll(dir)
	for _, path := range devcontainer.Paths {
		obj := base.SHA + ":" + path
		size, err := runGit(ctx, dir, "cat-file", "-s", obj)
		if err != nil {
			continue // not there
		}
		if n, _ := strconv.Atoi(strings.TrimSpace(size)); n > devcontainer.MaxBytes {
			return "", path + " is too large; using the default image"
		}
		data, err := runGit(ctx, dir, "cat-file", "blob", obj)
		if err != nil {
			return "", "couldn't read " + path + ": " + err.Error()
		}
		c, err := devcontainer.Parse([]byte(data))
		switch {
		case err != nil:
			return "", err.Error() + "; using the default image"
		case c.Unsupported != "":
			return "", fmt.Sprintf("%s uses %s, which needs an image build; using the default image", path, c.Unsupported)
		case c.Image == "":
			return "", path + " names no image; using the default image"
		}
		return c.Image, "image from " + path
	}
	return "", ""
}

// fetchInto fetches into a new bare repository and returns its directory.
func fetchInto(ctx context.Context, fetch ...string) (string, error) {
	dir, err := os.MkdirTemp("", "sbx-inspect-*")
	if err != nil {
		return "", err
	}
	if _, err = runGit(ctx, dir, "init", "-q", "--bare"); err == nil {
		_, err = runGit(ctx, dir, append([]string{"fetch", "-q"}, fetch...)...)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never fetch a missing object behind our back.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_DIR="+dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, snapshot.Redact(strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

func lsRemote(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"ls-remote"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := snapshot.Redact(strings.TrimSpace(stderr.String()))
		if strings.Contains(msg, "not found") || strings.Contains(msg, "Repository not found") {
			return "", PermanentError{fmt.Errorf("git ls-remote: %s", msg)}
		}
		return "", fmt.Errorf("git ls-remote: %w: %s", err, msg)
	}
	return stdout.String(), nil
}

func (g GitInspector) checkHost(ctx context.Context, raw string) error {
	if g.AllowPrivateHosts {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return PermanentError{err}
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("resolving %s: %w", u.Hostname(), err)
	}
	for _, ip := range ips {
		if ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() {
			return PermanentError{fmt.Errorf("repository host %s resolves to a private address", u.Hostname())}
		}
	}
	return nil
}
