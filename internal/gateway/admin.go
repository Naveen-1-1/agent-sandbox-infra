package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

// --- the cluster's sleep policy ---

// getPolicy reports the cluster policy the way the controller resolves it
// (sbx-policy, else the chart's defaults, else never) and every policy one
// can choose, with its parameters.
func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	out := sbxapi.PolicySettings{Name: "never", Source: "built-in"}
	for _, spec := range policy.Specs() {
		ps := sbxapi.PolicySpec{Name: spec.Name, Help: spec.Help}
		for _, p := range spec.Params {
			ps.Params = append(ps.Params, sbxapi.PolicyParam{Name: p.Name, Default: p.Default, Help: p.Help})
		}
		out.Specs = append(out.Specs, ps)
	}
	for _, name := range []string{sleep.PolicyConfigMap, sleep.DefaultsConfigMap} {
		cm := &corev1.ConfigMap{}
		err := s.waker.API.Get(r.Context(), client.ObjectKey{Namespace: s.cfg.PolicyNamespace, Name: name}, cm)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
			return
		}
		c, _, err := policy.ParseConfig([]byte(cm.Data[policy.ConfigKey]))
		if err != nil {
			// The controller keeps the last good policy; say why this one isn't used.
			out.Invalid = fmt.Sprintf("%s: %v", name, err)
			continue
		}
		out.Name, out.Params, out.Source = c.Name, c.Params, name
		break
	}
	writeJSON(w, http.StatusOK, out)
}

// putPolicy validates a policy and writes it to sbx-policy, which the
// controller applies to every running workspace at once.
func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	var req sbxapi.PolicyRef
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, err.Error())
		return
	}
	if _, err := (policy.Config{Name: req.Name, Params: req.Params}).Build(); err != nil {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, err.Error())
		return
	}
	data, err := yaml.Marshal(policy.Config{Name: req.Name, Params: req.Params})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return
	}
	ctx := r.Context()
	key := client.ObjectKey{Namespace: s.cfg.PolicyNamespace, Name: sleep.PolicyConfigMap}
	cm := &corev1.ConfigMap{}
	err = s.waker.API.Get(ctx, key, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name,
				Labels: map[string]string{"app.kubernetes.io/part-of": "sbx"}},
			Data: map[string]string{policy.ConfigKey: string(data)},
		}
		err = s.client.Create(ctx, cm)
	case err == nil:
		cm.Data = map[string]string{policy.ConfigKey: string(data)}
		err = s.client.Update(ctx, cm) // resourceVersion-checked
	}
	if err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			code = http.StatusConflict // someone else saved meanwhile; reload and retry
		}
		writeErr(w, code, sbxapi.CodeInternal, err.Error())
		return
	}
	s.getPolicy(w, r)
}

// resetPolicy deletes sbx-policy, so the chart's defaults apply again.
func (s *Server) resetPolicy(w http.ResponseWriter, r *http.Request) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.cfg.PolicyNamespace, Name: sleep.PolicyConfigMap}}
	if err := s.client.Delete(r.Context(), cm); client.IgnoreNotFound(err) != nil {
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return
	}
	s.getPolicy(w, r)
}

// --- live workspace events ---

// feed fans workspace changes out to event streams. Each subscriber keeps
// a set of changed workspaces rather than a queue, so a slow browser sees
// the latest state of each, never a backlog.
type feed struct {
	max  int
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	mu    sync.Mutex
	dirty map[string]bool
	wake  chan struct{}
}

func (f *feed) subscribe() (*subscriber, func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subs) >= f.max {
		return nil, nil, false
	}
	sub := &subscriber{dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
	f.subs[sub] = struct{}{}
	return sub, func() {
		f.mu.Lock()
		delete(f.subs, sub)
		f.mu.Unlock()
	}, true
}

func (f *feed) publish(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub := range f.subs {
		sub.mu.Lock()
		sub.dirty[key] = true
		sub.mu.Unlock()
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

func (sub *subscriber) take() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	keys := make([]string, 0, len(sub.dirty))
	for k := range sub.dirty {
		keys = append(keys, k)
	}
	clear(sub.dirty)
	return keys
}

// events streams every workspace as server-sent events: all of them first,
// then "ready", then each change as it happens ("workspace" with the new
// view, or "deleted"). A comment every 15 seconds keeps proxies from
// closing an idle stream.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	sub, done, ok := s.feed.subscribe()
	if !ok {
		s.reject(w, http.StatusServiceUnavailable, sbxapi.CodeQueueFull, "too many event streams", "streams")
		return
	}
	defer done()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return rc.Flush()
	}

	ctx := r.Context()
	list := &sbxv1.WorkspaceList{}
	if err := s.cache.List(ctx, list); err != nil {
		_ = send("error", sbxapi.Error{Error: err.Error(), Code: sbxapi.CodeInternal})
		return
	}
	for i := range list.Items {
		if send("workspace", View(&list.Items[i])) != nil {
			return
		}
	}
	if send("ready", struct{}{}) != nil {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-sub.wake:
			for _, key := range sub.take() {
				if err := s.sendChange(ctx, key, send); err != nil {
					return
				}
			}
		}
	}
}

func (s *Server) sendChange(ctx context.Context, key string, send func(string, any) error) error {
	ns, name, _ := strings.Cut(key, "/")
	ws := &sbxv1.Workspace{}
	err := s.cache.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ws)
	if apierrors.IsNotFound(err) {
		return send("deleted", map[string]string{"namespace": ns, "name": name})
	}
	if err != nil {
		return nil //nolint:nilerr // skip it: the next change sends the latest state anyway
	}
	return send("workspace", View(ws))
}
