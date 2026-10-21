package agent

import (
	"os"
	"slices"
	"testing"
)

func TestWithUsableHome(t *testing.T) {
	ok := t.TempDir()
	env := []string{"PATH=/bin", "HOME=" + ok}
	fallback := t.TempDir() + "/home"
	if got := withUsableHome(env, fallback); !slices.Equal(got, env) {
		t.Errorf("a writable HOME was changed: %v", got)
	}
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	for _, bad := range [][]string{
		{"PATH=/bin", "HOME=/"},
		{"PATH=/bin", "HOME=" + readOnly},
		{"PATH=/bin"},
	} {
		if os.Getuid() == 0 && bad[len(bad)-1] == "HOME="+readOnly {
			continue // root can write anywhere
		}
		got := withUsableHome(bad, fallback)
		if !slices.Equal(got, []string{"PATH=/bin", "HOME=" + fallback}) {
			t.Errorf("withUsableHome(%v) = %v", bad, got)
		}
	}
}
