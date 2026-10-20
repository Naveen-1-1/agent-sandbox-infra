package leaktest_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// verifyCall matches the TestMain wiring every test package must have.
var verifyCall = regexp.MustCompile(`\b(leaktest|goleak)\.VerifyTestMain\(`)

// skipDirs are never Go test packages of ours.
var skipDirs = map[string]bool{
	".git": true, ".cache": true, "bin": true, "node_modules": true,
	"charts": true, "web": true, "extensions": true, "notes": true,
}

// TestEveryTestPackageChecksLeaks enforces "goleak everywhere": every
// directory with _test.go files must call VerifyTestMain in one of them.
func TestEveryTestPackageChecksLeaks(t *testing.T) {
	root := repoRoot(t)
	testFiles := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			dir := filepath.Dir(path)
			testFiles[dir] = append(testFiles[dir], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var missing []string
	for dir, files := range testFiles {
		if !anyFileMatches(t, files) {
			rel, _ := filepath.Rel(root, dir)
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("test packages without leaktest.VerifyTestMain in TestMain:\n  %s", strings.Join(missing, "\n  "))
	}
}

func anyFileMatches(t *testing.T, files []string) bool {
	t.Helper()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if verifyCall.Match(b) {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}
