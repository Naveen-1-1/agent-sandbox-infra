// Command sbx-bench measures the platform on real repositories: a cold
// first run, then suspend/wake cycles with and without a wake-hint. It
// talks to the gateway like an agent harness, and reads the controller's
// counters and the pods' usage through the Kubernetes API. Results go to
// a JSON file (see internal/bench).
//
//	sbx-bench --env kind [--config test/bench/repos.json] [--cycles 10] [--repos a,b]
//
// The gateway comes from --gateway/SBX_GATEWAY with the admin token in
// SBX_TOKEN; the cluster from $KUBECONFIG. Object storage, for wiping the
// caches first, from --s3-endpoint and SBX_S3_ACCESS_KEY/SBX_S3_SECRET_KEY.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/bench"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sbx-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		o                   bench.Options
		cfgPath, out, repos string
		gateway, ns, commit string
		s3                  objstore.S3Config
		timeout             time.Duration
	)
	flag.StringVar(&o.Env, "env", "kind", "where this runs, for the output name: kind or aws")
	flag.StringVar(&cfgPath, "config", "test/bench/repos.json", "repositories to measure")
	flag.StringVar(&repos, "repos", "", "comma-separated repository names to run (default: all)")
	flag.IntVar(&o.Cycles, "cycles", 10, "measured suspend/wake cycles per repository")
	flag.DurationVar(&o.Thinking, "thinking", 10*time.Second, "simulated LLM thinking after a wake-hint")
	flag.BoolVar(&o.Fresh, "fresh", true, "wipe each repository's cached base and dependency archives first")
	flag.BoolVar(&o.Keep, "keep", false, "keep the workspaces afterwards")
	flag.DurationVar(&timeout, "timeout", 50*time.Minute, "give up after")
	flag.StringVar(&out, "out", "", "output file (default notes/bench-<env>-<date>.json)")
	flag.StringVar(&gateway, "gateway", envOr("SBX_GATEWAY", "http://127.0.0.1:8080"), "gateway URL")
	flag.StringVar(&ns, "namespace", "sbx-system", "the platform's namespace")
	flag.StringVar(&commit, "platform-commit", "", "the platform's git commit, for the record")
	flag.StringVar(&s3.Endpoint, "s3-endpoint", "", "object storage endpoint for wiping caches (empty: don't wipe)")
	flag.StringVar(&s3.Bucket, "s3-bucket", "sbx", "object storage bucket")
	flag.StringVar(&s3.Region, "s3-region", "us-east-1", "object storage region")
	flag.BoolVar(&s3.PathStyle, "s3-path-style", true, "path-style object storage URLs")
	flag.Parse()

	cfg, err := bench.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Only(splitList(repos)); err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join("notes", fmt.Sprintf("bench-%s-%s.json", o.Env, time.Now().Format("2006-01-02")))
	}
	token := os.Getenv("SBX_TOKEN")
	if token == "" {
		return errors.New("SBX_TOKEN (the admin token) is required")
	}
	rest, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return err
	}

	r := &bench.Runner{
		Gateway: &sbxapi.Client{BaseURL: gateway, Token: token},
		Cluster: &bench.Kube{Client: kube, Namespace: ns},
		Out:     os.Stdout,
		Options: o,
	}
	if s3.Endpoint != "" {
		s3.AccessKey, s3.SecretKey = os.Getenv("SBX_S3_ACCESS_KEY"), os.Getenv("SBX_S3_SECRET_KEY")
		if r.Store, err = objstore.NewS3(s3); err != nil {
			return err
		}
	} else if o.Fresh {
		return errors.New("--fresh needs --s3-endpoint (or pass --fresh=false)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, runErr := r.Run(ctx, cfg)
	if res != nil {
		res.Platform.Commit = commit
		if err := write(out, res); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", out)
	}
	return runErr
}

func write(path string, res *bench.Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
