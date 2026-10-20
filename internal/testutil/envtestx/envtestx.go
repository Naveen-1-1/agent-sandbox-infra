// Package envtestx starts a real kube-apiserver and etcd (controller-runtime
// envtest) with our CRDs installed, for tests of API validation and
// controllers. Binaries come from $KUBEBUILDER_ASSETS or from bin/envtest,
// where `make envtest` installs them.
package envtestx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// KubeVersion is the API server version tests run against; keep it in step
// with the kind node image and ENVTEST_K8S_VERSION in the Makefile.
const KubeVersion = "1.35.0"

// Env is a running test API server.
type Env struct {
	Config *rest.Config
	Client client.Client
	env    *envtest.Environment
	stopFn func()
}

// Options selects what to install.
type Options struct {
	// Scheme for the client; it must include every type the test uses.
	Scheme *k8sruntime.Scheme
	// Upstream also installs the vendored agent-sandbox CRDs.
	Upstream bool
}

// Start boots the API server and installs our CRDs.
func Start(o Options) (*Env, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = filepath.Join(root, "bin", "envtest", "k8s", KubeVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH)
	}
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil {
		return nil, fmt.Errorf("envtest binaries not found in %s; run `make envtest`", assets)
	}

	crds := []string{filepath.Join(root, "charts", "sbx", "crds")}
	if o.Upstream {
		crds = append(crds, filepath.Join(root, "charts", "agent-sandbox", "crds"))
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     crds,
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, err
	}

	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: o.Scheme, HTTPClient: hc})
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	e := &Env{Config: cfg, Client: c, env: env}
	e.stopFn = func() { utilnet.CloseIdleConnectionsFor(hc.Transport) }
	return e, nil
}

// Stop shuts the API server down and closes idle client connections.
func (e *Env) Stop() {
	if e.stopFn != nil {
		e.stopFn()
	}
	_ = e.env.Stop()
}

// NewScheme returns a scheme with the given AddToScheme functions applied.
func NewScheme(adders ...func(*k8sruntime.Scheme) error) (*k8sruntime.Scheme, error) {
	s := k8sruntime.NewScheme()
	for _, add := range adders {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("envtestx: go.mod not found")
		}
		dir = parent
	}
}
