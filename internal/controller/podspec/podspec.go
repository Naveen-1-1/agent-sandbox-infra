// Package podspec builds the pod template for one epoch of a workspace. It is
// a pure function of its inputs, so it is tested with golden files.
//
// Containers start in this order:
//
//  1. sbx-tools   (init)            copies the sbx runtime into a shared volume
//  2. sbx-restore (init)            restores this epoch's files onto the disk
//  3. sbx-sidecar (native sidecar)  snapshots and restore progress
//  4. workspace                     the user's image, with sbx-agent as PID 1
//
// The workload starts only once its files are in place. Pod tokens are
// mounted only into our containers; the workload sees just the hash of the
// agent token.
package podspec

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Paths, ports and names inside a workspace pod.
const (
	WorkspaceDir = "/workspace"
	ToolsDir     = "/sbx"
	ScratchDir   = "/sbx-scratch"
	TokenDir     = "/var/run/sbx/tokens" //nolint:gosec // a mount path, not a credential

	AgentGRPCPort = 9090
	AgentRESTPort = 8080
	SidecarPort   = 9091

	ContainerTools    = "sbx-tools"
	ContainerRestore  = "sbx-restore"
	ContainerSidecar  = "sbx-sidecar"
	ContainerWorkload = "workspace"

	VolumeWorkspace = "workspace"
	VolumeTools     = "sbx-tools"
	VolumeScratch   = "sbx-scratch"
	VolumeTokens    = "sbx-tokens"

	// Keys of the per-epoch token Secret.
	SecretKeyBroker  = "broker-token"  //nolint:gosec // a Secret key name, not a credential
	SecretKeySidecar = "sidecar-token" //nolint:gosec // a Secret key name, not a credential
	// The sidecar runs dependency installs through the agent with it.
	SecretKeyAgent = "agent-token" //nolint:gosec // a Secret key name, not a credential

	LabelWorkspace = "sbx.dev/workspace"
	LabelEpoch     = "sbx.dev/epoch"
)

// Tokens is the data of an epoch's token Secret: every token the pod's
// own containers present.
func Tokens(keys token.Keyring, workspaceUID string, epoch int64) map[string]string {
	return map[string]string{
		SecretKeyBroker:  keys.Derive(token.Broker, workspaceUID, epoch),
		SecretKeySidecar: keys.Derive(token.Sidecar, workspaceUID, epoch),
		SecretKeyAgent:   keys.Derive(token.Agent, workspaceUID, epoch),
	}
}

// SidecarResources are what each of our own containers asks for.
var SidecarResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
}

// Requests is what a running workspace pod asks for: the workload and the
// native sidecar (the init containers have finished by then).
func Requests(workload corev1.ResourceRequirements) (cpu, memory resource.Quantity) {
	cpu, memory = workload.Requests.Cpu().DeepCopy(), workload.Requests.Memory().DeepCopy()
	cpu.Add(*SidecarResources.Requests.Cpu())
	memory.Add(*SidecarResources.Requests.Memory())
	return cpu, memory
}

// ToolsMode is how the workload container gets /sbx: the agent it runs as
// PID 1, and the editor.
type ToolsMode string

const (
	// ToolsImage mounts /sbx from the runtime image, read-only, as an image
	// volume: nothing is copied, so the editor costs a wake nothing. It
	// needs Kubernetes 1.35 (or 1.33 with the ImageVolume feature gate).
	ToolsImage ToolsMode = "image"
	// ToolsCopy copies /sbx into the pod with an init container, for
	// clusters without image volumes. Every wake then copies the editor too.
	ToolsCopy ToolsMode = "copy"
)

// Snapshot limits and exclusions passed to the sidecar.
type Snapshot struct {
	Exclude        []string
	MaxFileBytes   int64
	MaxNewBytes    int64
	MaxBundleBytes int64
	// MaxDepsBytes is the largest dependency archive uploaded to the cache.
	MaxDepsBytes int64
}

// Input is everything one epoch's pod depends on.
type Input struct {
	Workspace *sbxv1.Workspace
	Epoch     int64
	// ClaimName is this epoch's PVC.
	ClaimName string
	// Image is the workload image (from devcontainer.json or the default).
	Image string
	// RuntimeImage carries sbx-sidecar, sbx-agent, git and the editor.
	RuntimeImage string
	// Tools says how the workload gets /sbx (default ToolsImage).
	Tools            ToolsMode
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	// TokenSecret holds this epoch's broker and sidecar tokens.
	TokenSecret string
	// AgentTokenSHA256 is the hex SHA-256 of this epoch's agent token.
	AgentTokenSHA256 string
	// BrokerAddr is host:port of the broker.
	BrokerAddr string
	// Resources for the workload container.
	Resources corev1.ResourceRequirements
	// RunAsUser and RunAsGroup apply to every container; FSGroup makes the
	// disk writable by them.
	RunAsUser, RunAsGroup int64
	Snapshot              Snapshot
	NodeSelector          map[string]string
	Tolerations           []corev1.Toleration
}

// Build returns the Sandbox pod template for in.
func Build(in Input) (sbv1.PodTemplate, error) {
	if err := validate(in); err != nil {
		return sbv1.PodTemplate{}, err
	}
	ws := in.Workspace
	epoch := strconv.FormatInt(in.Epoch, 10)

	common := []corev1.EnvVar{
		{Name: "SBX_WORKSPACE", Value: ws.Namespace + "/" + ws.Name},
		{Name: "SBX_WORKSPACE_UID", Value: string(ws.UID)},
		{Name: "SBX_EPOCH", Value: epoch},
		{Name: "SBX_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "SBX_BROKER_ADDR", Value: in.BrokerAddr},
		{Name: "SBX_TOKEN_DIR", Value: TokenDir},
		{Name: "SBX_WORKSPACE_DIR", Value: WorkspaceDir},
		{Name: "SBX_SCRATCH_DIR", Value: ScratchDir},
	}
	ours := []corev1.VolumeMount{
		{Name: VolumeWorkspace, MountPath: WorkspaceDir},
		{Name: VolumeScratch, MountPath: ScratchDir},
		{Name: VolumeTokens, MountPath: TokenDir, ReadOnly: true},
	}
	sidecarEnv := append(append([]corev1.EnvVar{}, common...),
		corev1.EnvVar{Name: "SBX_SIDECAR_ADDR", Value: fmt.Sprintf(":%d", SidecarPort)},
		corev1.EnvVar{Name: "SBX_SNAPSHOT_EXCLUDE", Value: strings.Join(in.Snapshot.Exclude, ",")},
		corev1.EnvVar{Name: "SBX_SNAPSHOT_MAX_FILE_BYTES", Value: strconv.FormatInt(in.Snapshot.MaxFileBytes, 10)},
		corev1.EnvVar{Name: "SBX_SNAPSHOT_MAX_NEW_BYTES", Value: strconv.FormatInt(in.Snapshot.MaxNewBytes, 10)},
		corev1.EnvVar{Name: "SBX_SNAPSHOT_MAX_BUNDLE_BYTES", Value: strconv.FormatInt(in.Snapshot.MaxBundleBytes, 10)},
		corev1.EnvVar{Name: "SBX_DEPS_MAX_BYTES", Value: strconv.FormatInt(in.Snapshot.MaxDepsBytes, 10)},
		corev1.EnvVar{Name: "SBX_AGENT_ADDR", Value: fmt.Sprintf("127.0.0.1:%d", AgentGRPCPort)},
	)
	small := SidecarResources
	hardened := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	spec := corev1.PodSpec{
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		TerminationGracePeriodSeconds: ptr.To[int64](10),
		ImagePullSecrets:              in.ImagePullSecrets,
		NodeSelector:                  in.NodeSelector,
		Tolerations:                   in.Tolerations,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:      ptr.To(in.RunAsUser),
			RunAsGroup:     ptr.To(in.RunAsGroup),
			FSGroup:        ptr.To(in.RunAsGroup),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: []corev1.Container{
			{
				Name: ContainerRestore, Image: in.RuntimeImage, ImagePullPolicy: in.PullPolicy,
				Args: []string{"restore"}, Env: common, VolumeMounts: ours,
				Resources: small, SecurityContext: hardened,
			},
			{
				Name: ContainerSidecar, Image: in.RuntimeImage, ImagePullPolicy: in.PullPolicy,
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
				Args:          []string{"serve"}, Env: sidecarEnv, VolumeMounts: ours,
				Ports: []corev1.ContainerPort{{Name: "sidecar", ContainerPort: SidecarPort}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: SidecarPort}},
					PeriodSeconds: 2,
				},
				Resources: small, SecurityContext: hardened,
			},
		},
		Containers: []corev1.Container{{
			Name: ContainerWorkload, Image: in.Image, ImagePullPolicy: in.PullPolicy,
			Command:    []string{ToolsDir + "/bin/sbx-agent"},
			WorkingDir: WorkspaceDir,
			Env: []corev1.EnvVar{
				{Name: "SBX_WORKSPACE_DIR", Value: WorkspaceDir},
				{Name: "SBX_AGENT_TOKEN_SHA256", Value: in.AgentTokenSHA256},
				{Name: "SBX_AGENT_GRPC_ADDR", Value: fmt.Sprintf(":%d", AgentGRPCPort)},
				{Name: "SBX_AGENT_REST_ADDR", Value: fmt.Sprintf(":%d", AgentRESTPort)},
				// The disk's mount root is owned by root on every storage
				// driver, so git would refuse the repository ("dubious
				// ownership"). Config from the environment counts as
				// command-line config, which is where safe.directory is honored.
				{Name: "GIT_CONFIG_COUNT", Value: "1"},
				{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
				{Name: "GIT_CONFIG_VALUE_0", Value: WorkspaceDir},
			},
			Ports: []corev1.ContainerPort{
				{Name: "agent-grpc", ContainerPort: AgentGRPCPort},
				{Name: "agent-rest", ContainerPort: AgentRESTPort},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/v1/health", Port: intstr.FromInt32(AgentRESTPort)}},
				PeriodSeconds: 2,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: VolumeWorkspace, MountPath: WorkspaceDir},
				{Name: VolumeTools, MountPath: ToolsDir, SubPath: "sbx", ReadOnly: true},
			},
			Resources: in.Resources,
		}},
		Volumes: []corev1.Volume{
			{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: in.ClaimName},
			}},
			{Name: VolumeTools, VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{
				Reference: in.RuntimeImage, PullPolicy: in.PullPolicy,
			}}},
			{Name: VolumeScratch, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: VolumeTokens, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  in.TokenSecret,
				DefaultMode: ptr.To[int32](0o440),
				Items: []corev1.KeyToPath{
					{Key: SecretKeyBroker, Path: SecretKeyBroker},
					{Key: SecretKeySidecar, Path: SecretKeySidecar},
					{Key: SecretKeyAgent, Path: SecretKeyAgent},
				},
			}}},
		},
	}
	if in.Tools == ToolsCopy {
		spec.InitContainers = append([]corev1.Container{{
			Name: ContainerTools, Image: in.RuntimeImage, ImagePullPolicy: in.PullPolicy,
			Args:            []string{"install-tools", "/tools/sbx"},
			VolumeMounts:    []corev1.VolumeMount{{Name: VolumeTools, MountPath: "/tools"}},
			Resources:       small,
			SecurityContext: hardened,
		}}, spec.InitContainers...)
		for i := range spec.Volumes {
			if spec.Volumes[i].Name == VolumeTools {
				spec.Volumes[i].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
			}
		}
	}
	return sbv1.PodTemplate{
		ObjectMeta: sbv1.PodMetadata{Labels: map[string]string{LabelWorkspace: ws.Name, LabelEpoch: epoch}},
		Spec:       spec,
	}, nil
}

func validate(in Input) error {
	var missing []string
	for name, v := range map[string]string{
		"Image": in.Image, "RuntimeImage": in.RuntimeImage, "ClaimName": in.ClaimName,
		"TokenSecret": in.TokenSecret, "AgentTokenSHA256": in.AgentTokenSHA256, "BrokerAddr": in.BrokerAddr,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if in.Workspace == nil || in.Workspace.UID == "" {
		missing = append(missing, "Workspace (with UID)")
	}
	if in.Epoch < 1 {
		missing = append(missing, "Epoch >= 1")
	}
	if in.Tools != "" && in.Tools != ToolsImage && in.Tools != ToolsCopy {
		return fmt.Errorf("podspec: unknown tools mode %q", in.Tools)
	}
	if len(missing) > 0 {
		return fmt.Errorf("podspec: missing %s", strings.Join(missing, ", "))
	}
	return nil
}
