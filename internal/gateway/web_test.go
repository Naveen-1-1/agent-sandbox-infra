package gateway_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/gateway"
)

const domain = "sbx.test"

// app is a dev server inside the "workspace" (on loopback, next to the
// test agent): it echoes what it got, and echoes lines over WebSockets.
func app(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			for {
				line, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				_, _ = rw.WriteString("echo " + line)
				_ = rw.Flush()
			}
		}
		fmt.Fprintf(w, "app %s %s cookie=%q", r.Method, r.URL.Path, r.Header.Get("Cookie"))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

type browser struct {
	t      *testing.T
	base   string // the gateway's address
	host   string // the workspace host it talks to
	cookie *http.Cookie
}

// reply is what a request got back.
type reply struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
}

func (r reply) Cookies() []*http.Cookie { return r.cookies }

func (b *browser) do(method, path string, header http.Header) (reply, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(ctx(b.t), method, b.base+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	if header != nil {
		req.Header = header.Clone()
	}
	req.Host = b.host
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	resp, err := (&http.Transport{}).RoundTrip(req) // no redirects followed
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies()}, string(body)
}

// login follows a sign-in link the way a browser would.
func (b *browser) login(link string) reply {
	b.t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		b.t.Fatal(err)
	}
	b.host = u.Host
	resp, _ := b.do(http.MethodGet, u.RequestURI(), nil)
	for _, c := range resp.Cookies() {
		if c.Name == "sbx_session" {
			b.cookie = c
		}
	}
	return resp
}

func navigate() http.Header {
	return http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Accept": {"text/html"}}
}

func TestWorkspaceHostsNeedASignIn(t *testing.T) {
	e := start(t, gateway.Config{Domain: domain}, 0)
	_, own := e.workspace(t, "web", sbxv1.PhaseRunning, "")
	port := app(t)
	b := &browser{t: t, base: e.http.URL, host: fmt.Sprintf("%d.web.%s.%s", port, e.ns, domain)}

	if resp, _ := b.do(http.MethodGet, "/", navigate()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without signing in: %d", resp.StatusCode)
	}
	link, err := own.EditorURL(ctx(t), e.ns, "web", port)
	if err != nil {
		t.Fatal(err)
	}
	if resp := b.login(link); resp.StatusCode != http.StatusSeeOther || b.cookie == nil {
		t.Fatalf("login: %d, cookie %v", resp.StatusCode, b.cookie)
	}
	if !b.cookie.HttpOnly || b.cookie.Domain != "" || b.cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie %+v: want HttpOnly, host-only, SameSite=Lax", b.cookie)
	}
	resp, body := b.do(http.MethodGet, "/api/items", http.Header{"Cookie": {"theme=dark"}})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "app GET /api/items") {
		t.Fatalf("signed in: %d %q", resp.StatusCode, body)
	}
	if strings.Contains(body, "sbx_session") || !strings.Contains(body, "theme=dark") {
		t.Errorf("the app got cookies %s; want its own, without the session", body)
	}

	// The cookie belongs to this host only.
	other := &browser{t: t, base: e.http.URL, host: "web." + e.ns + "." + domain, cookie: b.cookie}
	if resp, _ := other.do(http.MethodGet, "/", navigate()); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the app port's cookie on the editor host: %d", resp.StatusCode)
	}
	// A stale or forged ticket gets nothing.
	if resp := (&browser{t: t, base: e.http.URL}).login(link + "0"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged ticket: %d", resp.StatusCode)
	}
	// After signing in, the browser goes on to a page on this host only.
	for next, want := range map[string]string{
		"/files?x=1": "/files?x=1", "//evil.example/": "/", "/\\evil.example": "/", "https://evil.example": "/",
	} {
		fresh := &browser{t: t, base: e.http.URL}
		if resp := fresh.login(link + "&next=" + url.QueryEscape(next)); resp.Header.Get("Location") != want {
			t.Errorf("next=%q: redirected to %q, want %q", next, resp.Header.Get("Location"), want)
		}
	}
	// Scripts may use the workspace token instead.
	script := &browser{t: t, base: e.http.URL, host: b.host}
	if resp, body := script.do(http.MethodGet, "/x", http.Header{"Authorization": {"Bearer " + own.Token}}); resp.StatusCode != http.StatusOK {
		t.Errorf("bearer token: %d %q", resp.StatusCode, body)
	}
}

// upgrade opens a WebSocket through the gateway and returns the connection.
func upgrade(t *testing.T, b *browser, origin string) (int, *bufio.Reader, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(b.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nOrigin: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-Fetch-Mode: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		b.host, b.cookie.String(), origin)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
	}
	return resp.StatusCode, br, conn
}

func TestCookieRequestsMustComeFromTheWorkspaceOrigin(t *testing.T) {
	e := start(t, gateway.Config{Domain: domain}, 0)
	_, own := e.workspace(t, "web", sbxv1.PhaseRunning, "")
	port := app(t)
	b := &browser{t: t, base: e.http.URL}
	link, _ := own.EditorURL(ctx(t), e.ns, "web", port)
	b.login(link)
	origin := "http://" + b.host

	if resp, _ := b.do(http.MethodPost, "/save", http.Header{"Origin": {"http://evil.example"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", resp.StatusCode)
	}
	if resp, body := b.do(http.MethodPost, "/save", http.Header{"Origin": {origin}}); resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "app POST /save") {
		t.Errorf("same-origin POST: %d %q", resp.StatusCode, body)
	}
	if code, _, _ := upgrade(t, b, "http://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-origin WebSocket: %d", code)
	}

	code, br, conn := upgrade(t, b, origin)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket: %d", code)
	}
	fmt.Fprint(conn, "hello\n")
	if line, _ := br.ReadString('\n'); line != "echo hello\n" {
		t.Fatalf("over the socket: %q", line)
	}
	sig, err := e.srv.Signals().GetWorkspaceSignals(context.Background(), &gatewayv1.GetWorkspaceSignalsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var apps int32
	for _, st := range sig.GetSignals()[e.ns+"/web"].GetOpenStreams() {
		if st.GetKind() == signalsv1.StreamKind_STREAM_KIND_APP {
			apps = st.GetCount()
		}
	}
	if apps != 1 {
		t.Errorf("open app streams = %d, want 1 while the socket is open", apps)
	}
}

func TestOnlyOpeningThePageWakesTheWorkspace(t *testing.T) {
	e := start(t, gateway.Config{Domain: domain}, 0)
	ws, own := e.workspace(t, "web", sbxv1.PhaseSuspended, "")
	port := app(t)
	b := &browser{t: t, base: e.http.URL}
	link, _ := own.EditorURL(ctx(t), e.ns, "web", port)
	b.login(link)

	// An open tab reconnecting, an app polling, an asset: none wakes it.
	if resp, _ := b.do(http.MethodGet, "/api/poll", http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}}); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Sbx-Asleep") == "" {
		t.Errorf("XHR while asleep: %d", resp.StatusCode)
	}
	if code, _, _ := upgrade(t, b, "http://"+b.host); code != http.StatusServiceUnavailable {
		t.Errorf("WebSocket reconnect while asleep: %d", code)
	}
	if resp, _ := b.do(http.MethodGet, "/frame", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an iframe load while asleep: %d", resp.StatusCode)
	}
	if e.seq(t, ws) != 0 {
		t.Fatal("background traffic woke the workspace")
	}

	resp, body := b.do(http.MethodGet, "/", navigate())
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "app GET /") {
		t.Fatalf("opening the page: %d %q", resp.StatusCode, body)
	}
	if e.seq(t, ws) != 1 {
		t.Fatalf("wake seq = %d after opening the page, want 1", e.seq(t, ws))
	}
}

func TestOtherHostsGetTheAPI(t *testing.T) {
	e := start(t, gateway.Config{Domain: domain}, 0)
	for _, host := range []string{"", "sbx.test", "a.sbx.test", "x.a.b.c." + domain, "notaport.web." + e.ns + "." + domain} {
		b := &browser{t: t, base: e.http.URL, host: host}
		if host == "" {
			b.host = strings.TrimPrefix(e.http.URL, "http://")
		}
		resp, _ := b.do(http.MethodGet, "/api/v1/workspaces", http.Header{"Authorization": {"Bearer " + adminToken}})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("host %q: %d, want the API", host, resp.StatusCode)
		}
	}
}
