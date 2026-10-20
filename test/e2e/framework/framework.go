//go:build e2e

// Package framework holds helpers for end-to-end tests against the kind
// cluster created by `make kind-up` and populated by `make deploy`.
package framework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Namespace is where the platform is installed.
func Namespace() string {
	if ns := os.Getenv("SBX_NAMESPACE"); ns != "" {
		return ns
	}
	return "sbx-system"
}

// Kubectl runs kubectl against the test cluster and returns trimmed stdout.
func Kubectl(t testing.TB, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// SecretValue reads one key of a Secret.
func SecretValue(t testing.TB, ns, name, key string) string {
	t.Helper()
	out := Kubectl(t, "-n", ns, "get", "secret", name, "-o", "jsonpath={.data."+key+"}")
	dec, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("secret %s/%s key %s: %v", ns, name, key, err)
	}
	return string(dec)
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// PortForward forwards a free local port to remotePort of target (for
// example "svc/sbx-s3") and returns "127.0.0.1:<port>". The forward stops
// when the test ends.
func PortForward(t testing.TB, ns, target string, remotePort int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "-n", ns, "port-forward", "--address", "127.0.0.1", target, ":"+strconv.Itoa(remotePort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}

	addr := make(chan string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwarding.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case addr <- "127.0.0.1:" + m[1]:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		<-drained
	})

	select {
	case a := <-addr:
		return a
	case <-drained:
		t.Fatalf("kubectl port-forward %s exited before forwarding", target)
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for port-forward to %s", target)
	}
	return ""
}
