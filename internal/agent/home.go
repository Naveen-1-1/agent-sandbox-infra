package agent

import (
	"os"
	"strings"
	"syscall"
)

// fallbackHome is HOME for images that have no writable home for the
// workspace user. It lives in the container, so it doesn't survive a sleep.
const fallbackHome = "/tmp/sbx-home"

// withUsableHome makes sure env's HOME is a writable directory. Workspaces
// run every image as one fixed user, and an image without that user in
// /etc/passwd gets HOME=/ — where package managers can't keep their caches
// (uv sync fails outright). Such a HOME is replaced by fallback.
func withUsableHome(env []string, fallback string) []string {
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	if home != "" && home != "/" && writable(home) {
		return env
	}
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HOME="+fallback)
}

func writable(dir string) bool {
	const wOK = 2
	return syscall.Access(dir, wOK) == nil
}
