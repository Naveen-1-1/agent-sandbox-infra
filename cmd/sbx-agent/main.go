// Command sbx-agent is PID 1 of a workspace's workload container. It serves
// upstream sandboxd's exec and files APIs behind the freeze gate, reports
// activity, and reaps orphaned processes.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
)

func main() {
	if agent.ShouldSupervise() {
		os.Exit(agent.Supervise())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sbx-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg agent.Config
	flag.StringVar(&cfg.Root, "root", envOr("SBX_WORKSPACE_DIR", "/workspace"), "workspace directory")
	flag.StringVar(&cfg.GRPCAddr, "grpc-addr", envOr("SBX_AGENT_GRPC_ADDR", ":9090"), "gRPC listen address")
	flag.StringVar(&cfg.RESTAddr, "rest-addr", envOr("SBX_AGENT_REST_ADDR", ":8080"), "REST listen address")
	tokenHash := flag.String("token-sha256", os.Getenv("SBX_AGENT_TOKEN_SHA256"), "hex SHA-256 of the agent token")
	flag.Parse()

	if *tokenHash == "" {
		return fmt.Errorf("SBX_AGENT_TOKEN_SHA256 is required")
	}
	sum, err := hex.DecodeString(*tokenHash)
	if err != nil {
		return fmt.Errorf("SBX_AGENT_TOKEN_SHA256: %w", err)
	}
	cfg.TokenSHA256 = sum
	cfg.Log = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "sbx-agent")

	srv, err := agent.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return srv.Serve(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
