//go:build e2e

package framework

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
)

// Clients bundles the API clients e2e tests use.
type Clients struct {
	Config  *rest.Config
	Kube    kubernetes.Interface
	Sandbox sandboxclient.Interface
	// CR reads and writes Workspaces, Sandboxes and core objects.
	CR client.Client
}

// NewClients connects to the cluster named by $KUBECONFIG.
func NewClients(t testing.TB) *Clients {
	t.Helper()
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		t.Fatal("KUBECONFIG is not set; run `make kind-up` and use `make test-e2e`")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.QPS, cfg.Burst = 50, 100
	// One shared HTTP client, closed when the test ends so its idle
	// connections don't show up as leaked goroutines.
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { utilnet.CloseIdleConnectionsFor(hc.Transport) })
	kube, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandboxclient.NewForConfigAndClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, sbv1.AddToScheme, sbxv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cr, err := client.New(cfg, client.Options{Scheme: scheme, HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	return &Clients{Config: cfg, Kube: kube, Sandbox: sb, CR: cr}
}

// Ctx returns a context that ends with the test or after d.
func Ctx(t testing.TB, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestNamespace creates a uniquely named namespace deleted when the test ends.
func TestNamespace(t testing.TB, c *Clients) string {
	t.Helper()
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	name := "e2e-" + hex.EncodeToString(b)
	_, err := c.Kube.CoreV1().Namespaces().Create(Ctx(t, time.Minute), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"sbx.dev/e2e": "true"}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") != "" {
			t.Logf("keeping namespace %s", name)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.Kube.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	})
	return name
}

// CreatePVC creates a ReadWriteOnce claim on the default storage class.
func CreatePVC(t testing.TB, c *Clients, ns, name string) {
	t.Helper()
	_, err := c.Kube.CoreV1().PersistentVolumeClaims(ns).Create(Ctx(t, time.Minute), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// Eventually polls check every interval until it returns true or timeout
// passes, then fails the test with msg.
func Eventually(t testing.TB, timeout, interval time.Duration, msg string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		ok, err := check()
		if ok {
			return
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last error: %v)", timeout, msg, last)
		}
		time.Sleep(interval)
	}
}
