//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// browser talks to one workspace host through the gateway, the way a
// browser would after following a sign-in link.
type browser struct {
	t      *testing.T
	addr   string // the gateway, host:port
	host   string
	cookie string
}

func signIn(t *testing.T, gw, own *sbxapi.Client, ns, name string, port int) *browser {
	t.Helper()
	link, err := own.EditorURL(framework.Ctx(t, time.Minute), ns, name, port)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, addr: strings.TrimPrefix(gw.BaseURL, "http://"), host: u.Host}
	code, header, _ := b.get(u.RequestURI(), nil)
	for _, c := range (&http.Response{Header: header}).Cookies() {
		if c.Name == "sbx_session" {
			b.cookie = c.Name + "=" + c.Value
		}
	}
	if code != http.StatusSeeOther || b.cookie == "" {
		t.Fatalf("signing in to %s: %d", u.Host, code)
	}
	return b
}

func (b *browser) get(path string, header http.Header) (int, http.Header, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(framework.Ctx(b.t, 3*time.Minute), http.MethodGet, "http://"+b.addr+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Host = b.host
	if b.cookie != "" {
		req.Header.Set("Cookie", b.cookie)
	}
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// open navigates to path, as a person opening the page.
func (b *browser) open(path string) (int, string) {
	code, _, body := b.get(path, http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}, "Accept": {"text/html"}})
	return code, body
}

// socket opens a WebSocket and returns the response status.
func (b *browser) socket(path string) int {
	b.t.Helper()
	conn, err := net.Dial("tcp", b.addr)
	if err != nil {
		b.t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nOrigin: http://%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-Fetch-Mode: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, b.host, b.cookie, b.host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		b.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const editorSocket = "/?reconnectionToken=e2e&reconnection=false&skipWebSocketFrames=false"

// TestEditorThroughTheGateway opens code-server in a workspace whose image
// came from its devcontainer.json, with its WebSocket, through the gateway.
func TestEditorThroughTheGateway(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	resp, err := gw.Create(framework.Ctx(t, time.Minute), sbxapi.CreateRequest{
		RepoURL: p.FixturesURL + "/hello-deps.git", Policy: &sbxapi.PolicyRef{Name: "never"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, name := resp.Workspace.Namespace, resp.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	own := &sbxapi.Client{BaseURL: gw.BaseURL, Token: resp.Token, HTTP: gw.HTTP}
	w := waitPhase(t, own, ns, name, "Running", 5*time.Minute)
	if w.Image != nodeImage() || !strings.HasPrefix(w.ImageSource, "Devcontainer") {
		t.Errorf("image %q (%s), want the one devcontainer.json names", w.Image, w.ImageSource)
	}

	b := signIn(t, gw, own, ns, name, 0)
	start := time.Now()
	code, body := b.open("/?folder=/workspace")
	if code != http.StatusOK || !strings.Contains(body, "code-server") {
		t.Fatalf("opening the editor: %d", code)
	}
	t.Logf("editor loaded in %s (code-server starts on first use)", time.Since(start).Round(time.Millisecond))
	if code := b.socket(editorSocket); code != http.StatusSwitchingProtocols {
		t.Fatalf("editor WebSocket: %d", code)
	}
	out := run(t, own, ns, name, "ls /workspace/.sbx/editor/extensions")
	if !strings.Contains(out.Stdout, "sbx.activity-0.1.0") {
		t.Errorf("the activity extension isn't installed: %q", out.Stdout)
	}
}

// TestEditorReconnectDoesntWake leaves an editor "open" while the workspace
// sleeps: its reconnects and polls don't wake it; reloading the page does.
func TestEditorReconnectDoesntWake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := depsWorkspace(t, gw, p, nil)
	b := signIn(t, gw, own, ns, name, 0)
	if code, _ := b.open("/?folder=/workspace"); code != http.StatusOK {
		t.Fatalf("opening the editor: %d", code)
	}
	suspendAndWait(t, own, ns, name)

	if code := b.socket(editorSocket); code != http.StatusServiceUnavailable {
		t.Errorf("editor reconnect while asleep: %d, want 503", code)
	}
	if code, _, _ := b.get("/manifest.json", http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}}); code != http.StatusServiceUnavailable {
		t.Errorf("a background request while asleep: %d, want 503", code)
	}
	time.Sleep(3 * time.Second)
	if w, _ := own.Get(framework.Ctx(t, time.Minute), ns, name); w.Phase != "Suspended" {
		t.Fatalf("phase %s after reconnects; want Suspended", w.Phase)
	}

	code, body := b.open("/?folder=/workspace")
	if code != http.StatusOK || !strings.Contains(body, "code-server") {
		t.Fatalf("reloading the page: %d", code)
	}
	if w, _ := own.Get(framework.Ctx(t, time.Minute), ns, name); w.Phase != "Running" || w.Epoch < 2 {
		t.Errorf("after reloading: %s epoch %d, want a new epoch running", w.Phase, w.Epoch)
	}
}

// TestEditsKeepTheWorkspaceAwake reports edits the way the editor extension
// does: the workspace stays up past its idle timeout while they come in,
// and sleeps once they stop.
func TestEditsKeepTheWorkspaceAwake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	resp, err := gw.Create(framework.Ctx(t, time.Minute), sbxapi.CreateRequest{
		RepoURL: p.FixturesURL + "/hello-deps.git", Image: nodeImage(),
		Policy: fixedPolicy("idleTimeout", "15s", "afterTurnEnded", "0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, name := resp.Workspace.Namespace, resp.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	own := &sbxapi.Client{BaseURL: gw.BaseURL, Token: resp.Token, HTTP: gw.HTTP}
	waitPhase(t, own, ns, name, "Running", 5*time.Minute)

	noDeps := false
	typist := sbxapi.Service{Name: "typist", WaitForDeps: &noDeps, Command: []string{"node", "-e",
		`setInterval(() => fetch("http://127.0.0.1:9092/v1/activity", {method: "POST", body: JSON.stringify({kind: "edit"})}).catch(() => {}), 3000)`}}
	if err := own.PutService(framework.Ctx(t, time.Minute), ns, name, typist); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(40 * time.Second) // well past the 15s timeout
	for time.Now().Before(deadline) {
		if w, err := own.Get(context.Background(), ns, name); err != nil || w.Phase != "Running" {
			t.Fatalf("phase %s while edits come in (%v)", w.Phase, err)
		}
		time.Sleep(time.Second)
	}
	// The typist stops: the workspace sleeps once the idle timeout passes.
	if err := own.DeleteService(framework.Ctx(t, time.Minute), ns, name, "typist"); err != nil {
		t.Fatal(err)
	}
	w := waitPhase(t, own, ns, name, "Suspended", 2*time.Minute)
	if w.Sleep == nil || w.Sleep.Reason != "Idle" {
		t.Errorf("sleep = %+v, want Idle", w.Sleep)
	}
}

// TestAppPortThroughTheGateway reaches a dev server in the workspace on its
// own host.
func TestAppPortThroughTheGateway(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := depsWorkspace(t, gw, p, []sbxapi.Service{{Name: "web", Command: []string{"npm", "run", "dev"}}})
	b := signIn(t, gw, own, ns, name, 3000)
	framework.Eventually(t, time.Minute, 500*time.Millisecond, "the dev server answers", func() (bool, error) {
		code, body := b.open("/")
		return code == http.StatusOK && body == "hello, visitor\n", nil
	})
}
