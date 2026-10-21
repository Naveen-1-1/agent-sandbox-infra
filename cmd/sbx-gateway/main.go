// Command sbx-gateway fronts every workspace: it authenticates requests,
// holds those for a sleeping workspace while it wakes, and forwards them to
// the workspace's pod. It serves the REST API, and each workspace's editor
// and app ports on hosts of their own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/gateway"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

func main() {
	var (
		addr, adminAddr, signalsAddr  string
		masterKeyFile, adminTokenFile string
		cfg                           gateway.Config
	)
	flag.StringVar(&addr, "addr", ":8080", "API listen address")
	flag.StringVar(&adminAddr, "admin-addr", ":8081", "health and metrics listen address (never wakes workspaces)")
	flag.StringVar(&signalsAddr, "signals-addr", ":9444", "GatewaySignals gRPC listen address")
	flag.StringVar(&masterKeyFile, "master-key-file", "/var/run/sbx/master-key/key", "file holding the token master key")
	flag.StringVar(&adminTokenFile, "admin-token-file", "/var/run/sbx/admin-token/token", "file holding the admin API token")
	flag.StringVar(&cfg.WorkspaceNamespace, "workspace-namespace", envOr("SBX_WORKSPACE_NAMESPACE", "sbx-workspaces"), "namespace for new workspaces")
	flag.IntVar(&cfg.HoldPerWorkspace, "hold-per-workspace", envInt("SBX_HOLD_PER_WORKSPACE", 256), "requests that may wait for one workspace")
	flag.IntVar(&cfg.HoldGlobal, "hold-global", envInt("SBX_HOLD_GLOBAL", 4096), "requests that may wait in total")
	flag.DurationVar(&cfg.HoldTimeout, "hold-timeout", envDuration("SBX_HOLD_TIMEOUT", 90*time.Second), "how long a request may wait for a wake")
	flag.DurationVar(&cfg.ExecTimeout, "exec-timeout", envDuration("SBX_EXEC_TIMEOUT", 10*time.Minute), "default command timeout")
	flag.DurationVar(&cfg.DepsTimeout, "deps-timeout", envDuration("SBX_DEPS_TIMEOUT", 15*time.Minute), "how long a command may wait for dependencies")
	flag.StringVar(&cfg.Domain, "domain", os.Getenv("SBX_DOMAIN"), "serve editors and app ports on <name>.<namespace>.<domain> (empty: off)")
	flag.StringVar(&cfg.PublicScheme, "public-scheme", envOr("SBX_PUBLIC_SCHEME", "http"), "scheme browsers use to reach the gateway")
	flag.IntVar(&cfg.PublicPort, "public-port", envInt("SBX_PUBLIC_PORT", 0), "port browsers use to reach the gateway (0: the scheme's default)")
	flag.DurationVar(&cfg.SessionTTL, "session-ttl", envDuration("SBX_SESSION_TTL", 12*time.Hour), "how long a browser stays signed in to a workspace")
	flag.StringVar(&cfg.PolicyNamespace, "policy-namespace", envOr("SBX_POLICY_NAMESPACE", os.Getenv("POD_NAMESPACE")), "namespace of the sleep policy ConfigMaps (the controller's)")
	flag.IntVar(&cfg.MaxEventStreams, "max-event-streams", envInt("SBX_MAX_EVENT_STREAMS", 64), "concurrent workspace event streams")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "sbx-gateway")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, cfg, addr, adminAddr, signalsAddr, masterKeyFile, adminTokenFile, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg gateway.Config, addr, adminAddr, signalsAddr, masterKeyFile, adminTokenFile string, log *slog.Logger) error {
	key, err := os.ReadFile(masterKeyFile)
	if err != nil {
		return fmt.Errorf("reading master key: %w", err)
	}
	if cfg.Keys, err = token.NewKeyring([]byte(strings.TrimSpace(string(key)))); err != nil {
		return err
	}
	admin, err := os.ReadFile(adminTokenFile)
	if err != nil {
		return fmt.Errorf("reading admin token: %w", err)
	}
	cfg.AdminToken = strings.TrimSpace(string(admin))

	if cfg.PolicyNamespace == "" {
		return errors.New("POD_NAMESPACE or --policy-namespace is required")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, sbxv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	rest := ctrl.GetConfigOrDie()
	kube, err := client.New(rest, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	wsCache, err := cache.New(rest, cache.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := gateway.New(cfg, wsCache, kube, kube, reg, log)
	defer srv.Close()

	inf, err := wsCache.GetInformer(ctx, &sbxv1.Workspace{})
	if err != nil {
		return err
	}
	keyOf := func(obj any) string {
		if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			obj = t.Obj
		}
		if o, ok := obj.(client.Object); ok {
			return client.ObjectKeyFromObject(o).String()
		}
		return ""
	}
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { srv.Notify(keyOf(obj)) },
		UpdateFunc: func(_, obj any) { srv.Notify(keyOf(obj)) },
		DeleteFunc: func(obj any) { srv.Forget(keyOf(obj)) },
	}); err != nil {
		return err
	}

	errc := make(chan error, 4)
	go func() { errc <- wsCache.Start(ctx) }()
	go func() {
		if wsCache.WaitForCacheSync(ctx) {
			srv.SetSynced()
			log.Info("workspace cache synced")
		}
	}()

	api := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	adminSrv := &http.Server{Addr: adminAddr, Handler: srv.AdminHandler(reg), ReadHeaderTimeout: 5 * time.Second}
	for _, s := range []*http.Server{api, adminSrv} {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	var lc net.ListenConfig
	sl, err := lc.Listen(ctx, "tcp", signalsAddr)
	if err != nil {
		return err
	}
	g := grpc.NewServer()
	gatewayv1.RegisterGatewaySignalsServiceServer(g, srv.Signals())
	go func() { errc <- g.Serve(sl) }()
	log.Info("gateway serving", "api", addr, "admin", adminAddr, "signals", signalsAddr)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdown)
	_ = adminSrv.Shutdown(shutdown)
	g.GracefulStop()
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
