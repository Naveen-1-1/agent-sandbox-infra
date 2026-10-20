package snapshot

import (
	"path"
	"strings"
)

// matchAny reports whether p (a slash-separated path relative to the
// repository root) matches any pattern, with the same meaning as Exclude:
// a pattern without a slash matches a file or directory with that name
// anywhere; a pattern with a slash is a glob from the root where "**" spans
// directories. A match on a directory covers everything under it.
func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if matchPath(pat, p) {
			return true
		}
	}
	return false
}

func matchPath(pattern, p string) bool {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
	if pattern == "" {
		return false
	}
	segs := strings.Split(p, "/")
	if !strings.Contains(pattern, "/") {
		for _, s := range segs {
			if ok, _ := path.Match(pattern, s); ok {
				return true
			}
		}
		return false
	}
	pat := strings.Split(pattern, "/")
	// A directory pattern also matches everything below it.
	for i := len(segs); i > 0; i-- {
		if matchSegs(pat, segs[:i]) {
			return true
		}
	}
	return false
}

// matchSegs matches path segments against pattern segments, where "**"
// matches zero or more segments.
func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
