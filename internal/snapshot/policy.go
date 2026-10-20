package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Code says why a snapshot was refused.
type Code string

const (
	// CodeSecretDetected means a new file or commit looks like it holds a secret.
	CodeSecretDetected Code = "SecretDetected"
	// CodeTooLarge means a file, the new content or the bundle is over a limit.
	CodeTooLarge Code = "TooLarge"
)

// PolicyError means a safety rule refused the snapshot. The workspace keeps
// running; Message and Paths say what to fix. It never contains secret values.
type PolicyError struct {
	Code    Code
	Message string
	Paths   []string
}

func (e *PolicyError) Error() string {
	if len(e.Paths) == 0 {
		return fmt.Sprintf("snapshot refused (%s): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("snapshot refused (%s): %s: %s", e.Code, e.Message, strings.Join(e.Paths, ", "))
}

// Limits caps what one snapshot may contain. Zero means no limit.
type Limits struct {
	// MaxFileBytes is the largest single new file.
	MaxFileBytes int64
	// MaxNewBytes caps the total size of all new files.
	MaxNewBytes int64
	// MaxBundleBytes caps the compressed bundle.
	MaxBundleBytes int64
}

// SecretPolicy keeps secrets out of snapshots.
type SecretPolicy struct {
	// Paths are never saved from the working tree or staging area. If one
	// appears in a new commit, the snapshot fails, since history can't be
	// rewritten behind the user's back.
	Paths []string
	// Exceptions are Paths matches that are fine to save (e.g. .env.example).
	Exceptions []string
	// Allow lists paths whose content is not scanned, for false positives.
	Allow []string
	// DisableContentScan turns off content rules (path rules still apply).
	DisableContentScan bool
}

// DefaultSecretPaths are files that commonly hold credentials.
var DefaultSecretPaths = []string{
	".env", ".env.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	"id_rsa", "id_rsa.*", "id_dsa", "id_ecdsa", "id_ecdsa.*", "id_ed25519", "id_ed25519.*",
	".npmrc", ".pypirc", ".netrc", ".git-credentials",
	".aws", ".ssh", ".kube", ".docker/config.json", "kubeconfig", "*.kubeconfig",
}

// DefaultSecretExceptions are templates and public keys, safe to save.
var DefaultSecretExceptions = []string{
	".env.example", ".env.sample", ".env.template", ".env.dist", "*.pub",
}

// DefaultSecretPolicy is the policy used when none is configured.
func DefaultSecretPolicy() SecretPolicy {
	return SecretPolicy{Paths: DefaultSecretPaths, Exceptions: DefaultSecretExceptions}
}

// contentRule is a high-confidence secret pattern. Rules aim for near-zero
// false positives, since a hit blocks the snapshot.
type contentRule struct {
	name string
	re   *regexp.Regexp
}

var contentRules = []contentRule{
	{"private key", regexp.MustCompile(`-----BEGIN ((RSA|DSA|EC|OPENSSH|PGP|ENCRYPTED) )?PRIVATE KEY( BLOCK)?-----`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"Stripe live key", regexp.MustCompile(`\b(sk|rk)_live_[0-9A-Za-z]{24,}\b`)},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`)},
	{"OpenAI API key", regexp.MustCompile(`\bsk-(proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}\b`)},
}

// maxScanBytes bounds how much of one file is scanned for secrets.
const maxScanBytes = 8 << 20

// isSecretPath reports whether p must not be saved.
func (sp SecretPolicy) isSecretPath(p string) bool {
	return matchAny(sp.Paths, p) && !matchAny(sp.Exceptions, p)
}

// newObject is a git object the bundle would carry.
type newObject struct {
	sha  string
	typ  string
	size int64
	path string
}

// listNewObjects lists every object reachable from the bundle's refs but not
// from base: exactly what the bundle will contain.
func listNewObjects(ctx context.Context, g git, base string) ([]newObject, error) {
	args := []string{"rev-list", "--objects", "--branches", "--tags", wipRef}
	if base != "" {
		args = append(args, "--not", base)
	}
	revs, err := g.run(ctx, args...)
	if err != nil || revs == "" {
		return nil, err
	}
	out, err := g.runInput(ctx, []byte(revs+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize) %(rest)")
	if err != nil {
		return nil, err
	}
	var objs []newObject
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, " ", 4)
		if len(f) < 3 {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		o := newObject{sha: f[0], typ: f[1], size: size}
		if len(f) == 4 {
			o.path = f[3]
		}
		objs = append(objs, o)
	}
	return objs, nil
}

// checkPolicy enforces size limits and secret rules on the new objects, and
// returns the total size of new files.
func checkPolicy(ctx context.Context, g git, objs []newObject, sp SecretPolicy, lim Limits) (int64, error) {
	var total int64
	var tooBig, secretPaths []string
	var scan []newObject
	for _, o := range objs {
		if o.typ != "blob" {
			continue
		}
		total += o.size
		if lim.MaxFileBytes > 0 && o.size > lim.MaxFileBytes {
			tooBig = append(tooBig, fmt.Sprintf("%s (%d bytes)", o.path, o.size))
		}
		if o.path != "" && sp.isSecretPath(o.path) {
			secretPaths = append(secretPaths, o.path+" (secret file in a commit)")
		}
		if !sp.DisableContentScan && o.size <= maxScanBytes && !matchAny(sp.Allow, o.path) {
			scan = append(scan, o)
		}
	}
	if len(tooBig) > 0 {
		return total, &PolicyError{Code: CodeTooLarge, Message: fmt.Sprintf("files over the %d-byte limit; add them to .gitignore", lim.MaxFileBytes), Paths: sortedUnique(tooBig)}
	}
	if lim.MaxNewBytes > 0 && total > lim.MaxNewBytes {
		return total, &PolicyError{Code: CodeTooLarge, Message: fmt.Sprintf("new content is %d bytes, over the %d-byte limit", total, lim.MaxNewBytes)}
	}

	hits, err := scanContent(ctx, g, scan)
	if err != nil {
		return total, err
	}
	secretPaths = append(secretPaths, hits...)
	if len(secretPaths) > 0 {
		return total, &PolicyError{Code: CodeSecretDetected, Message: "possible secrets; remove them or move them out of the repository", Paths: sortedUnique(secretPaths)}
	}
	return total, nil
}

// scanContent streams blobs through `git cat-file --batch` and returns
// "path (rule)" for every blob matching a content rule. Binary files are
// skipped.
func scanContent(ctx context.Context, g git, blobs []newObject) ([]string, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	byPath := make(map[string]string, len(blobs))
	var stdin bytes.Buffer
	for _, b := range blobs {
		byPath[b.sha] = b.path
		stdin.WriteString(b.sha + "\n")
	}

	cmd := exec.CommandContext(ctx, "git", "-c", "safe.directory=*", "cat-file", "--batch")
	cmd.Dir = g.dir
	cmd.Env = append(baseEnv(), g.env...)
	cmd.Stdin = &stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var hits []string
	r := bufio.NewReaderSize(stdout, 64<<10)
	readErr := func() error {
		for {
			header, err := r.ReadString('\n')
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			f := strings.Fields(header)
			if len(f) != 3 {
				return fmt.Errorf("snapshot: unexpected cat-file header %q", header)
			}
			size, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil {
				return err
			}
			data := make([]byte, size+1) // content plus trailing newline
			if _, err := io.ReadFull(r, data); err != nil {
				return err
			}
			if rule := matchRule(data[:size]); rule != "" {
				hits = append(hits, byPath[f[0]]+" ("+rule+")")
			}
		}
	}()
	if readErr != nil {
		// Stop git before waiting, or it may block forever on a full pipe.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, readErr
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return hits, nil
}

// matchRule returns the name of the first rule data matches, or "" for no
// match or binary data.
func matchRule(data []byte) string {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return ""
	}
	for _, r := range contentRules {
		if r.re.Match(data) {
			return r.name
		}
	}
	return ""
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
