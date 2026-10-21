// Command sbx-controller runs the platform's control plane: the Workspace
// controller (on the elected leader) and the broker that presigns workspace
// objects for pods (on every replica).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/workspace"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

type options struct {
	brokerAddr, healthAddr, metricsAddr string
	masterKeyFile                       string
	s3                                  objstore.S3Config
	createBucket                        bool
	maxBundleBytes                      int64
	presignTTL                          time.Duration
	leaderElect                         bool
	allowPrivateRepos                   bool
	ws                                  workspace.Config
	storageSize, workspaceResources     string
	snapshotExclude                     string
	gatewaySignalsService               string
	opsMax, opsMinSuspend               int
}

func main() {
	var o options
	flag.StringVar(&o.brokerAddr, "broker-addr", ":9443", "broker gRPC listen address")
	flag.StringVar(&o.healthAddr, "health-addr", ":8081", "health probe listen address")
	flag.StringVar(&o.metricsAddr, "metrics-addr", ":8080", "Prometheus metrics listen address")
	flag.StringVar(&o.masterKeyFile, "master-key-file", "/var/run/sbx/master-key/key", "file holding the token master key")
	flag.BoolVar(&o.leaderElect, "leader-elect", envBool("SBX_LEADER_ELECT", true), "elect a leader so one replica runs the controller")

	flag.StringVar(&o.s3.Endpoint, "s3-endpoint", os.Getenv("SBX_S3_ENDPOINT"), "S3 endpoint URL (empty for AWS)")
	flag.StringVar(&o.s3.PresignEndpoint, "s3-presign-endpoint", os.Getenv("SBX_S3_PRESIGN_ENDPOINT"), "endpoint in presigned URLs (default: s3-endpoint)")
	flag.StringVar(&o.s3.Region, "s3-region", envOr("SBX_S3_REGION", "us-east-1"), "S3 region")
	flag.StringVar(&o.s3.Bucket, "s3-bucket", os.Getenv("SBX_S3_BUCKET"), "S3 bucket")
	flag.BoolVar(&o.s3.PathStyle, "s3-path-style", envBool("SBX_S3_PATH_STYLE", false), "use path-style S3 URLs")
	flag.BoolVar(&o.createBucket, "s3-create-bucket", envBool("SBX_S3_CREATE_BUCKET", false), "create the bucket at startup if missing")
	flag.Int64Var(&o.maxBundleBytes, "max-bundle-bytes", 500<<20, "largest snapshot bundle accepted")
	flag.Int64Var(&o.ws.Snapshot.MaxDepsBytes, "max-deps-bytes", envInt("SBX_MAX_DEPS_BYTES", 2<<30), "largest dependency archive cached")
	flag.DurationVar(&o.presignTTL, "presign-ttl", 15*time.Minute, "lifetime of presigned URLs")

	flag.StringVar(&o.ws.RuntimeImage, "runtime-image", os.Getenv("SBX_RUNTIME_IMAGE"), "sbx-runtime image for workspace pods")
	flag.StringVar(&o.ws.BrokerAddr, "pod-broker-addr", os.Getenv("SBX_BROKER_ADDR"), "broker address as workspace pods reach it")
	flag.StringVar((*string)(&o.ws.Tools), "tools", envOr("SBX_TOOLS_MODE", string(podspec.ToolsImage)), `how workspaces get the agent and editor: "image" (an image volume, Kubernetes 1.35+) or "copy"`)
	flag.StringVar(&o.ws.DefaultImage, "default-image", envOr("SBX_DEFAULT_IMAGE", "mcr.microsoft.com/devcontainers/base:trixie"), "workload image when none is chosen")
	flag.Int64Var(&o.ws.RunAsUser, "run-as-user", envInt("SBX_RUN_AS_USER", 1000), "UID for workspace containers")
	flag.Int64Var(&o.ws.RunAsGroup, "run-as-group", envInt("SBX_RUN_AS_GROUP", 1000), "GID for workspace containers")
	flag.StringVar(&o.storageSize, "storage-size", envOr("SBX_STORAGE_SIZE", "5Gi"), "workspace disk size")
	flag.StringVar(&o.ws.StorageClassName, "storage-class", os.Getenv("SBX_STORAGE_CLASS"), "workspace disk storage class (empty = default)")
	flag.StringVar(&o.workspaceResources, "workspace-resources", os.Getenv("SBX_WORKSPACE_RESOURCES"), "workload container resources as JSON")
	flag.StringVar(&o.snapshotExclude, "snapshot-exclude", envOr("SBX_SNAPSHOT_EXCLUDE", "node_modules,.venv"), "comma-separated paths never snapshotted")
	flag.Int64Var(&o.ws.Snapshot.MaxFileBytes, "snapshot-max-file-bytes", envInt("SBX_SNAPSHOT_MAX_FILE_BYTES", 50<<20), "largest new file in a snapshot")
	flag.Int64Var(&o.ws.Snapshot.MaxNewBytes, "snapshot-max-new-bytes", envInt("SBX_SNAPSHOT_MAX_NEW_BYTES", 500<<20), "largest total new content in a snapshot")
	flag.BoolVar(&o.allowPrivateRepos, "allow-private-repos", envBool("SBX_ALLOW_PRIVATE_REPOS", false), "allow source repositories on private addresses")
	flag.IntVar(&o.ws.MaxConcurrent, "max-concurrent-reconciles", int(envInt("SBX_MAX_CONCURRENT_RECONCILES", 16)), "parallel workspace reconciles")
	flag.StringVar(&o.gatewaySignalsService, "gateway-signals-service", os.Getenv("SBX_GATEWAY_SIGNALS_SERVICE"), "headless Service of the gateway replicas' signals port (empty: ignore gateway signals)")
	flag.IntVar(&o.opsMax, "ops-max-concurrent", int(envInt("SBX_OPS_MAX_CONCURRENT", 8)), "wakes and suspends in progress at once")
	flag.IntVar(&o.opsMinSuspend, "ops-min-suspend", int(envInt("SBX_OPS_MIN_SUSPEND", 2)), "slots kept for suspends while wakes wait")
	flag.Parse()
	o.s3.AccessKey, o.s3.SecretKey = os.Getenv("SBX_S3_ACCESS_KEY"), os.Getenv("SBX_S3_SECRET_KEY")
	o.ws.PullPolicy = corev1.PullPolicy(envOr("SBX_PULL_POLICY", string(corev1.PullIfNotPresent)))

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "sbx-controller")
	ctrllog.SetLogger(logr.FromSlogHandler(log.Handler()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, o, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, log *slog.Logger) error {
	key, err := os.ReadFile(o.masterKeyFile)
	if err != nil {
		return fmt.Errorf("reading master key: %w", err)
	}
	keys, err := token.NewKeyring([]byte(strings.TrimSpace(string(key))))
	if err != nil {
		return err
	}
	if o.ws.StorageSize, err = resource.ParseQuantity(o.storageSize); err != nil {
		return fmt.Errorf("storage size: %w", err)
	}
	if o.workspaceResources != "" {
		if err := json.Unmarshal([]byte(o.workspaceResources), &o.ws.Resources); err != nil {
			return fmt.Errorf("workspace resources: %w", err)
		}
	}
	o.ws.Snapshot.Exclude = splitList(o.snapshotExclude)
	o.ws.Snapshot.MaxBundleBytes = o.maxBundleBytes
	if o.ws.RuntimeImage == "" || o.ws.BrokerAddr == "" {
		return errors.New("--runtime-image and --pod-broker-addr are required")
	}

	store, err := objstore.NewS3(o.s3)
	if err != nil {
		return err
	}
	if o.createBucket {
		if err := store.EnsureBucket(ctx); err != nil {
			return fmt.Errorf("creating bucket %s: %w", o.s3.Bucket, err)
		}
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, sbv1.AddToScheme, sbxv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return errors.New("POD_NAMESPACE is required")
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// The only ConfigMaps read are the policy's, in our own namespace.
		Cache:                         cache.Options{ByObject: map[client.Object]cache.ByObject{&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{ns: {}}}}},
		Metrics:                       metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:        o.healthAddr,
		LeaderElection:                o.leaderElect,
		LeaderElectionID:              "sbx-controller.sbx.dev",
		LeaderElectionNamespace:       ns,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	wsMetrics := metrics.NewWorkspace(ctrlmetrics.Registry, func(ctx context.Context) (map[string]int, error) {
		list := &sbxv1.WorkspaceList{}
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, ws := range list.Items {
			counts[string(ws.Status.Phase)]++
		}
		return counts, nil
	})
	limiter := &ops.Limiter{Max: o.opsMax, MinSuspend: o.opsMinSuspend}
	r := &workspace.Reconciler{
		Client: mgr.GetClient(), API: mgr.GetAPIReader(), Scheme: scheme, Keys: keys,
		Store: snapstore.New(store), Objects: store, Pods: workspace.GRPCPods{Keys: keys},
		Inspector: workspace.GitInspector{AllowPrivateHosts: o.allowPrivateRepos},
		Metrics:   wsMetrics, Snapshots: metrics.NewSnapshot(ctrlmetrics.Registry),
		Config:   o.ws,
		Policies: &sleep.Policies{Client: mgr.GetClient(), Namespace: ns, Recorder: mgr.GetEventRecorder("sbx-controller")},
		Ops:      limiter,
		Sleep:    metrics.NewSleep(ctrlmetrics.Registry, limiter.Counts, ops.ClassNames),
	}
	if o.gatewaySignalsService != "" {
		wakeups := make(chan event.GenericEvent, 1024)
		col := &sleep.Collector{
			Reader: mgr.GetAPIReader(), Namespace: ns, Service: o.gatewaySignalsService,
			Log: ctrl.Log.WithName("gateway-signals"),
			Changed: func(keys []string) {
				for _, k := range keys {
					wsNS, name, _ := strings.Cut(k, "/")
					select {
					case wakeups <- event.GenericEvent{Object: &sbxv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: wsNS, Name: name}}}:
					default: // full: the periodic recheck catches it
					}
				}
			},
		}
		if err := mgr.Add(col); err != nil {
			return err
		}
		r.Gateways, r.Wakeups = col, wakeups
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}

	b := broker.New(broker.Config{
		Keyring: keys, Store: store, Workspaces: broker.KubeWorkspaces{Reader: mgr.GetAPIReader()},
		PresignTTL: o.presignTTL, MaxBundleBytes: o.maxBundleBytes, MaxDepsBytes: o.ws.Snapshot.MaxDepsBytes,
	})
	if err := mgr.Add(&brokerServer{addr: o.brokerAddr, b: b, log: log}); err != nil {
		return err
	}
	log.Info("starting", "broker", o.brokerAddr, "bucket", o.s3.Bucket, "runtimeImage", o.ws.RuntimeImage)
	return mgr.Start(ctx)
}

// brokerServer runs the broker on every replica, leader or not.
type brokerServer struct {
	addr string
	b    *broker.Server
	log  *slog.Logger
}

func (s *brokerServer) NeedLeaderElection() bool { return false }

func (s *brokerServer) Start(ctx context.Context) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(s.b.UnaryInterceptor()))
	s.b.Register(g)
	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()
	if err := g.Serve(l); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil {
		return v
	}
	return def
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
