package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// EditorConfig runs code-server, on first use, for the gateway to proxy.
type EditorConfig struct {
	// Binary is code-server's launcher.
	Binary string
	// Extensions holds extensions to install, one directory each (the
	// activity extension); they are linked into the editor's own
	// extension directory under the workspace's .sbx.
	Extensions string
	// Addr is where code-server listens, on loopback (default 127.0.0.1:13337).
	Addr string
}

// The routes the gateway proxies through, behind the agent token.
const (
	editorPrefix = "/sbx/editor"
	portPrefix   = "/sbx/port/"
)

// editor starts code-server once and keeps it running.
type editor struct {
	cfg      EditorConfig
	root     string
	activity string // the activity endpoint, for the extension
	services *Services

	mu      sync.Mutex
	started bool
}

func (e *editor) addr() string {
	if e.cfg.Addr == "" {
		return "127.0.0.1:13337"
	}
	return e.cfg.Addr
}

// ensure starts code-server if it isn't running and waits until it
// accepts connections.
func (e *editor) ensure(ctx context.Context) error {
	e.mu.Lock()
	if !e.started {
		if err := e.start(); err != nil {
			e.mu.Unlock()
			return err
		}
		e.started = true
	}
	e.mu.Unlock()
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", e.addr())
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the editor didn't start: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *editor) start() error {
	data := filepath.Join(e.root, ".sbx", "editor")
	exts := filepath.Join(data, "extensions")
	if err := os.MkdirAll(exts, 0o755); err != nil {
		return err
	}
	if e.cfg.Extensions != "" {
		bundled, _ := os.ReadDir(e.cfg.Extensions)
		for _, d := range bundled {
			link := filepath.Join(exts, d.Name())
			if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
				if err := os.Symlink(filepath.Join(e.cfg.Extensions, d.Name()), link); err != nil {
					return err
				}
			}
		}
	}
	_, err := e.services.Start(&agentv1.Service{
		Name: "editor",
		Command: []string{e.cfg.Binary,
			"--bind-addr", e.addr(), "--auth", "none",
			"--disable-telemetry", "--disable-update-check", "--disable-workspace-trust",
			"--user-data-dir", filepath.Join(data, "data"), "--extensions-dir", exts,
			e.root},
		Env: map[string]string{"SBX_ACTIVITY_URL": e.activity},
	})
	return err
}

// authorized checks the agent token on our own REST routes.
func authorized(want []byte, r *http.Request) bool {
	if want == nil {
		return true // tests
	}
	tok, ok := token.FromHeader(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// proxyRoutes serves the editor and app ports to the gateway. Requests
// carry the agent token; while frozen they are refused, since every
// process behind them is stopped.
func (s *Server) proxyRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		editorRoute := r.URL.Path == editorPrefix || strings.HasPrefix(r.URL.Path, editorPrefix+"/")
		if !editorRoute && !strings.HasPrefix(r.URL.Path, portPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if !authorized(s.cfg.TokenSHA256, r) {
			http.Error(w, "invalid agent token", http.StatusUnauthorized)
			return
		}
		if s.freezer.Frozen() {
			w.Header().Set(FrozenHeader, "1")
			http.Error(w, "sbx: workspace is frozen for a snapshot", http.StatusServiceUnavailable)
			return
		}
		if editorRoute {
			s.proxyEditor(w, r)
			return
		}
		s.proxyPort(w, r)
	})
}

func (s *Server) proxyEditor(w http.ResponseWriter, r *http.Request) {
	if s.editor == nil {
		http.Error(w, "this workspace has no editor", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.editor.ensure(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: s.editor.addr()}
	proxy(target, editorPrefix, true).ServeHTTP(w, r)
}

func (s *Server) proxyPort(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, portPrefix)
	p, _, _ := strings.Cut(rest, "/")
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", p)}
	proxy(target, portPrefix+p, false).ServeHTTP(w, r)
}

// proxy forwards to target with prefix stripped from the path. WebSocket
// upgrades pass through. The editor is shown the public host the gateway
// saw, which code-server checks against Origin; apps see their own
// address, which dev servers that guard Host accept.
func proxy(target *url.URL, prefix string, publicHost bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			path := strings.TrimPrefix(pr.In.URL.Path, prefix)
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Header.Del("Authorization")
			if host := pr.In.Header.Get("X-Forwarded-Host"); publicHost && host != "" {
				pr.Out.Host = host
			}
		},
		Transport: &http.Transport{
			// A dev server may listen on IPv6 loopback only (Node resolves
			// localhost to ::1 first).
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				conn, err := d.DialContext(ctx, network, addr)
				if err != nil && strings.HasPrefix(addr, "127.0.0.1:") {
					_, port, _ := net.SplitHostPort(addr)
					return d.DialContext(ctx, network, net.JoinHostPort("::1", port))
				}
				return conn, err
			},
			ResponseHeaderTimeout: 5 * time.Minute,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "nothing is answering in the workspace: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// --- the editor extension's activity reports ---

// terminalExpiry drops a terminal command whose extension stopped
// reporting it (the editor went away mid-command).
const terminalExpiry = 2 * time.Minute

type terminals struct {
	activity *Activity
	now      func() time.Time

	mu sync.Mutex
	m  map[string]*terminal
}

type terminal struct {
	done func()
	seen time.Time
}

func newTerminals(a *Activity) *terminals {
	return &terminals{activity: a, now: time.Now, m: map[string]*terminal{}}
}

func (t *terminals) start(id, command string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if x, ok := t.m[id]; ok {
		x.seen = t.now()
		return
	}
	t.m[id] = &terminal{done: t.activity.StartCommand(command, signalsv1.CommandKind_COMMAND_KIND_TERMINAL), seen: t.now()}
}

func (t *terminals) end(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if x, ok := t.m[id]; ok {
		x.done()
		delete(t.m, id)
	}
	t.activity.Touch()
}

func (t *terminals) alive(ids []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if x, ok := t.m[id]; ok {
			x.seen = t.now()
		}
	}
}

func (t *terminals) prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, x := range t.m {
		if t.now().Sub(x.seen) > terminalExpiry {
			x.done()
			delete(t.m, id)
		}
	}
}

// activityReport is what the editor extension posts.
type activityReport struct {
	Kind    string   `json:"kind"` // edit, save, terminal-start, terminal-end, terminal-running
	ID      string   `json:"id,omitempty"`
	Command string   `json:"command,omitempty"`
	IDs     []string `json:"ids,omitempty"`
}

// activityHandler serves POST /v1/activity on loopback. It takes no token:
// anything in the workload can reach it, but all a report can do is make
// the workspace look busier. Only real input is reported: the extension
// sends edits, saves and terminal commands, never heartbeats.
func (s *Server) activityHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/activity", func(w http.ResponseWriter, r *http.Request) {
		var rep activityReport
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch rep.Kind {
		case "edit", "save":
			s.activity.Touch()
		case "terminal-start":
			if rep.ID == "" {
				http.Error(w, "id is required", http.StatusBadRequest)
				return
			}
			s.terms.start(rep.ID, rep.Command)
		case "terminal-end":
			s.terms.end(rep.ID)
		case "terminal-running":
			s.terms.alive(rep.IDs)
		default:
			http.Error(w, "unknown kind "+rep.Kind, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
