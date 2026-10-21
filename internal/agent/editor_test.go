package agent_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
)

// fakeEditor stands in for code-server: it serves on --bind-addr and says
// which host and path it was asked for.
func fakeEditor() {
	addr := ""
	for i, a := range os.Args {
		if a == "--bind-addr" && i+1 < len(os.Args) {
			addr = os.Args[i+1]
		}
	}
	_ = http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // test server
		fmt.Fprintf(w, "editor host=%s path=%s activity=%s", r.Host, r.URL.Path, os.Getenv("SBX_ACTIVITY_URL"))
	}))
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func get(t *testing.T, url string, header http.Header) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(authed(t), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func bearer() http.Header {
	return http.Header{"Authorization": {"Bearer " + tok}}
}

func TestEditorStartsOnFirstUseBehindTheToken(t *testing.T) {
	t.Setenv("SBX_FAKE_EDITOR", "1")
	h := startWith(t, func(c *agent.Config) {
		c.Editor = &agent.EditorConfig{Binary: os.Args[0], Addr: freeAddr(t)}
	})
	if code, _, _ := get(t, h.rest+"/sbx/editor/", nil); code != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", code)
	}
	hdr := bearer()
	hdr.Set("X-Forwarded-Host", "demo.team.sbx.localhost:8080")
	code, body, _ := get(t, h.rest+"/sbx/editor/static/app.js", hdr)
	if code != http.StatusOK || !strings.Contains(body, "host=demo.team.sbx.localhost:8080 path=/static/app.js") {
		t.Fatalf("editor: %d %q; want the path unprefixed and the public host passed on", code, body)
	}
	if !strings.Contains(body, "activity=http://127.0.0.1:") {
		t.Errorf("the editor wasn't told where to report activity: %q", body)
	}
	if _, err := h.agent.Freeze(authed(t), &agentv1.FreezeRequest{AttemptId: "a1"}); err != nil {
		t.Fatal(err)
	}
	if code, _, hd := get(t, h.rest+"/sbx/editor/", bearer()); code != http.StatusServiceUnavailable || hd.Get(agent.FrozenHeader) == "" {
		t.Fatalf("while frozen: %d", code)
	}
}

func TestPortProxyIncludingWebSockets(t *testing.T) {
	h := start(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			_, _ = rw.WriteString("echo " + line)
			_ = rw.Flush()
			return
		}
		fmt.Fprintf(w, "app host=%s path=%s", r.Host, r.URL.Path)
	}))
	defer app.Close()
	_, port, _ := net.SplitHostPort(app.Listener.Addr().String())

	code, body, _ := get(t, h.rest+"/sbx/port/"+port+"/api/items", bearer())
	if code != http.StatusOK || !strings.HasPrefix(body, "app host=127.0.0.1:"+port+" path=/api/items") {
		t.Fatalf("port proxy: %d %q", code, body)
	}
	if code, _, _ := get(t, h.rest+"/sbx/port/99999/", bearer()); code != http.StatusBadRequest {
		t.Errorf("bad port: %d", code)
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(h.rest, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /sbx/port/%s/ws HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", port, tok)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // a 101 has no body; the connection carries on
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %s", resp.Status)
	}
	fmt.Fprint(conn, "ping\n")
	if line, _ := br.ReadString('\n'); line != "echo ping\n" {
		t.Fatalf("over the socket: %q", line)
	}
}

func report(t *testing.T, h *harness, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(h.activity, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestEditorActivityReports(t *testing.T) {
	h := start(t)
	before, _ := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	time.Sleep(10 * time.Millisecond)
	if code := report(t, h, map[string]string{"kind": "edit"}); code != http.StatusNoContent {
		t.Fatalf("edit: %d", code)
	}
	after, _ := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if !after.GetSignals().GetLastActivity().AsTime().After(before.GetSignals().GetLastActivity().AsTime()) {
		t.Error("an edit didn't count as activity")
	}

	report(t, h, map[string]string{"kind": "terminal-start", "id": "t1", "command": "make build"})
	sig, _ := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	cmds := sig.GetSignals().GetRunningCommands()
	if len(cmds) != 1 || cmds[0].GetKind() != signalsv1.CommandKind_COMMAND_KIND_TERMINAL || cmds[0].GetCommand() != "make build" {
		t.Fatalf("running commands = %v, want the terminal command", cmds)
	}
	report(t, h, map[string]string{"kind": "terminal-end", "id": "t1"})
	sig, _ = h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if len(sig.GetSignals().GetRunningCommands()) != 0 {
		t.Fatal("the terminal command is still running after it ended")
	}
	if code := report(t, h, map[string]string{"kind": "heartbeat"}); code != http.StatusBadRequest {
		t.Errorf("an unknown kind: %d, want 400", code)
	}
}
