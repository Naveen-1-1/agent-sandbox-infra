package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
)

var validServiceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// How long a stopped service gets to exit on SIGTERM before SIGKILL, and
// the most a crashing service waits before its next restart.
const (
	stopGrace  = 10 * time.Second
	maxBackoff = time.Minute
)

// Services runs declared long-running processes, such as a dev server,
// each in its own process group, and restarts them with backoff when they
// exit. The pod is gone after a sleep; the controller declares them again
// on the next wake.
type Services struct {
	root     string
	activity *Activity
	log      *slog.Logger

	ops  sync.Mutex // serializes Start, Stop and Close
	mu   sync.Mutex // guards svcs
	svcs map[string]*svc
}

type svc struct {
	spec   *agentv1.Service
	out    *tailBuffer
	ctx    context.Context // cancelled to stop it
	cancel context.CancelFunc
	done   chan struct{} // closed when its supervisor has returned

	mu        sync.Mutex
	pid       int
	running   bool
	restarts  int
	lastExit  int
	startedAt time.Time
}

// NewServices returns an empty service table.
func NewServices(root string, a *Activity, log *slog.Logger) *Services {
	return &Services{root: root, activity: a, log: log, svcs: map[string]*svc{}}
}

// Start runs a service, or returns it if it already runs with the same
// spec. A changed spec restarts it.
func (m *Services) Start(spec *agentv1.Service) (*agentv1.ServiceStatus, error) {
	if !validServiceName.MatchString(spec.GetName()) {
		return nil, fmt.Errorf("invalid service name %q", spec.GetName())
	}
	if len(spec.GetCommand()) == 0 {
		return nil, errors.New("command is required")
	}
	dir, err := m.dir(spec.GetCwd())
	if err != nil {
		return nil, err
	}
	m.ops.Lock()
	defer m.ops.Unlock()
	m.mu.Lock()
	old := m.svcs[spec.GetName()]
	m.mu.Unlock()
	if old != nil && proto.Equal(old.spec, spec) {
		return old.status(), nil
	}
	if old != nil {
		old.halt()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &svc{spec: proto.Clone(spec).(*agentv1.Service), out: &tailBuffer{max: 8 << 10},
		ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.svcs[spec.GetName()] = s
	m.mu.Unlock()
	go m.supervise(s, dir)
	return s.status(), nil
}

// dir resolves a service's working directory inside the workspace.
func (m *Services) dir(cwd string) (string, error) {
	if cwd == "" {
		return m.root, nil
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(m.root, cwd)
	}
	cwd = filepath.Clean(cwd)
	if rel, err := filepath.Rel(m.root, cwd); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("cwd %q is outside the workspace", cwd)
	}
	return cwd, nil
}

// Stop ends a service and waits for it to exit.
func (m *Services) Stop(name string) error {
	m.ops.Lock()
	defer m.ops.Unlock()
	m.mu.Lock()
	s, ok := m.svcs[name]
	delete(m.svcs, name)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no service %q", name)
	}
	s.halt()
	return nil
}

// List reports every service, by name.
func (m *Services) List() []*agentv1.ServiceStatus {
	m.mu.Lock()
	all := make([]*svc, 0, len(m.svcs))
	for _, s := range m.svcs {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]*agentv1.ServiceStatus, 0, len(all))
	for _, s := range all {
		out = append(out, s.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetService().GetName() < out[j].GetService().GetName() })
	return out
}

// Close stops every service.
func (m *Services) Close() {
	m.ops.Lock()
	defer m.ops.Unlock()
	m.mu.Lock()
	all := m.svcs
	m.svcs = map[string]*svc{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Go(s.halt)
	}
	wg.Wait()
}

// supervise runs s until it is stopped, restarting it when it exits: at
// once after a long run, with doubling delays while it keeps crashing.
func (m *Services) supervise(s *svc, dir string) {
	defer close(s.done)
	backoff := time.Second
	for {
		began := time.Now()
		code, err := m.runOnce(s, dir)
		if err != nil {
			fmt.Fprintf(s.out, "\n[sbx] could not start: %v\n", err)
		}
		if s.ctx.Err() != nil {
			return
		}
		if time.Since(began) > maxBackoff {
			backoff = time.Second
		}
		m.log.Warn("service exited; restarting", "service", s.spec.GetName(), "code", code, "in", backoff)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
	}
}

func (m *Services) runOnce(s *svc, dir string) (int, error) {
	argv := s.spec.GetCommand()
	cmd := exec.CommandContext(s.ctx, argv[0], argv[1:]...) //nolint:gosec // running declared commands is the point
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range s.spec.GetEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = s.out, s.out
	// Its own process group, so stopping it stops its children too: SIGTERM
	// first, SIGKILL if it is still there after the grace period.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	pid := cmd.Process.Pid
	done := m.activity.StartCommand(strings.Join(argv, " "), signalsv1.CommandKind_COMMAND_KIND_SERVICE)
	s.mu.Lock()
	s.pid, s.running, s.startedAt = pid, true, time.Now()
	s.mu.Unlock()

	err := cmd.Wait()
	_ = syscall.Kill(-pid, syscall.SIGKILL) // whatever it left behind in its group
	done()
	code := cmd.ProcessState.ExitCode()
	s.mu.Lock()
	s.running, s.lastExit = false, code
	s.mu.Unlock()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && s.ctx.Err() == nil {
		return code, err
	}
	return code, nil
}

func (s *svc) halt() {
	s.cancel()
	<-s.done
}

func (s *svc) status() *agentv1.ServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &agentv1.ServiceStatus{
		Service: proto.Clone(s.spec).(*agentv1.Service), Running: s.running,
		Restarts: int32(s.restarts), LastExitCode: int32(s.lastExit), //nolint:gosec // small numbers
		OutputTail: s.out.String(),
	}
	if s.running {
		st.Pid = int32(s.pid) //nolint:gosec // pids fit
		st.StartedAt = timestamppb.New(s.startedAt)
	}
	return st
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

var _ io.Writer = (*tailBuffer)(nil)

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
