package snapshot_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
	gf "github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/gitfixture"
)

// Fake credentials are assembled at runtime so no secret-looking literal sits
// in the source tree.
var (
	fakeAWSKey     = "AKIA" + strings.Repeat("Q", 16)
	fakePrivateKey = "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END RSA PRIVATE KEY-----\n"
	fakeGitHub     = "gh" + "p_" + strings.Repeat("a1B2", 9)
)

// smallWorkspace clones a one-file upstream repo (plus any upstream files).
func smallWorkspace(t *testing.T, upstream map[string]string) (origin, ws, base string) {
	t.Helper()
	files := map[string]string{"README.md": "hello\n"}
	for k, v := range upstream {
		files[k] = v
	}
	origin = gf.NewRepo(t, files)
	base = gf.Git(t, origin, "rev-parse", "HEAD")
	ws = filepath.Join(t.TempDir(), "ws")
	gf.Git(t, filepath.Dir(ws), "clone", "-q", origin, ws)
	return origin, ws, base
}

func create(t *testing.T, ws, base string, o snapshot.Options) (*snapshot.Result, error) {
	t.Helper()
	o.Base, o.Time, o.WorkDir = base, freeze, t.TempDir()
	return snapshot.Create(context.Background(), ws, o)
}

func wantPolicy(t *testing.T, err error, code snapshot.Code, pathFragment string) {
	t.Helper()
	var pe *snapshot.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PolicyError %s", err, code)
	}
	if pe.Code != code {
		t.Fatalf("code = %s, want %s (%v)", pe.Code, code, pe)
	}
	if pathFragment != "" && !strings.Contains(strings.Join(pe.Paths, "\n"), pathFragment) {
		t.Fatalf("paths %q do not mention %q", pe.Paths, pathFragment)
	}
}

func TestSecretInNewFileFailsWithoutLeakingIt(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "config.py"), "AWS_KEY = '"+fakeAWSKey+"'\n")
	_, err := create(t, ws, base, snapshot.Options{})
	wantPolicy(t, err, snapshot.CodeSecretDetected, "config.py (AWS access key)")
	if strings.Contains(err.Error(), fakeAWSKey) {
		t.Fatal("error message contains the secret value")
	}
}

func TestPrivateKeyInStagedFileFails(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "deploy/key.txt"), fakePrivateKey)
	gf.Git(t, ws, "add", "deploy/key.txt")
	_, err := create(t, ws, base, snapshot.Options{})
	wantPolicy(t, err, snapshot.CodeSecretDetected, "deploy/key.txt (private key)")
}

func TestTokenInAgentCommitFails(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "ci.yaml"), "token: "+fakeGitHub+"\n")
	gf.Git(t, ws, "add", "ci.yaml")
	gf.Git(t, ws, "commit", "-q", "-m", "add ci")
	gf.Git(t, ws, "rm", "-q", "ci.yaml") // deleting it now doesn't remove it from history
	_, err := create(t, ws, base, snapshot.Options{})
	wantPolicy(t, err, snapshot.CodeSecretDetected, "ci.yaml (GitHub token)")
}

func TestSecretFileInAgentCommitFails(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Git(t, ws, "checkout", "-q", "-b", "oops")
	gf.Write(t, filepath.Join(ws, ".env"), "PASSWORD=hunter2\n")
	gf.Git(t, ws, "add", "-f", ".env")
	gf.Git(t, ws, "commit", "-q", "-m", "commit env")
	_, err := create(t, ws, base, snapshot.Options{})
	wantPolicy(t, err, snapshot.CodeSecretDetected, ".env (secret file in a commit)")
}

func TestStagedSecretFileIsDroppedNotSaved(t *testing.T) {
	origin, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, ".env"), "PASSWORD=hunter2\n")
	gf.Write(t, filepath.Join(ws, ".env.example"), "PASSWORD=\n")
	gf.Write(t, filepath.Join(ws, "id_ed25519.pub"), "ssh-ed25519 AAAA public\n")
	gf.Git(t, ws, "add", ".env", ".env.example", "id_ed25519.pub")
	res, err := create(t, ws, base, snapshot.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file://" + origin, Base: base, Snapshot: res.BundlePath,
	}); err != nil {
		t.Fatal(err)
	}
	staged := gf.Git(t, dst, "ls-files")
	if strings.Contains(staged, ".env\n") || strings.HasSuffix(staged, ".env") {
		t.Errorf(".env was saved in the staging area:\n%s", staged)
	}
	for _, keep := range []string{".env.example", "id_ed25519.pub"} {
		if !strings.Contains(staged, keep) {
			t.Errorf("%s was dropped; it is a safe exception:\n%s", keep, staged)
		}
	}
	if st := gf.State(t, dst); st["file:.env"] != "" {
		t.Errorf(".env was restored into the working tree")
	}
}

func TestAllowListSkipsContentScan(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "testdata/fixture.txt"), fakeAWSKey+"\n")
	sp := snapshot.DefaultSecretPolicy()
	sp.Allow = []string{"testdata"}
	if _, err := create(t, ws, base, snapshot.Options{Secrets: &sp}); err != nil {
		t.Fatalf("allowlisted fixture was refused: %v", err)
	}
}

func TestSecretAlreadyInBaseIsNotReported(t *testing.T) {
	_, ws, base := smallWorkspace(t, map[string]string{"upstream.cfg": "key=" + fakeAWSKey + "\n"})
	gf.Write(t, filepath.Join(ws, "notes.md"), "agent notes\n")
	if _, err := create(t, ws, base, snapshot.Options{}); err != nil {
		t.Fatalf("a secret that upstream already has blocked the snapshot: %v", err)
	}
}

func TestBinaryFilesAreNotScanned(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "blob.bin"), "\x00\x01binary"+fakeAWSKey)
	if _, err := create(t, ws, base, snapshot.Options{}); err != nil {
		t.Fatalf("binary file was content-scanned: %v", err)
	}
}

func TestFileOverLimit(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "big.log.txt"), strings.Repeat("x", 2000))
	_, err := create(t, ws, base, snapshot.Options{Limits: snapshot.Limits{MaxFileBytes: 1000}})
	wantPolicy(t, err, snapshot.CodeTooLarge, "big.log.txt (2000 bytes)")
}

func TestNewContentOverLimit(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	for _, n := range []string{"a", "b", "c"} {
		gf.Write(t, filepath.Join(ws, n+".txt"), strings.Repeat(n, 600))
	}
	_, err := create(t, ws, base, snapshot.Options{Limits: snapshot.Limits{MaxFileBytes: 1000, MaxNewBytes: 1500}})
	wantPolicy(t, err, snapshot.CodeTooLarge, "")
}

func TestBundleOverLimit(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	gf.Write(t, filepath.Join(ws, "random.txt"), randomHex(t, 4096))
	_, err := create(t, ws, base, snapshot.Options{Limits: snapshot.Limits{MaxBundleBytes: 512}})
	wantPolicy(t, err, snapshot.CodeTooLarge, "")
}

func TestRestoreNeverStoresCredentials(t *testing.T) {
	_, ws, base := smallWorkspace(t, nil)
	res := snapshotOf(t, ws, base)
	bb, _, _, err := snapshot.CreateBaseBundle(context.Background(), ws, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	withCreds := "https://bot:s3cr3t@example.invalid/org/repo.git"

	dst := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: withCreds, Base: base, BaseBundle: bb, Snapshot: res.BundlePath,
	}); err != nil {
		t.Fatal(err)
	}
	if got := gf.Git(t, dst, "remote", "get-url", "origin"); got != "https://example.invalid/org/repo.git" {
		t.Errorf("origin = %q, want it without credentials", got)
	}

	// A failing fetch must not echo the credentials in its error.
	err = snapshot.Restore(context.Background(), filepath.Join(t.TempDir(), "x"), snapshot.RestoreOptions{
		OriginURL: "http://bot:s3cr3t@127.0.0.1:1/repo.git", Base: base,
	})
	if err == nil {
		t.Fatal("fetch from a closed port succeeded")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"fetch https://u:p@host/x failed":   "fetch https://host/x failed",
		"http://token@host and git://a@b/c": "http://host and git://b/c",
		"https://host/no/creds@in/path":     "https://host/no/creds@in/path",
	} {
		if got := snapshot.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
