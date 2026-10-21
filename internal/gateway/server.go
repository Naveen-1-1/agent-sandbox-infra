package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Config configures the gateway.
type Config struct {
	Keys       token.Keyring
	AdminToken string
	// WorkspaceNamespace is where workspaces are created by default.
	WorkspaceNamespace string

	// Holding: per-workspace and total waiters, and how long each may wait.
	HoldPerWorkspace int
	HoldGlobal       int
	HoldTimeout      time.Duration

	ExecTimeout time.Duration
	// DepsTimeout bounds how long a command waits for the dependency
	// directory (an install can take minutes).
	DepsTimeout time.Duration
	// MaxBufferedBody is the largest file upload buffered so it can be
	// retried after a wake.
	MaxBufferedBody int64
	// Wake-hints are best effort: beyond this rate they are dropped.
	HintsPerSecond float64
	HintBurst      int

	AgentGRPCPort, AgentRESTPort int
	// AgentToken returns the token for a workspace's current pod. Default:
	// derived for the workspace's epoch.
	AgentToken func(ws *sbxv1.Workspace) string
}

func (c *Config) defaults() {
	if c.HoldPerWorkspace <= 0 {
		c.HoldPerWorkspace = 256
	}
	if c.HoldGlobal <= 0 {
		c.HoldGlobal = 4096
	}
	if c.HoldTimeout <= 0 {
		c.HoldTimeout = 90 * time.Second
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = 10 * time.Minute
	}
	if c.DepsTimeout <= 0 {
		c.DepsTimeout = 15 * time.Minute
	}
	if c.MaxBufferedBody <= 0 {
		c.MaxBufferedBody = 8 << 20
	}
	if c.HintsPerSecond <= 0 {
		c.HintsPerSecond, c.HintBurst = 50, 100
	}
	if c.AgentGRPCPort == 0 {
		c.AgentGRPCPort = podspec.AgentGRPCPort
	}
	if c.AgentRESTPort == 0 {
		c.AgentRESTPort = podspec.AgentRESTPort
	}
	if c.AgentToken == nil {
		keys := c.Keys
		c.AgentToken = func(ws *sbxv1.Workspace) string { return keys.Derive(token.Agent, string(ws.UID), ws.Status.Epoch) }
	}
}

// Server is the gateway.
type Server struct {
	cfg     Config
	cache   client.Reader // cached Workspaces, kept fresh by an informer
	client  client.Client // writes
	holder  *Holder
	waker   *Waker
	signals *Signals
	metrics *metrics.Gateway
	hints   *rate.Limiter
	podHTTP *http.Client
	log     *slog.Logger
	synced  atomic.Bool

	mu     sync.Mutex
	hintAt map[string]time.Time
}

// New builds a gateway. cache serves Workspaces from an informer; api reads
// them uncached for wake decisions.
func New(cfg Config, cache client.Reader, c client.Client, api client.Reader, reg prometheus.Registerer, log *slog.Logger) *Server {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	h := &Holder{PerWorkspace: cfg.HoldPerWorkspace, Global: cfg.HoldGlobal, Timeout: cfg.HoldTimeout}
	s := &Server{
		cfg: cfg, cache: cache, client: c, holder: h,
		waker:   &Waker{Client: c, API: api},
		signals: NewSignals(h),
		hints:   rate.NewLimiter(rate.Limit(cfg.HintsPerSecond), cfg.HintBurst),
		podHTTP: &http.Client{Timeout: cfg.ExecTimeout},
		log:     log,
		hintAt:  map[string]time.Time{},
	}
	s.metrics = metrics.NewGateway(reg, h.Total)
	h.Observe = s.metrics.ObserveHold
	return s
}

// Close releases idle connections to workspace pods.
func (s *Server) Close() { s.podHTTP.CloseIdleConnections() }

// Signals is this replica's traffic view, served over gRPC to the controller.
func (s *Server) Signals() *Signals { return s.signals }

// Notify tells waiters on a workspace that it changed (informer events).
func (s *Server) Notify(key string) { s.holder.Notify(key) }

// Forget drops state for a deleted workspace.
func (s *Server) Forget(key string) {
	s.signals.Forget(key)
	s.mu.Lock()
	delete(s.hintAt, key)
	s.mu.Unlock()
	s.holder.Notify(key)
}

// SetSynced marks the workspace cache as ready.
func (s *Server) SetSynced() { s.synced.Store(true) }

// AdminHandler serves health and metrics. Nothing here ever wakes a workspace.
func (s *Server) AdminHandler(g prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.synced.Load() {
			http.Error(w, "workspace cache not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace)

// APIHandler serves the REST API (see package sbxapi).
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	const ws = "/api/v1/workspaces/{ns}/{name}"
	mux.HandleFunc("POST /api/v1/workspaces", s.admin(s.create))
	mux.HandleFunc("GET /api/v1/workspaces", s.admin(s.list))
	mux.HandleFunc("GET "+ws, s.member(s.get))
	mux.HandleFunc("DELETE "+ws, s.adminFor(s.delete))
	mux.HandleFunc("GET "+ws+"/token", s.adminFor(s.token))
	mux.HandleFunc("POST "+ws+"/exec", s.member(s.exec))
	mux.HandleFunc("POST "+ws+"/wake", s.member(s.wake))
	mux.HandleFunc("POST "+ws+"/wake-hint", s.member(s.wakeHint))
	mux.HandleFunc("POST "+ws+"/turn-ended", s.member(s.turnEnded))
	mux.HandleFunc("POST "+ws+"/suspend", s.member(s.suspend))
	mux.HandleFunc(ws+"/files/{path...}", s.member(s.files))
	mux.HandleFunc("GET "+ws+"/services", s.member(s.listServices))
	mux.HandleFunc("PUT "+ws+"/services/{svc}", s.member(s.putService))
	mux.HandleFunc("DELETE "+ws+"/services/{svc}", s.member(s.deleteService))
	return s.instrument(mux)
}

// --- auth ---

func bearer(r *http.Request) string {
	tok, _ := token.FromHeader(r.Header.Get("Authorization"))
	return tok
}

func (s *Server) isAdmin(r *http.Request) bool {
	return s.cfg.AdminToken != "" && token.Equal(bearer(r), s.cfg.AdminToken) == nil
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			s.reject(w, http.StatusUnauthorized, sbxapi.CodeUnauthorized, "admin token required", metrics.RejectAuth)
			return
		}
		h(w, r)
	}
}

// lookup resolves the workspace a route names.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*sbxv1.Workspace, bool) {
	ws := &sbxv1.Workspace{}
	err := s.cache.Get(r.Context(), client.ObjectKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}, ws)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Same answer whether or not it exists, unless the caller is admin.
			if s.isAdmin(r) {
				writeErr(w, http.StatusNotFound, sbxapi.CodeNotFound, "no such workspace")
			} else {
				s.reject(w, http.StatusUnauthorized, sbxapi.CodeUnauthorized, "invalid token", metrics.RejectAuth)
			}
			return nil, false
		}
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return nil, false
	}
	return ws, true
}

// member allows the admin token or the workspace's own token.
func (s *Server) member(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := s.lookup(w, r)
		if !ok {
			return
		}
		if !s.isAdmin(r) && s.cfg.Keys.Verify(bearer(r), token.Gateway, string(ws.UID), 0) != nil {
			s.reject(w, http.StatusUnauthorized, sbxapi.CodeUnauthorized, "invalid token", metrics.RejectAuth)
			return
		}
		h(w, r, ws)
	}
}

func (s *Server) adminFor(h handler) http.HandlerFunc {
	return s.admin(func(w http.ResponseWriter, r *http.Request) {
		if ws, ok := s.lookup(w, r); ok {
			h(w, r, ws)
		}
	})
}

// --- responses ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, apiCode, msg string) {
	writeJSON(w, code, sbxapi.Error{Error: msg, Code: apiCode})
}

func (s *Server) reject(w http.ResponseWriter, code int, apiCode, msg, reason string) {
	s.metrics.Rejected.WithLabelValues(reason).Inc()
	if code == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "2")
	}
	writeErr(w, code, apiCode, msg)
}

// wakeErr turns a hold or wake failure into a response.
func (s *Server) wakeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		s.reject(w, http.StatusServiceUnavailable, sbxapi.CodeQueueFull, "too many requests waiting for this workspace to wake", metrics.RejectQueueFull)
	case errors.Is(err, errDepsTimeout):
		s.reject(w, http.StatusGatewayTimeout, sbxapi.CodeDepsTimeout, "the dependencies weren't restored in time", metrics.RejectWakeTimeout)
	case errors.Is(err, ErrWakeTimeout):
		s.reject(w, http.StatusGatewayTimeout, sbxapi.CodeWakeTimeout, "the workspace did not wake in time", metrics.RejectWakeTimeout)
	case errors.Is(err, ErrOff):
		s.reject(w, http.StatusConflict, sbxapi.CodeOff, "the workspace is off; set mode to Auto to use it", metrics.RejectOff)
	case apierrors.IsNotFound(err):
		writeErr(w, http.StatusNotFound, sbxapi.CodeNotFound, "the workspace was deleted")
	case errors.Is(err, context.Canceled):
		// The client went away; nobody is listening.
	default:
		writeErr(w, http.StatusServiceUnavailable, sbxapi.CodeUnavailable, err.Error())
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.metrics.Requests.WithLabelValues(route, strconv.Itoa(rec.code)).Inc()
	})
}

// --- waking ---

// awake returns ws once it is ready to serve, holding the request (and
// asking for a wake) while it isn't.
func (s *Server) awake(ctx context.Context, ws *sbxv1.Workspace) (*sbxv1.Workspace, time.Duration, error) {
	key := client.ObjectKeyFromObject(ws)
	s.observeHintLead(key.String())
	if Ready(ws) {
		return ws, 0, nil
	}
	start := time.Now()
	cur := ws
	err := s.holder.Wait(ctx, key.String(), func() (bool, error) {
		fresh := &sbxv1.Workspace{}
		if err := s.cache.Get(ctx, key, fresh); err != nil {
			return false, err
		}
		cur = fresh
		if fresh.Spec.Mode == sbxv1.ModeOff {
			return false, ErrOff
		}
		if Ready(fresh) {
			return true, nil
		}
		if NeedsWake(fresh) {
			if err := s.requestWake(ctx, key, sbxv1.WakeTriggerRequest); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	return cur, time.Since(start), err
}

func (s *Server) requestWake(ctx context.Context, key client.ObjectKey, trigger sbxv1.WakeTrigger) error {
	made, err := s.waker.Wake(ctx, key, trigger)
	if made {
		s.metrics.WakeRequests.WithLabelValues(string(trigger)).Inc()
		if trigger == sbxv1.WakeTriggerHint {
			s.mu.Lock()
			s.hintAt[key.String()] = time.Now()
			s.mu.Unlock()
		}
	}
	return err
}

// observeHintLead records how far ahead of the first real request a hint
// started the wake.
func (s *Server) observeHintLead(key string) {
	s.mu.Lock()
	at, ok := s.hintAt[key]
	delete(s.hintAt, key)
	s.mu.Unlock()
	if ok {
		s.metrics.HintLead.Observe(time.Since(at).Seconds())
	}
}

// --- handlers ---

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req sbxapi.CreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.RepoURL == "" {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, "repoURL is required")
		return
	}
	ns, name := req.Namespace, req.Name
	if ns == "" {
		ns = s.cfg.WorkspaceNamespace
	}
	if name == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		name = "ws-" + hex.EncodeToString(b)
	}
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: sbxv1.WorkspaceSpec{
			Source: sbxv1.Source{RepoURL: req.RepoURL, Ref: req.Ref, Depth: req.Depth},
			Image:  req.Image, Mode: sbxv1.Mode(req.Mode),
		},
	}
	for _, svc := range req.Services {
		ws.Spec.Services = append(ws.Spec.Services, sbxv1.ServiceSpec{
			Name: svc.Name, Command: svc.Command, Cwd: svc.Cwd, Env: svc.Env, WaitForDeps: svc.WaitForDeps,
		})
	}
	if p := req.Policy; p != nil {
		if _, err := policy.New(p.Name, p.Params); err != nil {
			writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, "policy: "+err.Error())
			return
		}
		ws.Spec.Policy = &sbxv1.PolicyRef{Name: p.Name, Params: p.Params}
	}
	if err := s.client.Create(r.Context(), ws); err != nil {
		code := http.StatusBadRequest
		if apierrors.IsAlreadyExists(err) {
			code = http.StatusConflict
		}
		writeErr(w, code, sbxapi.CodeInvalid, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sbxapi.CreateResponse{
		Workspace: View(ws), Token: s.cfg.Keys.Derive(token.Gateway, string(ws.UID), 0),
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	list := &sbxv1.WorkspaceList{}
	if err := s.cache.List(r.Context(), list); err != nil {
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return
	}
	out := make([]sbxapi.Workspace, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, View(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, _ *http.Request, ws *sbxv1.Workspace) {
	writeJSON(w, http.StatusOK, View(ws))
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	if err := s.client.Delete(r.Context(), ws); client.IgnoreNotFound(err) != nil {
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) token(w http.ResponseWriter, _ *http.Request, ws *sbxv1.Workspace) {
	writeJSON(w, http.StatusOK, sbxapi.TokenResponse{Token: s.cfg.Keys.Derive(token.Gateway, string(ws.UID), 0)})
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	var req sbxapi.ExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || len(req.Command) == 0 {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, "command is required")
		return
	}
	for _, v := range req.WaitFor {
		if v != waitDeps && v != waitNone {
			writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, `waitFor takes "deps" or "none"`)
			return
		}
	}
	key := client.ObjectKeyFromObject(ws).String()
	s.signals.Touch(key)
	ctx := r.Context()
	cur, waited, err := s.awake(ctx, ws)
	if err != nil {
		s.wakeErr(w, err)
		return
	}
	var depsWaited time.Duration
	run := func() (*processv1.ExecuteResponse, bool, error) {
		if needsDeps(cur, req) {
			var d time.Duration
			if cur, d, err = s.awaitDeps(ctx, cur); err != nil {
				return nil, false, err
			}
			depsWaited += d
		}
		return s.execute(ctx, cur, req)
	}
	resp, frozen, err := run()
	if frozen {
		// A suspend froze the workload after we checked; nothing ran. Our
		// wake request aborts the suspend (or wakes it right after).
		var more time.Duration
		if cur, more, err = s.awakeAfterFreeze(ctx, cur); err != nil {
			s.wakeErr(w, err)
			return
		}
		waited += more
		resp, frozen, err = run()
	}
	if errors.Is(err, errDepsTimeout) || errors.Is(err, ErrQueueFull) || errors.Is(err, context.Canceled) {
		s.wakeErr(w, err)
		return
	}
	if err != nil || frozen {
		if err == nil {
			err = errors.New("the workspace is still frozen")
		}
		writeErr(w, http.StatusServiceUnavailable, sbxapi.CodeUnavailable, err.Error())
		return
	}
	s.signals.Touch(key)
	writeJSON(w, http.StatusOK, sbxapi.ExecResponse{
		ExitCode: resp.GetExitCode(), Stdout: string(resp.GetStdout()), Stderr: string(resp.GetStderr()),
		WaitedMillis: waited.Milliseconds(), DepsWaitedMillis: depsWaited.Milliseconds(),
	})
}

// ExecRequest.WaitFor values.
const (
	waitDeps = "deps"
	waitNone = "none"
)

var errDepsTimeout = errors.New("gateway: dependencies weren't restored in time")

// needsDeps reports whether a command must wait for the dependency
// directory: it asks to, or it runs the package manager or the runtime the
// workspace's profile installs for. Before the profile is known, any
// profile's commands count.
func needsDeps(ws *sbxv1.Workspace, req sbxapi.ExecRequest) bool {
	switch {
	case !sbxv1.DepsPending(ws), slices.Contains(req.WaitFor, waitNone):
		return false
	case slices.Contains(req.WaitFor, waitDeps):
		return true
	}
	if d := ws.Status.Deps; d != nil && d.Profile != "" {
		p, ok := depcache.Lookup(d.Profile)
		return ok && p.NeedsDeps(req.Command)
	}
	return slices.ContainsFunc(depcache.Profiles, func(p depcache.Profile) bool { return p.NeedsDeps(req.Command) })
}

// awaitDeps holds a command until ws's dependency restore settles (ready
// or failed: a failed restore lets the command run and report the error).
func (s *Server) awaitDeps(ctx context.Context, ws *sbxv1.Workspace) (*sbxv1.Workspace, time.Duration, error) {
	key := client.ObjectKeyFromObject(ws)
	start := time.Now()
	cur := ws
	err := s.holder.WaitFor(ctx, key.String(), s.cfg.DepsTimeout, func() (bool, error) {
		fresh := &sbxv1.Workspace{}
		if err := s.cache.Get(ctx, key, fresh); err != nil {
			return false, err
		}
		cur = fresh
		return !sbxv1.DepsPending(fresh), nil
	})
	if errors.Is(err, ErrWakeTimeout) {
		err = errDepsTimeout
	}
	return cur, time.Since(start), err
}

// awakeAfterFreeze asks for a wake (the API, not our cache, knows a suspend
// started) and holds until the workspace serves again.
func (s *Server) awakeAfterFreeze(ctx context.Context, ws *sbxv1.Workspace) (*sbxv1.Workspace, time.Duration, error) {
	key := client.ObjectKeyFromObject(ws)
	if err := s.requestWake(ctx, key, sbxv1.WakeTriggerRequest); err != nil {
		return nil, 0, err
	}
	frozenRV := ws.ResourceVersion
	start := time.Now()
	cur := ws
	err := s.holder.Wait(ctx, key.String(), func() (bool, error) {
		fresh := &sbxv1.Workspace{}
		if err := s.cache.Get(ctx, key, fresh); err != nil {
			return false, err
		}
		cur = fresh
		// Wait for a newer version than the one we saw frozen.
		return fresh.ResourceVersion != frozenRV && Ready(fresh), nil
	})
	return cur, time.Since(start), err
}

func (s *Server) wake(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	if err := s.requestWake(r.Context(), client.ObjectKeyFromObject(ws), sbxv1.WakeTriggerAPI); err != nil {
		s.wakeErr(w, err)
		return
	}
	cur, _, err := s.awake(r.Context(), ws)
	if err != nil {
		s.wakeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, View(cur))
}

// wakeHint starts a wake without waiting: a user message arrived and the
// agent will soon need the workspace. Hints are best effort.
func (s *Server) wakeHint(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	key := client.ObjectKeyFromObject(ws)
	s.signals.TurnStarted(key.String())
	if !NeedsWake(ws) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.hints.Allow() {
		w.WriteHeader(http.StatusAccepted) // dropped under load; the real request will wake it
		return
	}
	if err := s.requestWake(r.Context(), key, sbxv1.WakeTriggerHint); err != nil && !errors.Is(err, ErrOff) {
		s.wakeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) turnEnded(w http.ResponseWriter, _ *http.Request, ws *sbxv1.Workspace) {
	s.signals.TurnEnded(client.ObjectKeyFromObject(ws).String())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) suspend(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	patch := []byte(`{"metadata":{"annotations":{"` + sbxv1.AnnotationSuspendRequested + `":"api"}}}`)
	if err := s.client.Patch(r.Context(), ws, client.RawPatch(types.MergePatchType, patch)); err != nil {
		writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) files(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
	default:
		writeErr(w, http.StatusMethodNotAllowed, sbxapi.CodeInvalid, "method not allowed")
		return
	}
	var body []byte
	if r.Method == http.MethodPut {
		b, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBufferedBody+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, err.Error())
			return
		}
		if int64(len(b)) > s.cfg.MaxBufferedBody {
			writeErr(w, http.StatusRequestEntityTooLarge, sbxapi.CodeInvalid, "file too large for this API")
			return
		}
		body = b
	}
	key := client.ObjectKeyFromObject(ws).String()
	s.signals.Touch(key)
	ctx := r.Context()
	cur, _, err := s.awake(ctx, ws)
	if err != nil {
		s.wakeErr(w, err)
		return
	}
	path := r.PathValue("path")
	resp, frozen, err := s.forwardFile(ctx, cur, r.Method, path, r.URL.RawQuery, r.Header.Get("Content-Type"), body)
	if frozen {
		if cur, _, err = s.awakeAfterFreeze(ctx, cur); err != nil {
			s.wakeErr(w, err)
			return
		}
		resp, frozen, err = s.forwardFile(ctx, cur, r.Method, path, r.URL.RawQuery, r.Header.Get("Content-Type"), body)
	}
	if err != nil || frozen {
		msg := "the workspace is still frozen"
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, http.StatusServiceUnavailable, sbxapi.CodeUnavailable, msg)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	out := make([]sbxapi.Service, 0, len(ws.Spec.Services))
	var running map[string]*agentv1.ServiceStatus
	if Ready(ws) { // never wake for a status read
		if st, err := s.agentServices(r.Context(), ws); err == nil {
			running = map[string]*agentv1.ServiceStatus{}
			for _, x := range st {
				running[x.GetService().GetName()] = x
			}
		}
	}
	for _, sp := range ws.Spec.Services {
		svc := sbxapi.Service{Name: sp.Name, Command: sp.Command, Cwd: sp.Cwd, Env: sp.Env, WaitForDeps: sp.WaitForDeps}
		if x := running[sp.Name]; x != nil {
			svc.Running, svc.Pid, svc.Restarts = x.GetRunning(), x.GetPid(), x.GetRestarts()
			svc.LastExitCode, svc.Output = x.GetLastExitCode(), x.GetOutputTail()
		}
		out = append(out, svc)
	}
	writeJSON(w, http.StatusOK, out)
}

// putService declares a service. The controller starts it (now if the
// workspace is awake, and after every wake); it doesn't wake a sleeping one.
func (s *Server) putService(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	var svc sbxapi.Service
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&svc); err != nil || len(svc.Command) == 0 {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, "command is required")
		return
	}
	spec := sbxv1.ServiceSpec{Name: r.PathValue("svc"), Command: svc.Command, Cwd: svc.Cwd, Env: svc.Env, WaitForDeps: svc.WaitForDeps}
	err := s.patchServices(r.Context(), ws, func(list []sbxv1.ServiceSpec) []sbxv1.ServiceSpec {
		if i := slices.IndexFunc(list, func(x sbxv1.ServiceSpec) bool { return x.Name == spec.Name }); i >= 0 {
			list[i] = spec
			return list
		}
		return append(list, spec)
	})
	if err != nil {
		s.patchErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request, ws *sbxv1.Workspace) {
	name := r.PathValue("svc")
	err := s.patchServices(r.Context(), ws, func(list []sbxv1.ServiceSpec) []sbxv1.ServiceSpec {
		return slices.DeleteFunc(list, func(x sbxv1.ServiceSpec) bool { return x.Name == name })
	})
	if err != nil {
		s.patchErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchServices rewrites spec.services with a resourceVersion
// precondition, retrying on conflicts.
func (s *Server) patchServices(ctx context.Context, ws *sbxv1.Workspace, edit func([]sbxv1.ServiceSpec) []sbxv1.ServiceSpec) error {
	key := client.ObjectKeyFromObject(ws)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &sbxv1.Workspace{}
		if err := s.waker.API.Get(ctx, key, cur); err != nil {
			return err
		}
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": cur.ResourceVersion},
			"spec":     map[string]any{"services": edit(slices.Clone(cur.Spec.Services))},
		})
		if err != nil {
			return err
		}
		return s.client.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch))
	})
}

func (s *Server) patchErr(w http.ResponseWriter, err error) {
	if apierrors.IsInvalid(err) {
		writeErr(w, http.StatusBadRequest, sbxapi.CodeInvalid, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, sbxapi.CodeInternal, err.Error())
}

// View is the public view of a workspace.
func View(ws *sbxv1.Workspace) sbxapi.Workspace {
	v := sbxapi.Workspace{
		Namespace: ws.Namespace, Name: ws.Name, UID: string(ws.UID),
		RepoURL: ws.Spec.Source.RepoURL, Ref: ws.Spec.Source.Ref, Image: ws.Status.ResolvedImage, ImageSource: ws.Status.ImageSource,
		Mode: string(ws.Spec.Mode), Phase: string(ws.Status.Phase), Epoch: ws.Status.Epoch,
		CreatedAt: ws.CreationTimestamp.Time, Ready: Ready(ws),
	}
	if v.Image == "" {
		v.Image = ws.Spec.Image
	}
	if c := meta.FindStatusCondition(ws.Status.Conditions, sbxv1.ConditionReady); c != nil {
		v.Message = c.Message
	}
	for _, c := range ws.Status.Conditions {
		v.Conditions = append(v.Conditions, sbxapi.Condition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message})
	}
	if ws.Status.Pod != nil {
		v.Node = ws.Status.Pod.NodeName
	}
	if s := ws.Status.LastSnapshot; s != nil {
		v.LastSnapshot = &sbxapi.Snapshot{Seq: s.Seq, Bytes: s.Bytes, CreatedAt: timePtr(s.CreatedAt)}
	}
	if p := ws.Spec.Policy; p != nil {
		v.Policy = &sbxapi.PolicyRef{Name: p.Name, Params: p.Params}
	}
	if c := meta.FindStatusCondition(ws.Status.Conditions, sbxv1.ConditionDepsReady); c != nil && ws.Status.Phase == sbxv1.PhaseRunning {
		v.Deps = &sbxapi.Deps{Pending: sbxv1.DepsPending(ws), Ready: c.Status == metav1.ConditionTrue, Message: c.Message}
		if d := ws.Status.Deps; d != nil {
			v.Deps.Profile, v.Deps.Source = d.Profile, d.Source
		}
	}
	if sl := ws.Status.Sleep; sl != nil {
		v.Sleep = &sbxapi.Sleep{Policy: sl.Policy, Reason: sl.Reason, Message: sl.Message, SuspendAt: timePtr(sl.SuspendAt)}
	}
	if t := ws.Status.Timeline; t != nil {
		v.Timeline = &sbxapi.Timeline{
			Trigger: string(t.Trigger), RequestedAt: microPtr(t.RequestedAt), PodScheduledAt: microPtr(t.PodScheduledAt),
			FilesReadyAt: microPtr(t.FilesReadyAt), ReadyAt: microPtr(t.ReadyAt), DepsReadyAt: microPtr(t.DepsReadyAt),
		}
	}
	return v
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}

func microPtr(t *metav1.MicroTime) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}
