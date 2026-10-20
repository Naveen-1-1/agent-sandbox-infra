//go:build linux

package agent

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// SystemProcs lists processes from /proc. Inside a container (without a
// shared process namespace) that is exactly the workload's processes.
type SystemProcs struct{}

func (SystemProcs) List() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// /proc/<pid>/stat is "pid (comm) state ..."; skip zombies.
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited
		}
		if i := bytes.LastIndexByte(stat, ')'); i > 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func (SystemProcs) Signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
