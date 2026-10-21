package agent

import (
	"os"
	"os/signal"
	"syscall"
)

// childEnv marks the re-executed agent process.
const childEnv = "SBX_AGENT_CHILD=1"

// ShouldSupervise reports whether this process is PID 1 and must hand the
// real work to a child. PID 1 then only reaps orphans and forwards signals,
// which keeps zombie reaping from racing Go's own waits on exec'd commands.
func ShouldSupervise() bool {
	return os.Getpid() == 1 && os.Getenv("SBX_AGENT_CHILD") == ""
}

// Supervise re-executes the current binary as a child, forwards termination
// signals to it, reaps every child process (including orphans reparented to
// PID 1), and returns the child's exit code. The child, and so every
// command, service and the editor, gets a writable HOME.
func Supervise() int {
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	//nolint:gosec // re-executes this same binary with its own arguments
	child, err := os.StartProcess(exe, os.Args, &os.ProcAttr{
		Env:   append(withUsableHome(os.Environ(), fallbackHome), childEnv),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 1
	}
	go func() {
		for s := range sigs {
			_ = child.Signal(s)
		}
	}()
	return reapUntil(child.Pid)
}

// reapUntil waits for any child until pid exits, and returns its exit code.
func reapUntil(pid int) int {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		if got != pid {
			continue // an orphan; reaped
		}
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
}
