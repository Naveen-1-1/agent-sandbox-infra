package costsim

import (
	"slices"
	"strings"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
)

// ToolClass says what a tool call means for the sandbox.
type ToolClass int

const (
	// ToolOther runs outside the sandbox (web search, MCP servers, the
	// model's own bookkeeping): the model's time, invisible to the platform.
	ToolOther ToolClass = iota
	// ToolSandbox reads, writes or runs something in the workspace: a
	// gateway request that counts as activity and keeps it awake.
	ToolSandbox
	// ToolSubagent starts or waits for a subagent; its own calls show up
	// in the subagent's session.
	ToolSubagent
	// ToolHuman waits for a person: its window is human time.
	ToolHuman
)

var (
	sandboxTools = []string{
		// Claude Code; TaskOutput, TaskStop and Monitor read, stop and watch
		// background shells.
		"Bash", "BashOutput", "KillShell", "KillBash", "TaskOutput", "TaskStop", "Monitor", "Read", "Write", "Edit",
		"MultiEdit", "NotebookEdit", "NotebookRead", "Glob", "Grep", "LS",
		// Codex
		"shell", "shell_command", "local_shell", "exec_command", "unified_exec", "write_stdin", "apply_patch",
		"view_image", "container.exec",
	}
	shellTools    = []string{"Bash", "shell", "shell_command", "local_shell", "exec_command", "unified_exec", "container.exec"}
	subagentTools = []string{"Task", "Agent", "spawn_agent", "wait_agent", "close_agent", "send_input"}
	humanTools    = []string{"AskUserQuestion", "ExitPlanMode", "request_user_input"}
)

// Classify names a tool's class.
func Classify(name string) ToolClass {
	switch {
	case slices.Contains(sandboxTools, name):
		return ToolSandbox
	case slices.Contains(subagentTools, name):
		return ToolSubagent
	case slices.Contains(humanTools, name):
		return ToolHuman
	}
	return ToolOther
}

// DepsMode says which sandbox calls need the dependency directory, so a
// wake holds them until it's restored, not just until files are.
type DepsMode string

const (
	// DepsNone: no call waits for dependencies (only files).
	DepsNone DepsMode = "none"
	// DepsAuto: shell calls that run a package manager or runtime, as the
	// gateway infers from the command. Needs the trace's executables.
	DepsAuto DepsMode = "auto"
	// DepsShell: every shell call waits for dependencies (a pessimistic
	// bound when the trace doesn't say what ran).
	DepsShell DepsMode = "shell"
)

func needsDeps(t Tool, mode DepsMode) bool {
	if !slices.Contains(shellTools, t.Name) {
		return false
	}
	switch mode {
	case DepsShell:
		return true
	case DepsAuto:
		for _, exe := range t.Exe {
			base := exe[strings.LastIndexByte(exe, '/')+1:]
			for _, p := range depcache.Profiles {
				if slices.Contains(p.Commands, base) {
					return true
				}
			}
		}
	}
	return false
}
