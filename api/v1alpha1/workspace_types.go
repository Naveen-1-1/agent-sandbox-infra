package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Workspace is a long-lived agent sandbox. While awake it owns one
// agent-sandbox Sandbox (one pod) and one PVC. While asleep it owns neither:
// its files live in S3 as a git bundle.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ws
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.epoch`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.resolvedImage`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// WorkspaceList is a list of Workspaces.
//
// +kubebuilder:object:root=true
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

// WorkspaceSpec is what the user (and, for Wake, the gateway) asks for.
type WorkspaceSpec struct {
	// Source is the git repository the workspace starts from.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="source is immutable"
	Source Source `json:"source"`

	// Image overrides the image chosen from the repository's devcontainer.json
	// (or the platform default).
	// +optional
	Image string `json:"image,omitempty"`

	// Profile names a resource profile (CPU, memory, disk) from the platform
	// configuration.
	// +kubebuilder:default=default
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Profile string `json:"profile,omitempty"`

	// Mode says who decides when the workspace sleeps:
	//   - Auto: the sleep policy suspends it when idle; requests wake it.
	//   - AlwaysOn: it is never suspended.
	//   - Off: it stays suspended; requests never wake it.
	// +kubebuilder:default=Auto
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// Wake is written by the gateway to ask for a wake. Seq only increases;
	// the controller wakes once for each new value.
	// +optional
	Wake WakeRequest `json:"wake,omitempty"`

	// Policy overrides the cluster-wide sleep policy for this workspace.
	// +optional
	Policy *PolicyRef `json:"policy,omitempty"`
}

// Source is a git repository and the revision to start from.
type Source struct {
	// RepoURL is the repository to clone, over https or http. Credentials
	// must not be embedded in it.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/@]+(/[^\s]*)?$`
	// +kubebuilder:validation:MaxLength=2048
	RepoURL string `json:"repoURL"`

	// Ref is a branch, tag or commit. Empty means the remote's default branch.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Ref string `json:"ref,omitempty"`

	// Depth makes the initial clone shallow. Zero clones full history.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Depth int32 `json:"depth,omitempty"`
}

// Mode controls whether the sleep policy may suspend the workspace.
// +kubebuilder:validation:Enum=Auto;AlwaysOn;Off
type Mode string

const (
	ModeAuto     Mode = "Auto"
	ModeAlwaysOn Mode = "AlwaysOn"
	ModeOff      Mode = "Off"
)

// WakeRequest is the gateway's request to wake the workspace.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.seq) || (has(self.seq) && self.seq >= oldSelf.seq)",message="wake.seq must not decrease"
type WakeRequest struct {
	// Seq is bumped by one for each wake request.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Seq int64 `json:"seq,omitempty"`

	// Trigger says what asked for the wake.
	// +optional
	Trigger WakeTrigger `json:"trigger,omitempty"`

	// At is when the wake was requested.
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// WakeTrigger is what asked for a wake.
// +kubebuilder:validation:Enum=Request;Hint;API
type WakeTrigger string

const (
	// WakeTriggerRequest is a real request that is being held until the
	// workspace is ready.
	WakeTriggerRequest WakeTrigger = "Request"
	// WakeTriggerHint is a wake-hint: a user message arrived and the agent
	// will probably need the workspace soon.
	WakeTriggerHint WakeTrigger = "Hint"
	// WakeTriggerAPI is an explicit wake through the management API.
	WakeTriggerAPI WakeTrigger = "API"
)

// SleepStatus is a sleep policy verdict.
type SleepStatus struct {
	// Policy is the policy that decided, for example "fixed". Empty for an
	// explicit suspend.
	// +optional
	Policy string `json:"policy,omitempty"`

	// Reason is short and stable, for example "Active", "CommandRunning",
	// "Idle" or "TurnEnded".
	Reason string `json:"reason"`

	// +optional
	Message string `json:"message,omitempty"`

	// SuspendAt is when the workspace will suspend if nothing happens first.
	// +optional
	SuspendAt *metav1.Time `json:"suspendAt,omitempty"`
}

// PolicyRef selects a sleep policy and its parameters.
type PolicyRef struct {
	// Name is a registered policy, for example "fixed" or "never".
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Params are policy-specific settings, for example idleTimeout: "10m".
	// +optional
	Params map[string]string `json:"params,omitempty"`
}

// Phase is where a workspace is in its sleep/wake cycle.
// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Suspending;TearingDown;Suspended;Waking;Deleting;Failed
type Phase string

const (
	PhasePending      Phase = "Pending"      // accepted, nothing created yet
	PhaseProvisioning Phase = "Provisioning" // first pod and disk are being created
	PhaseRunning      Phase = "Running"      // pod is up and files are in place
	PhaseSuspending   Phase = "Suspending"   // freezing and snapshotting; can still abort
	PhaseTearingDown  Phase = "TearingDown"  // snapshot committed; deleting pod and disk
	PhaseSuspended    Phase = "Suspended"    // no pod, no disk; state lives in S3
	PhaseWaking       Phase = "Waking"       // new pod and disk are being restored
	PhaseDeleting     Phase = "Deleting"     // workspace is being removed
	PhaseFailed       Phase = "Failed"       // needs a human; see conditions
)

// AnnotationPaused makes the controller leave the workspace alone (for
// maintenance or debugging); remove it to resume reconciling.
const AnnotationPaused = "sbx.dev/paused"

// AnnotationSuspendRequested asks the controller to suspend the workspace
// once; it removes the annotation when the suspend finishes or is aborted.
const AnnotationSuspendRequested = "sbx.dev/suspend-requested"

// Condition types reported on a Workspace.
const (
	// ConditionReady is true when the workspace can serve requests.
	ConditionReady = "Ready"
	// ConditionFilesReady is true once the workspace files are restored.
	ConditionFilesReady = "FilesReady"
	// ConditionDepsReady is true once dependency directories are restored.
	ConditionDepsReady = "DepsReady"
	// ConditionSnapshotFailed is true while the last snapshot attempt failed;
	// the workspace keeps running.
	ConditionSnapshotFailed = "SnapshotFailed"
	// ConditionVolumeLost is true if the disk vanished while awake.
	ConditionVolumeLost = "VolumeLost"
)

// WorkspaceStatus is owned by the controller.
type WorkspaceStatus struct {
	// ObservedGeneration is the spec generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase Phase `json:"phase,omitempty"`

	// PhaseSince is when the workspace entered its current phase.
	// +optional
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`

	// Epoch counts wakes. Each epoch gets its own pod, disk and tokens.
	// +optional
	Epoch int64 `json:"epoch,omitempty"`

	// ObservedWakeSeq is the last spec.wake.seq the controller acted on.
	// +optional
	ObservedWakeSeq int64 `json:"observedWakeSeq,omitempty"`

	// Base is the upstream commit the workspace started from.
	// +optional
	Base *BaseStatus `json:"base,omitempty"`

	// ResolvedImage is the workload image in use.
	// +optional
	ResolvedImage string `json:"resolvedImage,omitempty"`

	// Volume is the current epoch's disk.
	// +optional
	Volume *VolumeStatus `json:"volume,omitempty"`

	// Pod identifies the current epoch's pod.
	// +optional
	Pod *PodStatus `json:"pod,omitempty"`

	// Suspend tracks an in-progress suspend attempt.
	// +optional
	Suspend *SuspendStatus `json:"suspend,omitempty"`

	// Wake tracks an in-progress wake.
	// +optional
	Wake *WakeStatus `json:"wake,omitempty"`

	// LastSnapshot is the newest snapshot committed to S3.
	// +optional
	LastSnapshot *SnapshotRef `json:"lastSnapshot,omitempty"`

	// Timeline records how long the last wake took, stage by stage.
	// +optional
	Timeline *WakeTimeline `json:"timeline,omitempty"`

	// SuspendFailures counts snapshot attempts that failed in a row.
	// +optional
	SuspendFailures int32 `json:"suspendFailures,omitempty"`

	// SuspendRetryAt holds off the next suspend after a failure (exponential
	// backoff), so a workspace that can't be snapshotted keeps running.
	// +optional
	SuspendRetryAt *metav1.Time `json:"suspendRetryAt,omitempty"`

	// LastActivityAt is a coarse record of the last user or agent activity,
	// so a restarted controller doesn't think every workspace is idle.
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// Sleep is the sleep policy's latest verdict: why the workspace is still
	// up and when it will suspend, or why it suspended.
	// +optional
	Sleep *SleepStatus `json:"sleep,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BaseStatus is the upstream commit a workspace started from.
type BaseStatus struct {
	// URL is the upstream repository.
	URL string `json:"url"`
	// SHA is the commit the workspace was created at.
	SHA string `json:"sha"`
	// Branch is the branch created at SHA.
	// +optional
	Branch string `json:"branch,omitempty"`
	// BundleKey is the S3 key of a bundle of SHA, if one is cached.
	// +optional
	BundleKey string `json:"bundleKey,omitempty"`
	// Shallow is true when the workspace was cloned with a depth limit; its
	// base always comes from the upstream remote.
	// +optional
	Shallow bool `json:"shallow,omitempty"`
}

// VolumeStatus is the disk backing the current epoch.
type VolumeStatus struct {
	ClaimName string `json:"claimName"`
	// EverBound is set once a pod has used the claim. A claim that was ever
	// bound is never recreated, because an empty replacement would look like
	// lost work.
	// +optional
	EverBound bool `json:"everBound,omitempty"`
}

// PodStatus identifies the pod serving the current epoch.
type PodStatus struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +optional
	IP string `json:"ip,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`
}

// SuspendStatus tracks one suspend attempt.
type SuspendStatus struct {
	// AttemptID names the attempt; every side effect is keyed by it.
	AttemptID string `json:"attemptID"`
	// PodUID is the pod being snapshotted. If it changes, the attempt is void.
	PodUID types.UID `json:"podUID"`
	// StartedWakeSeq is spec.wake.seq when the attempt began. A different
	// value at commit time means a wake raced the suspend, and it aborts.
	StartedWakeSeq int64       `json:"startedWakeSeq"`
	StartedAt      metav1.Time `json:"startedAt"`
	// Step is the last completed step.
	// +optional
	Step SuspendStep `json:"step,omitempty"`
	// FrozenAt is when the workload was frozen; it also timestamps the
	// snapshot commits.
	// +optional
	FrozenAt *metav1.Time `json:"frozenAt,omitempty"`
	// Snapshot is the verified snapshot for this attempt.
	// +optional
	Snapshot *SnapshotRef `json:"snapshot,omitempty"`
	// Committed is the point of no return: the pod and disk will be deleted.
	// +optional
	Committed bool `json:"committed,omitempty"`
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// SuspendStep is a completed step of a suspend.
// +kubebuilder:validation:Enum=Started;Frozen;Snapshotted;Verified;ManifestWritten;Committed;PodGone;VolumeDeleted
type SuspendStep string

const (
	SuspendStarted         SuspendStep = "Started"
	SuspendFrozen          SuspendStep = "Frozen"
	SuspendSnapshotted     SuspendStep = "Snapshotted"
	SuspendVerified        SuspendStep = "Verified"
	SuspendManifestWritten SuspendStep = "ManifestWritten"
	SuspendCommitted       SuspendStep = "Committed"
	SuspendPodGone         SuspendStep = "PodGone"
	SuspendVolumeDeleted   SuspendStep = "VolumeDeleted"
)

// WakeStatus tracks an in-progress wake.
type WakeStatus struct {
	// TargetEpoch is the epoch being brought up.
	TargetEpoch int64 `json:"targetEpoch"`
	// RestoreSeq is the snapshot being restored; zero restores the base.
	// +optional
	RestoreSeq int64 `json:"restoreSeq,omitempty"`
	// +optional
	Trigger   WakeTrigger `json:"trigger,omitempty"`
	StartedAt metav1.Time `json:"startedAt"`
}

// SnapshotRef points at a snapshot bundle in S3.
type SnapshotRef struct {
	// Seq numbers snapshots of one workspace, starting at 1.
	Seq   int64 `json:"seq"`
	Epoch int64 `json:"epoch"`
	// WIPSHA is the snapshot commit.
	WIPSHA string `json:"wipSHA"`
	// BundleKey is the S3 key of the bundle (content addressed).
	BundleKey string `json:"bundleKey"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}

// WakeTimeline records when each stage of the last wake finished. Times
// have microsecond precision: wake stages are seconds apart, so whole
// seconds would hide most of what they measure.
type WakeTimeline struct {
	// +optional
	Trigger WakeTrigger `json:"trigger,omitempty"`
	// +optional
	RequestedAt *metav1.MicroTime `json:"requestedAt,omitempty"`
	// +optional
	PodScheduledAt *metav1.MicroTime `json:"podScheduledAt,omitempty"`
	// +optional
	FilesReadyAt *metav1.MicroTime `json:"filesReadyAt,omitempty"`
	// +optional
	ReadyAt *metav1.MicroTime `json:"readyAt,omitempty"`
	// +optional
	DepsReadyAt *metav1.MicroTime `json:"depsReadyAt,omitempty"`
}
