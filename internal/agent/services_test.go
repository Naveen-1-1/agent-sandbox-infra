package agent_test

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
)

func service(name string, argv ...string) *agentv1.Service {
	return &agentv1.Service{Name: name, Command: argv}
}

// waitService polls until ok holds for the named service.
func waitService(t *testing.T, h *harness, name string, ok func(*agentv1.ServiceStatus) bool) *agentv1.ServiceStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := h.agent.ListServices(authed(t), &agentv1.ListServicesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range resp.GetServices() {
			if s.GetService().GetName() == name && ok(s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("service %s never got there: %v", name, resp.GetServices())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServiceRunsAndIsRestartedWhenItDies(t *testing.T) {
	h := start(t)
	if _, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: service("web", "sh", "-c", "echo listening; exec sleep 60")}); err != nil {
		t.Fatal(err)
	}
	first := waitService(t, h, "web", func(s *agentv1.ServiceStatus) bool {
		return s.GetRunning() && strings.Contains(s.GetOutputTail(), "listening")
	})
	sig, err := h.agent.GetSignals(authed(t), &agentv1.GetSignalsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cmds := sig.GetSignals().GetRunningCommands()
	if len(cmds) != 1 || cmds[0].GetKind() != signalsv1.CommandKind_COMMAND_KIND_SERVICE {
		t.Fatalf("running commands = %v, want the service", cmds)
	}

	if err := syscall.Kill(int(first.GetPid()), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	again := waitService(t, h, "web", func(s *agentv1.ServiceStatus) bool {
		return s.GetRunning() && s.GetRestarts() == 1
	})
	if again.GetPid() == first.GetPid() || again.GetLastExitCode() == 0 {
		t.Fatalf("after a crash: %v", again)
	}
}

func TestServiceStartIsIdempotentAndASpecChangeRestartsIt(t *testing.T) {
	h := start(t)
	spec := service("worker", "sleep", "60")
	for range 2 {
		if _, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: spec}); err != nil {
			t.Fatal(err)
		}
	}
	first := waitService(t, h, "worker", (*agentv1.ServiceStatus).GetRunning)
	if _, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: spec}); err != nil {
		t.Fatal(err)
	}
	if same := waitService(t, h, "worker", (*agentv1.ServiceStatus).GetRunning); same.GetPid() != first.GetPid() {
		t.Fatal("starting the same spec again restarted it")
	}

	changed := service("worker", "sleep", "61")
	if _, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: changed}); err != nil {
		t.Fatal(err)
	}
	next := waitService(t, h, "worker", func(s *agentv1.ServiceStatus) bool {
		return s.GetRunning() && s.GetService().GetCommand()[1] == "61"
	})
	if next.GetPid() == first.GetPid() || syscall.Kill(int(first.GetPid()), 0) == nil {
		t.Fatal("the old process survived a spec change")
	}

	if _, err := h.agent.StopService(authed(t), &agentv1.StopServiceRequest{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(int(next.GetPid()), 0) == nil {
		t.Fatal("the process survived StopService")
	}
	resp, _ := h.agent.ListServices(authed(t), &agentv1.ListServicesRequest{})
	if len(resp.GetServices()) != 0 {
		t.Fatalf("services after stop: %v", resp.GetServices())
	}
}

func TestServiceSpecsAreChecked(t *testing.T) {
	h := start(t)
	for name, spec := range map[string]*agentv1.Service{
		"bad name":      service("Web Server", "true"),
		"no command":    service("web"),
		"cwd outside":   {Name: "web", Command: []string{"true"}, Cwd: "../.."},
		"absolute away": {Name: "web", Command: []string{"true"}, Cwd: "/etc"},
	} {
		if _, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: spec}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServicesDontStartWhileFrozen(t *testing.T) {
	h := start(t)
	if _, err := h.agent.Freeze(authed(t), &agentv1.FreezeRequest{AttemptId: "a1"}); err != nil {
		t.Fatal(err)
	}
	var trailer metadata.MD
	_, err := h.agent.StartService(authed(t), &agentv1.StartServiceRequest{Service: service("web", "sleep", "1")}, grpc.Trailer(&trailer))
	if err == nil || len(trailer.Get(agent.FrozenTrailer)) == 0 {
		t.Fatalf("StartService while frozen: %v (trailer %v)", err, trailer)
	}
}
