//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// TestUIIsServed: the gateway serves the web UI it was built with at /ui/,
// the app's own routes too, and / leads there.
func TestUIIsServed(t *testing.T) {
	gw := framework.Gateway(t)
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, http.Header, string) {
		t.Helper()
		resp, err := hc.Get(gw.BaseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(body)
	}

	code, _, page := get("/ui/")
	if code != http.StatusOK || !strings.Contains(page, `<div id="root">`) {
		t.Fatalf("/ui/: %d %.200q (built without make web?)", code, page)
	}
	script := regexp.MustCompile(`/ui/assets/[^"]+\.js`).FindString(page)
	if script == "" {
		t.Fatalf("no script in %q", page)
	}
	if code, h, body := get(script); code != http.StatusOK || !strings.Contains(h.Get("Cache-Control"), "immutable") || len(body) < 1000 {
		t.Errorf("%s: %d cache=%q %d bytes", script, code, h.Get("Cache-Control"), len(body))
	}
	if code, _, body := get("/ui/w/sbx-workspaces/some-sandbox"); code != http.StatusOK || body != page {
		t.Errorf("an app route: %d, want the app", code)
	}
	if code, h, _ := get("/"); code != http.StatusFound || h.Get("Location") != "/ui/" {
		t.Errorf("/: %d to %q, want a redirect to /ui/", code, h.Get("Location"))
	}
}

// TestSettingsSwitchTheClusterPolicy does what the settings page does: it
// switches the cluster's sleep policy through the API, a running workspace
// that has none of its own follows it, and a reset brings the chart's back.
func TestSettingsSwitchTheClusterPolicy(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ctx := framework.Ctx(t, 8*time.Minute)
	before, err := gw.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if before.Source == sleep.PolicyConfigMap {
			_, _ = gw.SetPolicy(context.Background(), sbxapi.PolicyRef{Name: before.Name, Params: before.Params})
		} else {
			_, _ = gw.ResetPolicy(context.Background())
		}
	})
	if len(before.Specs) < 2 {
		t.Fatalf("specs %+v, want fixed and never at least", before.Specs)
	}

	var se *sbxapi.StatusError
	if _, err := gw.SetPolicy(ctx, *fixedPolicy("idleTimeout", "whenever")); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("an invalid policy: %v, want 400", err)
	}

	ns, name, own := gwWorkspace(t, gw, p, nil)
	got, err := gw.SetPolicy(ctx, *fixedPolicy("idleTimeout", "10m", "afterTurnEnded", "3s"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != sleep.PolicyConfigMap || got.Name != "fixed" || got.Params["afterTurnEnded"] != "3s" {
		t.Fatalf("after saving: %+v", got)
	}
	if err := own.TurnEnded(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	if w := waitPhase(t, own, ns, name, "Suspended", 2*time.Minute); w.Sleep == nil || w.Sleep.Reason != "TurnEnded" {
		t.Errorf("suspended with %+v, want TurnEnded under the new policy", w.Sleep)
	}

	got, err = gw.ResetPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != sleep.DefaultsConfigMap {
		t.Errorf("after a reset the policy comes from %q, want the chart's", got.Source)
	}
}

type serverEvent struct {
	name string
	data string
}

// TestEventsStreamWorkspaceChanges reads /api/v1/events the way the UI
// does: every workspace, then "ready", then a new workspace as it changes,
// until it is deleted.
func TestEventsStreamWorkspaceChanges(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gw.BaseURL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gw.Token)
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req) //nolint:bodyclose // closed by the deferred func below, after the reader stops
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan serverEvent, 1024)
	read := make(chan struct{})
	go func() {
		defer close(read)
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var ev serverEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					select {
					case events <- ev:
					case <-ctx.Done():
						return
					}
				}
				ev = serverEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	defer func() {
		cancel()
		resp.Body.Close()
		<-read
	}()

	// until returns once an event matches, failing on a timeout.
	until := func(what string, timeout time.Duration, match func(serverEvent) bool) {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("the stream ended before %s", what)
				}
				if match(ev) {
					return
				}
			case <-deadline:
				t.Fatalf("no %s within %s", what, timeout)
			}
		}
	}
	until(`"ready"`, 30*time.Second, func(ev serverEvent) bool { return ev.name == "ready" })

	created, err := gw.Create(ctx, sbxapi.CreateRequest{
		RepoURL: p.FixturesURL + "/hello-node.git", Image: "debian:trixie-slim", Policy: &sbxapi.PolicyRef{Name: "never"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, name := created.Workspace.Namespace, created.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	var phases []string
	until(name+" running", 5*time.Minute, func(ev serverEvent) bool {
		var w sbxapi.Workspace
		if ev.name != "workspace" || json.Unmarshal([]byte(ev.data), &w) != nil || w.Namespace != ns || w.Name != name {
			return false
		}
		if len(phases) == 0 || phases[len(phases)-1] != w.Phase {
			phases = append(phases, w.Phase)
		}
		return w.Phase == "Running"
	})
	t.Logf("phases streamed: %v", phases)

	if err := gw.Delete(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	until(name+" deleted", 3*time.Minute, func(ev serverEvent) bool {
		return ev.name == "deleted" && strings.Contains(ev.data, `"name":"`+name+`"`)
	})
}
