package bench

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Kube is the Cluster of a real platform install.
type Kube struct {
	Client    kubernetes.Interface
	Namespace string // where the controller runs
	// Kubectl runs commands in pods (default "kubectl"; it uses $KUBECONFIG).
	Kubectl string
}

// ControllerMetrics reads every running controller's /metrics through
// the API server's pod proxy: exact counters, no scrape interval.
func (k *Kube) ControllerMetrics(ctx context.Context) ([]string, error) {
	pods, err := k.Client.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=sbx-controller"})
	if err != nil {
		return nil, err
	}
	var pages []string
	for _, p := range pods.Items {
		port := metricsPort(&p)
		if p.Status.Phase != corev1.PodRunning || port == 0 {
			continue
		}
		raw, err := k.Client.CoreV1().Pods(k.Namespace).ProxyGet("http", p.Name, strconv.Itoa(port), "/metrics", nil).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		pages = append(pages, string(raw))
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no running controller in %s", k.Namespace)
	}
	return pages, nil
}

func metricsPort(p *corev1.Pod) int {
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == "metrics" {
				return int(cp.ContainerPort)
			}
		}
	}
	return 0
}

// Sidecar runs script in a workspace pod's sidecar container.
func (k *Kube) Sidecar(ctx context.Context, ns, pod, script string) (string, error) {
	kubectl := k.Kubectl
	if kubectl == "" {
		kubectl = "kubectl"
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, kubectl, "exec", "-n", ns, pod, "-c", "sbx-sidecar", "--", "sh", "-c", script)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl exec %s/%s: %w: %s", ns, pod, err, bytes.TrimSpace(errb.Bytes()))
	}
	return out.String(), nil
}

// Platform reads the Kubernetes version, the nodes, and how the
// controller deploys workspaces.
func (k *Kube) Platform(ctx context.Context) (Platform, error) {
	var p Platform
	v, err := k.Client.Discovery().ServerVersion()
	if err != nil {
		return p, err
	}
	p.Kubernetes = v.GitVersion
	nodes, err := k.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return p, err
	}
	for _, n := range nodes.Items {
		p.Nodes = append(p.Nodes, Node{
			Name: n.Name, InstanceType: n.Labels[corev1.LabelInstanceTypeStable], Arch: n.Labels[corev1.LabelArchStable],
			CPU: n.Status.Allocatable.Cpu().String(), Memory: n.Status.Allocatable.Memory().String(),
		})
	}
	deps, err := k.Client.AppsV1().Deployments(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=sbx-controller"})
	if err != nil {
		return p, err
	}
	for _, d := range deps.Items {
		for _, c := range d.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				switch e.Name {
				case "SBX_TOOLS_MODE":
					p.ToolsMode = e.Value
				case "SBX_RUNTIME_IMAGE":
					p.RuntimeImage = e.Value
				}
			}
		}
	}
	return p, nil
}
