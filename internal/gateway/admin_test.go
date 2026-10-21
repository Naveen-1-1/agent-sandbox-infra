package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/gateway"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

func TestPolicySettings(t *testing.T) {
	e := start(t, gateway.Config{}, 0)
	got, err := e.admin.Policy(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "never" || got.Source != "built-in" || len(got.Specs) < 2 || got.Specs[0].Name != "fixed" || len(got.Specs[0].Params) == 0 {
		t.Fatalf("with no config: %+v", got)
	}

	defaults := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: sleep.DefaultsConfigMap},
		Data: map[string]string{policy.ConfigKey: "name: fixed\nparams: {idleTimeout: 10m}\n"}}
	if err := k8s.Create(ctx(t), defaults); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.admin.Policy(ctx(t)); got.Source != sleep.DefaultsConfigMap || got.Params["idleTimeout"] != "10m" {
		t.Fatalf("with the chart's defaults: %+v", got)
	}

	set := sbxapi.PolicyRef{Name: "fixed", Params: map[string]string{"idleTimeout": "2m", "afterTurnEnded": "30s"}}
	for range 2 { // create, then update
		if got, err = e.admin.SetPolicy(ctx(t), set); err != nil || got.Source != sleep.PolicyConfigMap || got.Params["idleTimeout"] != "2m" {
			t.Fatalf("after setting it: %+v, %v", got, err)
		}
		set.Params["idleTimeout"] = "2m"
	}
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx(t), client.ObjectKey{Namespace: e.ns, Name: sleep.PolicyConfigMap}, cm); err != nil {
		t.Fatal(err)
	}
	if _, p, err := policy.ParseConfig([]byte(cm.Data[policy.ConfigKey])); err != nil || p.Name() != "fixed" {
		t.Fatalf("written config %q: %v", cm.Data[policy.ConfigKey], err)
	}
	if _, err := e.admin.SetPolicy(ctx(t), sbxapi.PolicyRef{Name: "fixed", Params: map[string]string{"idleTimout": "1m"}}); statusOf(err) != http.StatusBadRequest {
		t.Errorf("a misspelled parameter: %v, want 400", err)
	}
	if got, err := e.admin.ResetPolicy(ctx(t)); err != nil || got.Source != sleep.DefaultsConfigMap {
		t.Fatalf("after resetting: %+v, %v", got, err)
	}

	_, own := e.workspace(t, "member", sbxv1.PhaseRunning, "")
	if _, err := own.Policy(ctx(t)); statusOf(err) != http.StatusUnauthorized {
		t.Errorf("a workspace token read the cluster policy: %v", err)
	}
}

type event struct {
	name string
	data map[string]any
}

// stream opens /api/v1/events and returns its events as they come.
func stream(t *testing.T, e *env) <-chan event {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx(t), http.MethodGet, e.http.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := tr.RoundTrip(req) //nolint:bodyclose // the reader goroutine below closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		_ = resp.Body.Close()
		t.Fatalf("events: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	out := make(chan event, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev event
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data)
			case line == "" && ev.name != "":
				out <- ev
				ev = event{}
			}
		}
	}()
	return out
}

// next waits for an event named name, about namespace ns if ns is set
// (other tests' workspaces share the API server).
func next(t *testing.T, events <-chan event, name, ns string) event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended waiting for %q", name)
			}
			if ev.name == name && (ns == "" || ev.data["namespace"] == ns) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %q event", name)
		}
	}
}

func TestEventsStreamWorkspaceChanges(t *testing.T) {
	e := start(t, gateway.Config{}, -1)
	existing, _ := e.workspace(t, "before", sbxv1.PhaseRunning, "")
	events := stream(t, e)
	first := next(t, events, "workspace", e.ns)
	if first.data["name"] != existing.Name || first.data["phase"] != "Running" {
		t.Fatalf("snapshot: %v", first.data)
	}
	next(t, events, "ready", "")

	cur := &sbxv1.Workspace{}
	if err := k8s.Get(ctx(t), client.ObjectKeyFromObject(existing), cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Phase = sbxv1.PhaseSuspending
	if err := k8s.Status().Update(ctx(t), cur); err != nil {
		t.Fatal(err)
	}
	e.srv.Notify(client.ObjectKeyFromObject(cur).String())
	if ev := next(t, events, "workspace", e.ns); ev.data["phase"] != "Suspending" {
		t.Fatalf("change: %v", ev.data)
	}

	if err := k8s.Delete(ctx(t), cur); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for { // the finalizer-free fake workspace goes away at once; wait for the cache
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(cur), &sbxv1.Workspace{}); err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.srv.Forget(client.ObjectKeyFromObject(cur).String())
	if ev := next(t, events, "deleted", e.ns); ev.data["name"] != existing.Name {
		t.Fatalf("deleted: %v", ev.data)
	}
}

func TestEventsNeedTheAdminToken(t *testing.T) {
	e := start(t, gateway.Config{}, -1)
	_, own := e.workspace(t, "w", sbxv1.PhaseRunning, "")
	req, _ := http.NewRequestWithContext(ctx(t), http.MethodGet, e.http.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+own.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a workspace token opened the fleet's event stream: %d", resp.StatusCode)
	}
}
