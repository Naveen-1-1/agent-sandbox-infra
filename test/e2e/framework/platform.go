//go:build e2e

package framework

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Platform is what the deployed chart tells a test about itself.
type Platform struct {
	Keyring      token.Keyring
	RuntimeImage string
	BrokerAddr   string
	FixturesURL  string // base URL of the fixtures git server, in-cluster
}

// DeployedPlatform reads the master key and the controller's settings.
func DeployedPlatform(t testing.TB, c *Clients) Platform {
	t.Helper()
	ns := Namespace()
	keys, err := token.NewKeyring([]byte(strings.TrimSpace(SecretValue(t, ns, "sbx-master-key", "key"))))
	if err != nil {
		t.Fatal(err)
	}
	dep, err := c.Kube.AppsV1().Deployments(ns).Get(Ctx(t, time.Minute), "sbx-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := Platform{Keyring: keys, FixturesURL: "http://sbx-fixtures." + ns + ".svc"}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		switch e.Name {
		case "SBX_RUNTIME_IMAGE":
			p.RuntimeImage = e.Value
		case "SBX_BROKER_ADDR":
			p.BrokerAddr = e.Value
		}
	}
	if p.RuntimeImage == "" || p.BrokerAddr == "" {
		t.Fatal("controller deployment lacks SBX_RUNTIME_IMAGE or SBX_BROKER_ADDR; run make deploy")
	}
	return p
}

// Dial opens a plaintext gRPC connection closed when the test ends.
func Dial(t testing.TB, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
