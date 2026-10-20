// Package brokertest runs a real broker in-process, backed by the in-memory
// object store, for tests of the broker and of its clients.
package brokertest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Env is a running broker.
type Env struct {
	Addr    string
	Store   *objstore.Mem
	Keyring token.Keyring

	mu         sync.Mutex
	workspaces map[string]*sbxv1.Workspace
}

// Start runs a broker until the test ends. maxBundle limits snapshot uploads
// (0 = no limit).
func Start(t testing.TB, maxBundle int64) *Env {
	t.Helper()
	k, err := token.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	mem := objstore.NewMem()
	s3 := httptest.NewServer(mem.Handler())
	t.Cleanup(s3.Close)
	mem.SetBaseURL(s3.URL)

	e := &Env{Store: mem, Keyring: k, workspaces: map[string]*sbxv1.Workspace{}}
	b := broker.New(broker.Config{Keyring: k, Store: mem, Workspaces: e, MaxBundleBytes: maxBundle})
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(b.UnaryInterceptor()))
	b.Register(g)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)
	e.Addr = l.Addr().String()
	return e
}

// Set stores (or replaces) a workspace the broker will see.
func (e *Env) Set(ws *sbxv1.Workspace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspaces[ws.Namespace+"/"+ws.Name] = ws.DeepCopy()
}

// Get implements broker.Workspaces.
func (e *Env) Get(_ context.Context, namespace, name string) (*sbxv1.Workspace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ws, ok := e.workspaces[namespace+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "sbx.dev", Resource: "workspaces"}, name)
	}
	return ws.DeepCopy(), nil
}

// Token returns the broker token a pod of ws at epoch would hold.
func (e *Env) Token(ws *sbxv1.Workspace, epoch int64) string {
	return e.Keyring.Derive(token.Broker, string(ws.UID), epoch)
}

// Client dials the broker as a pod of ws at epoch.
func (e *Env) Client(t testing.TB, ws *sbxv1.Workspace, epoch int64) brokerv1.BrokerServiceClient {
	t.Helper()
	return e.ClientWithToken(t, ws, epoch, e.Token(ws, epoch))
}

// ClientWithToken dials the broker presenting tok.
func (e *Env) ClientWithToken(t testing.TB, ws *sbxv1.Workspace, epoch int64, tok string) brokerv1.BrokerServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(e.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		broker.Credentials(tok, fmt.Sprintf("%s/%s", ws.Namespace, ws.Name), epoch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return brokerv1.NewBrokerServiceClient(conn)
}
