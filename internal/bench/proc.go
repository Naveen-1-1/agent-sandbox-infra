package bench

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// clockTicks is Linux's USER_HZ: /proc reports CPU time in these.
const clockTicks = 100

// sectionSep splits the outputs of the scripts below.
const sectionSep = "---sbx-bench---"

// sidecarScript prints the sidecar's own process (PID 1 of its container)
// and its container's cgroup peak memory, if the kernel reports one.
const sidecarScript = `cat /proc/1/stat; echo ` + sectionSep + `; cat /proc/1/status; echo ` + sectionSep + `; cat /sys/fs/cgroup/memory.peak 2>/dev/null || true`

// agentScript prints sbx-agent: PID 1 of the workspace container is its
// supervisor, and the agent that serves requests is the supervisor's
// child. Other children are reaped orphans, told apart by name.
const agentScript = `for p in 1 $(cat /proc/1/task/1/children); do echo ` + sectionSep + `; cat /proc/$p/stat; echo ` + sectionSep + `; cat /proc/$p/status; done`

// Usage is what a process (or a container's processes) used.
type Usage struct {
	CPUSeconds   float64 `json:"cpuSeconds"`   // user and system, children it reaped included
	PeakRSSBytes int64   `json:"peakRssBytes"` // the high-water mark of its resident memory
	// CgroupPeakBytes is the container's peak memory, if known.
	CgroupPeakBytes int64 `json:"cgroupPeakBytes,omitempty"`
}

// proc is one process's line in /proc/<pid>/stat and its status.
type proc struct {
	comm  string
	ticks int64
	hwm   int64 // bytes
}

// parseStat reads /proc/<pid>/stat: the name sits in parentheses and may
// hold spaces or parentheses itself, so fields are counted from the last
// ")". utime, stime, cutime and cstime are fields 14 to 17.
func parseStat(line string) (comm string, ticks int64, err error) {
	open, closing := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
	if open < 0 || closing < open {
		return "", 0, fmt.Errorf("bench: not a /proc stat line: %.60q", line)
	}
	comm = line[open+1 : closing]
	f := strings.Fields(line[closing+1:]) // f[0] is field 3, the state
	if len(f) < 15 {
		return "", 0, fmt.Errorf("bench: short /proc stat line: %.60q", line)
	}
	for _, s := range f[11:15] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("bench: /proc stat field %q: %w", s, err)
		}
		ticks += n
	}
	return comm, ticks, nil
}

// parseStatus reads VmHWM, the peak resident set, from /proc/<pid>/status.
func parseStatus(text string) int64 {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmHWM:"); ok {
			f := strings.Fields(v)
			if len(f) == 2 && f[1] == "kB" {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

func sections(out string) []string {
	parts := strings.Split(out, sectionSep)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseSidecar reads sidecarScript's output.
func parseSidecar(out string) (Usage, error) {
	s := sections(out)
	if len(s) < 2 {
		return Usage{}, fmt.Errorf("bench: unexpected sidecar output %.80q", out)
	}
	_, ticks, err := parseStat(s[0])
	if err != nil {
		return Usage{}, err
	}
	u := Usage{CPUSeconds: float64(ticks) / clockTicks, PeakRSSBytes: parseStatus(s[1])}
	if len(s) > 2 && s[2] != "" {
		u.CgroupPeakBytes, _ = strconv.ParseInt(s[2], 10, 64)
	}
	return u, nil
}

// parseAgent reads agentScript's output: the supervisor's CPU time plus
// the agent's, and the larger of their peaks.
func parseAgent(out string) (Usage, error) {
	s := sections(out)
	var procs []proc
	for i := 1; i+1 < len(s); i += 2 {
		comm, ticks, err := parseStat(s[i])
		if err != nil {
			continue // the process exited between listing and reading
		}
		procs = append(procs, proc{comm: comm, ticks: ticks, hwm: parseStatus(s[i+1])})
	}
	if len(procs) == 0 {
		return Usage{}, fmt.Errorf("bench: unexpected agent output %.80q", out)
	}
	var u Usage
	for i, p := range procs {
		if i > 0 && p.comm != "sbx-agent" {
			continue
		}
		u.CPUSeconds += float64(p.ticks) / clockTicks
		u.PeakRSSBytes = max(u.PeakRSSBytes, p.hwm)
	}
	return u, nil
}
