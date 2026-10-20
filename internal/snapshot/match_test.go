package snapshot

import "testing"

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"node_modules", "node_modules/x/index.js", true},
		{"node_modules", "web/node_modules/react/index.js", true},
		{"node_modules", "my_node_modules.txt", false},
		{".env", ".env", true},
		{".env", "services/api/.env", true},
		{".env.*", "services/.env.local", true},
		{".env.*", ".envrc", false},
		{"*.pem", "certs/server.pem", true},
		{".ssh", ".ssh/id_rsa", true},
		{".docker/config.json", ".docker/config.json", true},
		{".docker/config.json", "app/.docker/config.json", false},
		{"build/**/*.o", "build/a/b/c.o", true},
		{"build/**/*.o", "build/c.o", true},
		{"build/**/*.o", "src/c.o", false},
		{"docs/", "docs/a.md", true},
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
