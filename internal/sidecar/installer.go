package sidecar

import (
	"context"
	"net"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// AgentInstaller runs installs through the workload's agent, over the
// pod's own network namespace, with this epoch's agent token.
type AgentInstaller struct {
	Addr  string // default 127.0.0.1:9090
	Token string
}

type agentToken string

func (t agentToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (agentToken) RequireTransportSecurity() bool { return false }

// Install runs command with sh in dir and returns its exit code and output.
func (a AgentInstaller) Install(ctx context.Context, command, dir string) (int, string, error) {
	addr := a.Addr
	if addr == "" {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(9090))
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(agentToken(a.Token)))
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	resp, err := processv1.NewProcessServiceClient(conn).Execute(ctx, &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{
		Command: []string{"sh", "-c", command},
		Cwd:     &dir,
		// Installers behave better (no spinners, no prompts) when told so.
		EnvVars: map[string]string{"CI": "true", "npm_config_update_notifier": "false"},
	}})
	if err != nil {
		return 0, "", err
	}
	return int(resp.GetExitCode()), string(resp.GetStdout()) + string(resp.GetStderr()), nil
}
