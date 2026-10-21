// Command sbx-sidecar runs in every workspace pod:
//
//	sbx-sidecar install-tools DIR   copy the sbx runtime (agent, editor) into DIR
//	sbx-sidecar restore             init container: restore this epoch's files
//	sbx-sidecar serve               native sidecar: snapshots and progress
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sidecar"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sbx-sidecar install-tools DIR | restore | serve")
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "sbx-sidecar", "mode", os.Args[1])
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var err error
	switch os.Args[1] {
	case "install-tools":
		if len(os.Args) != 3 {
			err = errors.New("usage: sbx-sidecar install-tools DIR")
			break
		}
		err = installTools(os.Args[2])
	case "restore":
		err = restore(ctx, log)
	case "serve":
		err = serve(ctx, log)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Error("failed", "err", snapshot.Redact(err.Error()))
		os.Exit(1)
	}
}

func config(log *slog.Logger) (sidecar.Config, error) {
	epoch, err := strconv.ParseInt(os.Getenv("SBX_EPOCH"), 10, 64)
	if err != nil {
		return sidecar.Config{}, fmt.Errorf("SBX_EPOCH: %w", err)
	}
	tokens := envOr("SBX_TOKEN_DIR", "/var/run/sbx/tokens")
	readToken := func(name string) string {
		b, err := os.ReadFile(filepath.Join(tokens, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	cfg := sidecar.Config{
		Workspace:    os.Getenv("SBX_WORKSPACE"),
		WorkspaceUID: os.Getenv("SBX_WORKSPACE_UID"),
		Epoch:        epoch,
		PodUID:       os.Getenv("SBX_POD_UID"),
		Root:         envOr("SBX_WORKSPACE_DIR", "/workspace"),
		Scratch:      envOr("SBX_SCRATCH_DIR", "/sbx-scratch"),
		BrokerAddr:   os.Getenv("SBX_BROKER_ADDR"),
		BrokerToken:  readToken("broker-token"),
		SidecarToken: readToken("sidecar-token"),
		ListenAddr:   envOr("SBX_SIDECAR_ADDR", ":9091"),
		Exclude:      splitList(os.Getenv("SBX_SNAPSHOT_EXCLUDE")),
		Limits: snapshot.Limits{
			MaxFileBytes:   envInt("SBX_SNAPSHOT_MAX_FILE_BYTES"),
			MaxNewBytes:    envInt("SBX_SNAPSHOT_MAX_NEW_BYTES"),
			MaxBundleBytes: envInt("SBX_SNAPSHOT_MAX_BUNDLE_BYTES"),
		},
		Installer:    sidecar.AgentInstaller{Addr: os.Getenv("SBX_AGENT_ADDR"), Token: readToken("agent-token")},
		DepsMaxBytes: envInt("SBX_DEPS_MAX_BYTES"),
		Log:          log,
	}
	for k, v := range map[string]string{
		"SBX_WORKSPACE": cfg.Workspace, "SBX_WORKSPACE_UID": cfg.WorkspaceUID, "SBX_BROKER_ADDR": cfg.BrokerAddr,
		"broker token": cfg.BrokerToken,
	} {
		if v == "" {
			return cfg, fmt.Errorf("%s is required", k)
		}
	}
	return cfg, nil
}

func dialBroker(cfg sidecar.Config) (brokerv1.BrokerServiceClient, func(), error) {
	conn, err := grpc.NewClient(cfg.BrokerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		broker.Credentials(cfg.BrokerToken, cfg.Workspace, cfg.Epoch))
	if err != nil {
		return nil, nil, err
	}
	return brokerv1.NewBrokerServiceClient(conn), func() { _ = conn.Close() }, nil
}

// restore retries while the broker is starting or hasn't seen this epoch
// yet, rather than failing the init container into kubelet's slow backoff.
func restore(ctx context.Context, log *slog.Logger) error {
	cfg, err := config(log)
	if err != nil {
		return err
	}
	client, closeFn, err := dialBroker(cfg)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	wait := 200 * time.Millisecond
	for {
		err := sidecar.Restore(ctx, cfg, client)
		if err == nil || !retryable(err) {
			return err
		}
		log.Warn("restore not possible yet; retrying", "err", snapshot.Redact(err.Error()), "in", wait)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}

func retryable(err error) bool {
	switch status.Code(errors.Unwrap(err)) {
	case codes.Unavailable, codes.PermissionDenied, codes.FailedPrecondition, codes.DeadlineExceeded:
		return true
	}
	return false
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, err := config(log)
	if err != nil {
		return err
	}
	if cfg.SidecarToken == "" {
		return errors.New("sidecar token is required")
	}
	client, closeFn, err := dialBroker(cfg)
	if err != nil {
		return err
	}
	defer closeFn()
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("sidecar serving", "addr", l.Addr().String(), "epoch", cfg.Epoch)
	return sidecar.NewServer(cfg, client).Serve(ctx, l)
}

// installTools copies the runtime tree this binary lives in (/sbx) to dst.
// Writes go through os.Root, so nothing can escape dst.
func installTools(dst string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	src := filepath.Dir(filepath.Dir(exe)) // /sbx/bin/sbx-sidecar -> /sbx
	//nolint:gosec // dst is the install target from our own pod spec
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return root.MkdirAll(rel, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = root.Remove(rel)
			return root.Symlink(target, rel)
		default:
			return copyFile(root, path, rel, info.Mode().Perm())
		}
	})
}

func copyFile(root *os.Root, from, to string, mode fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string) int64 {
	n, _ := strconv.ParseInt(os.Getenv(key), 10, 64)
	return n
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
