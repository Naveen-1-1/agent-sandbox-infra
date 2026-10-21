// Package sbxapi is the gateway's REST API: the JSON types and a Go client.
// sbxctl, the end-to-end tests, the demo and the web UI all use it.
//
// Routes (all under /api/v1):
//
//	POST   /workspaces                              create (admin)
//	GET    /workspaces                              list (admin)
//	GET    /workspaces/{ns}/{name}                  status; never wakes
//	DELETE /workspaces/{ns}/{name}                  delete (admin)
//	GET    /workspaces/{ns}/{name}/token            workspace token (admin)
//	POST   /workspaces/{ns}/{name}/exec             run a command; waits for a wake
//	GET|PUT|DELETE /workspaces/{ns}/{name}/files/…  files; waits for a wake
//	POST   /workspaces/{ns}/{name}/wake             wake now and wait until ready
//	POST   /workspaces/{ns}/{name}/wake-hint        a user message arrived; start waking, don't wait
//	POST   /workspaces/{ns}/{name}/turn-ended       the agent finished its turn
//	POST   /workspaces/{ns}/{name}/suspend          suspend now
//	GET    /workspaces/{ns}/{name}/services         declared services and, if awake, their state
//	PUT|DELETE /workspaces/{ns}/{name}/services/{svc} declare or remove a service
//
// Requests carry "Authorization: Bearer <token>", either the admin token or
// the workspace's own token.
package sbxapi

import "time"

// Workspace is the public view of a workspace.
type Workspace struct {
	Namespace    string      `json:"namespace"`
	Name         string      `json:"name"`
	UID          string      `json:"uid"`
	RepoURL      string      `json:"repoURL"`
	Ref          string      `json:"ref,omitempty"`
	Image        string      `json:"image,omitempty"`
	Mode         string      `json:"mode"`
	Phase        string      `json:"phase"`
	Epoch        int64       `json:"epoch"`
	Ready        bool        `json:"ready"`
	Message      string      `json:"message,omitempty"`
	Node         string      `json:"node,omitempty"`
	Conditions   []Condition `json:"conditions,omitempty"`
	LastSnapshot *Snapshot   `json:"lastSnapshot,omitempty"`
	Timeline     *Timeline   `json:"timeline,omitempty"`
	Policy       *PolicyRef  `json:"policy,omitempty"` // its own policy, if not the cluster's
	Sleep        *Sleep      `json:"sleep,omitempty"`
	Deps         *Deps       `json:"deps,omitempty"`
	CreatedAt    time.Time   `json:"createdAt"`
}

// Deps is the state of the workspace's dependency directory.
type Deps struct {
	Profile string `json:"profile,omitempty"` // npm, pip, uv; empty if none
	Source  string `json:"source,omitempty"`  // None, Present, Cache or Install
	// Pending means commands that need the dependencies are held.
	Pending bool   `json:"pending"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

// Service is a declared long-running process and, when the workspace is
// awake, its state.
type Service struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// WaitForDeps holds the start until dependencies are restored (default true).
	WaitForDeps *bool `json:"waitForDeps,omitempty"`

	Running      bool   `json:"running"`
	Pid          int32  `json:"pid,omitempty"`
	Restarts     int32  `json:"restarts,omitempty"`
	LastExitCode int32  `json:"lastExitCode,omitempty"`
	Output       string `json:"output,omitempty"` // the end of its output
}

// PolicyRef names a sleep policy and its parameters.
type PolicyRef struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
}

// Sleep is the sleep policy's latest verdict on a workspace.
type Sleep struct {
	Policy    string     `json:"policy,omitempty"`
	Reason    string     `json:"reason"`
	Message   string     `json:"message,omitempty"`
	SuspendAt *time.Time `json:"suspendAt,omitempty"`
}

// Condition mirrors a Kubernetes condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Snapshot summarizes the last committed snapshot.
type Snapshot struct {
	Seq       int64      `json:"seq"`
	Bytes     int64      `json:"bytes"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// Timeline is when each stage of the last wake finished.
type Timeline struct {
	Trigger        string     `json:"trigger,omitempty"`
	RequestedAt    *time.Time `json:"requestedAt,omitempty"`
	PodScheduledAt *time.Time `json:"podScheduledAt,omitempty"`
	FilesReadyAt   *time.Time `json:"filesReadyAt,omitempty"`
	ReadyAt        *time.Time `json:"readyAt,omitempty"`
	DepsReadyAt    *time.Time `json:"depsReadyAt,omitempty"`
}

// CreateRequest creates a workspace from a git repository.
type CreateRequest struct {
	Namespace string `json:"namespace,omitempty"` // default: the gateway's workspace namespace
	Name      string `json:"name,omitempty"`      // default: generated
	RepoURL   string `json:"repoURL"`
	Ref       string `json:"ref,omitempty"`
	Depth     int32  `json:"depth,omitempty"`
	Image     string `json:"image,omitempty"`
	Mode      string `json:"mode,omitempty"`
	// Policy overrides the cluster's sleep policy for this workspace.
	Policy *PolicyRef `json:"policy,omitempty"`
	// Services to keep running; see PUT .../services/{svc}.
	Services []Service `json:"services,omitempty"`
}

// CreateResponse is the new workspace and its token.
type CreateResponse struct {
	Workspace Workspace `json:"workspace"`
	Token     string    `json:"token"`
}

// TokenResponse carries a workspace token.
type TokenResponse struct {
	Token string `json:"token"`
}

// ExecRequest runs one command in the workspace.
type ExecRequest struct {
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	// WaitFor says what the command needs: "deps" always waits for the
	// dependency directory, "none" never does. Empty: commands of the
	// workspace's package manager (npm, node, python, pytest, ...) wait.
	WaitFor []string `json:"waitFor,omitempty"`
}

// ExecResponse is a finished command.
type ExecResponse struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// WaitedMillis is how long the request was held for a wake; zero means
	// the workspace was already awake (or the wake was fully hidden).
	WaitedMillis int64 `json:"waitedMillis"`
	// DepsWaitedMillis is how long it then waited for dependencies.
	DepsWaitedMillis int64 `json:"depsWaitedMillis"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Error codes.
const (
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid"
	CodeOff          = "workspace_off" // mode Off: requests never wake it
	CodeQueueFull    = "queue_full"    // too many requests waiting; retry later
	CodeWakeTimeout  = "wake_timeout"  // the workspace did not wake in time
	CodeDepsTimeout  = "deps_timeout"  // dependencies weren't restored in time
	CodeUnavailable  = "unavailable"   // the workspace or its pod is unreachable
	CodeInternal     = "internal"
)
