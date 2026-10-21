package gateway

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Browsers reach a workspace on hosts of their own, so each gets its own
// cookies and origin:
//
//	<name>.<namespace>.<domain>         the editor
//	<port>.<name>.<namespace>.<domain>  an app port inside the workspace
//
// A login link carries a short-lived ticket; /_sbx/login swaps it for a
// cookie bound to that one host. Scripts can send a bearer token instead.

const (
	sessionCookie = "sbx_session"
	loginPath     = "/_sbx/login"
	ticketTTL     = 5 * time.Minute
)

// webTarget is a workspace host.
type webTarget struct {
	ns, name string
	port     int // 0: the editor
}

// parseHost reports which workspace host, if any, host names.
func (s *Server) parseHost(host string) (webTarget, bool) {
	if s.cfg.Domain == "" {
		return webTarget{}, false
	}
	h := hostname(host)
	rest, ok := strings.CutSuffix(h, "."+s.cfg.Domain)
	if !ok {
		return webTarget{}, false
	}
	labels := strings.Split(rest, ".")
	switch len(labels) {
	case 2:
		return webTarget{name: labels[0], ns: labels[1]}, labels[0] != "" && labels[1] != ""
	case 3:
		port, err := strconv.Atoi(labels[0])
		if err != nil || port < 1 || port > 65535 {
			return webTarget{}, false
		}
		return webTarget{port: port, name: labels[1], ns: labels[2]}, labels[1] != "" && labels[2] != ""
	}
	return webTarget{}, false
}

// webHost is the public host (with port, if not the scheme's default) of t.
func (s *Server) webHost(t webTarget) string {
	h := t.name + "." + t.ns + "." + s.cfg.Domain
	if t.port != 0 {
		h = strconv.Itoa(t.port) + "." + h
	}
	if s.cfg.PublicPort != 0 {
		h = net.JoinHostPort(h, strconv.Itoa(s.cfg.PublicPort))
	}
	return h
}

func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(host)
}

// LoginURL returns a link that signs a browser in to ws's editor (port 0)
// or app port, valid for a few minutes.
func (s *Server) LoginURL(ws *sbxv1.Workspace, port int) (string, error) {
	if s.cfg.Domain == "" {
		return "", errors.New("the gateway has no domain for editors (gateway.domain)")
	}
	t := webTarget{ns: ws.Namespace, name: ws.Name, port: port}
	host := s.webHost(t)
	ticket := s.cfg.Keys.Sign(token.Ticket, string(ws.UID), hostname(host), time.Now().Add(ticketTTL))
	return fmt.Sprintf("%s://%s%s?ticket=%s", s.cfg.PublicScheme, host, loginPath, url.QueryEscape(ticket)), nil
}

// Handler serves everything: workspace hosts when a domain is set, and the
// REST API on any other host.
func (s *Server) Handler() http.Handler {
	api := s.APIHandler()
	if s.cfg.Domain == "" {
		return api
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := s.parseHost(r.Host); ok {
			s.serveWeb(w, r, t)
			return
		}
		api.ServeHTTP(w, r)
	})
}

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request, t webTarget) {
	route := "editor"
	if t.port != 0 {
		route = "port"
	}
	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	defer func() { s.metrics.Requests.WithLabelValues(route, strconv.Itoa(rec.code)).Inc() }()
	w = rec

	ws := &sbxv1.Workspace{}
	key := client.ObjectKey{Namespace: t.ns, Name: t.name}
	if err := s.cache.Get(r.Context(), key, ws); err != nil {
		page(w, http.StatusNotFound, "No such workspace", "There is no workspace "+t.ns+"/"+t.name+".")
		return
	}
	if r.URL.Path == loginPath {
		s.webLogin(w, r, ws)
		return
	}
	viaCookie, ok := s.webAuth(r, ws)
	if !ok {
		s.metrics.Rejected.WithLabelValues(metrics.RejectAuth).Inc()
		page(w, http.StatusUnauthorized, "Sign in", "Open this workspace from a fresh link: sbxctl editor "+t.ns+"/"+t.name)
		return
	}
	// A cookie rides along on any request to this host, including ones
	// another site makes the browser send. Upgrades and writes must come
	// from this host's own pages.
	if viaCookie && (isUpgrade(r) || !safeMethod(r.Method)) && r.Header.Get("Origin") != s.cfg.PublicScheme+"://"+r.Host {
		page(w, http.StatusForbidden, "Cross-origin request refused", "This request didn't come from the workspace's own page.")
		return
	}

	k := key.String()
	nav := isNavigation(r)
	if nav {
		s.signals.Touch(k)
	}
	if !Ready(ws) {
		// Only someone opening the page wakes a workspace. An editor tab
		// left open reconnecting, or an app polling, must not.
		if !nav {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("X-Sbx-Asleep", "1")
			page(w, http.StatusServiceUnavailable, "Asleep", "The workspace is asleep. Reload the page to wake it.")
			return
		}
		cur, _, err := s.awake(r.Context(), ws)
		if err != nil {
			s.wakeErr(w, err)
			return
		}
		ws = cur
	}
	if isUpgrade(r) {
		kind := signalsv1.StreamKind_STREAM_KIND_EDITOR
		if t.port != 0 {
			kind = signalsv1.StreamKind_STREAM_KIND_APP
		}
		defer s.signals.OpenStream(k, kind)()
	}
	s.webProxy(ws, t, nav).ServeHTTP(w, r)
}

// webAuth accepts a session cookie for this host, or a bearer token (the
// workspace's own, or the admin token). viaCookie says which.
func (s *Server) webAuth(r *http.Request, ws *sbxv1.Workspace) (viaCookie, ok bool) {
	if tok := bearer(r); tok != "" {
		return false, s.isAdmin(r) || s.cfg.Keys.Verify(tok, token.Gateway, string(ws.UID), 0) == nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false, false
	}
	return true, s.cfg.Keys.Check(c.Value, token.Session, string(ws.UID), hostname(r.Host), time.Now()) == nil
}

// webLogin swaps a ticket for a session cookie on this host and sends the
// browser on to the page.
func (s *Server) webLogin(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	host := hostname(r.Host)
	if s.cfg.Keys.Check(r.URL.Query().Get("ticket"), token.Ticket, string(ws.UID), host, time.Now()) != nil {
		s.metrics.Rejected.WithLabelValues(metrics.RejectAuth).Inc()
		page(w, http.StatusUnauthorized, "Link expired", "This sign-in link has expired or is for another workspace. Ask for a new one: sbxctl editor "+ws.Namespace+"/"+ws.Name)
		return
	}
	expires := time.Now().Add(s.cfg.SessionTTL)
	// Secure follows the public scheme: a plain-HTTP local cluster can't
	// use a Secure cookie at all.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name: sessionCookie, Value: s.cfg.Keys.Sign(token.Session, string(ws.UID), host, expires),
		Path: "/", Expires: expires, HttpOnly: true, Secure: s.cfg.PublicScheme == "https",
		SameSite: http.SameSiteLaxMode, // no Domain: the cookie stays on this one host
	})
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // localPath keeps it on this host
}

// localPath returns next if it is a path on this host, else "/". Anything
// a browser could read as another host ("//x", "/\x", "https:x") is out.
func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return u.RequestURI()
}

// errFrozenWeb marks an agent reply refused because a snapshot froze the
// workload after the gateway checked.
var errFrozenWeb = errors.New("gateway: the workspace is freezing for a snapshot")

// webProxy forwards to the editor or app port through the pod's agent,
// WebSockets included. The browser's session cookie stays here.
func (s *Server) webProxy(ws *sbxv1.Workspace, t webTarget, nav bool) *httputil.ReverseProxy {
	prefix := "/sbx/editor"
	if t.port != 0 {
		prefix = "/sbx/port/" + strconv.Itoa(t.port)
	}
	agentTok := s.cfg.AgentToken(ws)
	key := client.ObjectKeyFromObject(ws)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The pod IP comes from controller-written status, never from the client.
			pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(ws.Status.Pod.IP, strconv.Itoa(s.cfg.AgentRESTPort))})
			pr.Out.URL.Path, pr.Out.URL.RawPath = prefix+pr.In.URL.Path, ""
			pr.Out.Header.Set("Authorization", "Bearer "+agentTok)
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", s.cfg.PublicScheme)
			withoutCookie(pr.Out.Header, sessionCookie)
		},
		Transport: s.webTransport,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get(agent.FrozenHeader) != "" {
				return errFrozenWeb
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errFrozenWeb) {
				if nav {
					// Opening the page wins over the snapshot: it aborts the
					// suspend before its commit point, or wakes it after.
					ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
					defer cancel()
					_ = s.requestWake(ctx, key, sbxv1.WakeTriggerRequest)
					w.Header().Set("Refresh", "2")
				}
				w.Header().Set("Retry-After", "2")
				page(w, http.StatusServiceUnavailable, "Waking up", "The workspace was going to sleep. It's waking up; this page reloads by itself.")
				return
			}
			page(w, http.StatusBadGateway, "Not answering", "Nothing in the workspace answered: "+err.Error())
		},
	}
}

func withoutCookie(h http.Header, name string) {
	cookies := (&http.Request{Header: h}).Cookies()
	h.Del("Cookie")
	var keep []string
	for _, c := range cookies {
		if c.Name != name {
			keep = append(keep, c.String())
		}
	}
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

// isNavigation reports whether a person is opening a page: a top-level
// document request. Assets, XHR and WebSocket reconnects aren't.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	// Clients without fetch metadata: a GET for HTML.
	return !isUpgrade(r) && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// page writes a small HTML page for people (and its title for scripts).
func page(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:16px system-ui;margin:4rem auto;max-width:36rem"><h1>%s</h1><p>%s</p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}
