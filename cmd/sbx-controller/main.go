// Command sbx-controller runs the platform's control plane. Today it serves
// the broker that presigns workspace objects for pods; the Workspace
// reconciler joins it next.
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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

type options struct {
	brokerAddr, healthAddr, metricsAddr string
	masterKeyFile                       string
	s3                                  objstore.S3Config
	createBucket                        bool
	maxBundleBytes                      int64
	presignTTL                          time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.brokerAddr, "broker-addr", ":9443", "broker gRPC listen address")
	flag.StringVar(&o.healthAddr, "health-addr", ":8081", "health probe listen address")
	flag.StringVar(&o.metricsAddr, "metrics-addr", ":8080", "Prometheus metrics listen address")
	flag.StringVar(&o.masterKeyFile, "master-key-file", "/var/run/sbx/master-key/key", "file holding the token master key")
	flag.StringVar(&o.s3.Endpoint, "s3-endpoint", os.Getenv("SBX_S3_ENDPOINT"), "S3 endpoint URL (empty for AWS)")
	flag.StringVar(&o.s3.PresignEndpoint, "s3-presign-endpoint", os.Getenv("SBX_S3_PRESIGN_ENDPOINT"), "endpoint used in presigned URLs (default: s3-endpoint)")
	flag.StringVar(&o.s3.Region, "s3-region", envOr("SBX_S3_REGION", "us-east-1"), "S3 region")
	flag.StringVar(&o.s3.Bucket, "s3-bucket", os.Getenv("SBX_S3_BUCKET"), "S3 bucket")
	flag.BoolVar(&o.s3.PathStyle, "s3-path-style", os.Getenv("SBX_S3_PATH_STYLE") == "true", "use path-style S3 URLs")
	flag.BoolVar(&o.createBucket, "s3-create-bucket", os.Getenv("SBX_S3_CREATE_BUCKET") == "true", "create the bucket at startup if missing")
	flag.Int64Var(&o.maxBundleBytes, "max-bundle-bytes", 500<<20, "largest snapshot bundle accepted")
	flag.DurationVar(&o.presignTTL, "presign-ttl", 15*time.Minute, "lifetime of presigned URLs")
	flag.Parse()
	o.s3.AccessKey, o.s3.SecretKey = os.Getenv("SBX_S3_ACCESS_KEY"), os.Getenv("SBX_S3_SECRET_KEY")

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "sbx-controller")
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
	if err := sbxv1.AddToScheme(scheme); err != nil {
		return err
	}
	kube, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	b := broker.New(broker.Config{
		Keyring: keys, Store: store, Workspaces: broker.KubeWorkspaces{Reader: kube},
		PresignTTL: o.presignTTL, MaxBundleBytes: o.maxBundleBytes,
	})
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(b.UnaryInterceptor()))
	b.Register(g)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	var lc net.ListenConfig
	bl, err := lc.Listen(ctx, "tcp", o.brokerAddr)
	if err != nil {
		return err
	}
	health := &http.Server{Addr: o.healthAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })}
	metrics := &http.Server{Addr: o.metricsAddr, ReadHeaderTimeout: 5 * time.Second,
		Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{})}

	errc := make(chan error, 3)
	go func() { errc <- g.Serve(bl) }()
	for _, s := range []*http.Server{health, metrics} {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("controller serving", "broker", o.brokerAddr, "bucket", o.s3.Bucket)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g.GracefulStop()
	_ = health.Shutdown(shutdown)
	_ = metrics.Shutdown(shutdown)
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
