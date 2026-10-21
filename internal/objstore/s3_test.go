package objstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
)

// isolate keeps the developer's own AWS setup out of a test.
func isolate(t *testing.T) {
	t.Helper()
	none := filepath.Join(t.TempDir(), "none")
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_CONFIG_FILE": none, "AWS_SHARED_CREDENTIALS_FILE": none, "AWS_WEB_IDENTITY_TOKEN_FILE": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": "", "AWS_CONTAINER_AUTHORIZATION_TOKEN": "",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
}

// fakeS3 records the requests it gets and answers with status.
type fakeS3 struct {
	mu     sync.Mutex
	got    []*http.Request
	bodies []string
	status func(r *http.Request) int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, r.Clone(context.Background()))
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	w.WriteHeader(f.status(r))
}

// TestAmbientCredentialsFromPodIdentity: with no keys configured, requests
// are signed with what the EKS Pod Identity agent hands out.
func TestAmbientCredentialsFromPodIdentity(t *testing.T) {
	isolate(t)
	var asked int
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get("Authorization") != "pod-token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"AccessKeyId": "AKIAPODIDENTITY", "SecretAccessKey": "secret", "Token": "session-token",
			"Expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer agent.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "pod-token")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", agent.URL+"/v1/credentials")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", tokenFile)

	fake := &fakeS3{status: func(*http.Request) int { return http.StatusNotFound }}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s, err := objstore.NewS3(objstore.S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Head(context.Background(), "ws/u/manifest.json"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("head: %v", err)
	}
	if asked != 1 || len(fake.got) != 1 {
		t.Fatalf("credentials asked %d times, S3 got %d requests", asked, len(fake.got))
	}
	r := fake.got[0]
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIAPODIDENTITY/") || r.Header.Get("X-Amz-Security-Token") != "session-token" {
		t.Errorf("signed with %q, token %q", r.Header.Get("Authorization"), r.Header.Get("X-Amz-Security-Token"))
	}

	// Presigned URLs carry the session token too, so pods can use them.
	req, err := s.PresignGet(context.Background(), "ws/u/b/x.bundle", time.Minute)
	if err != nil || !strings.Contains(req.URL, "X-Amz-Security-Token=session-token") {
		t.Errorf("presigned %q, %v", req.URL, err)
	}
}

func TestStaticKeysNeedBoth(t *testing.T) {
	isolate(t)
	if _, err := objstore.NewS3(objstore.S3Config{Bucket: "b", AccessKey: "only"}); err == nil {
		t.Error("an access key without a secret was accepted")
	}
}

func TestEnsureBucket(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	newStore := func(region string, head int) (*objstore.S3, *fakeS3) {
		fake := &fakeS3{status: func(r *http.Request) int {
			if r.Method == http.MethodHead {
				return head
			}
			return http.StatusOK
		}}
		srv := httptest.NewServer(fake)
		t.Cleanup(srv.Close)
		s, err := objstore.NewS3(objstore.S3Config{Endpoint: srv.URL, Region: region, Bucket: "b", PathStyle: true, AccessKey: "k", SecretKey: "s"})
		if err != nil {
			t.Fatal(err)
		}
		return s, fake
	}

	s, fake := newStore("eu-west-1", http.StatusNotFound)
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.got) != 2 || fake.got[1].Method != http.MethodPut || !strings.Contains(fake.bodies[1], "<LocationConstraint>eu-west-1</LocationConstraint>") {
		t.Errorf("outside us-east-1 the bucket needs its region: %d requests, body %q", len(fake.got), fake.bodies)
	}
	s, fake = newStore("us-east-1", http.StatusNotFound)
	if err := s.EnsureBucket(ctx); err != nil || strings.Contains(fake.bodies[1], "LocationConstraint") {
		t.Errorf("us-east-1: %v, body %q", err, fake.bodies[1])
	}
	s, fake = newStore("us-east-1", http.StatusForbidden)
	if err := s.EnsureBucket(ctx); err == nil || len(fake.got) != 1 {
		t.Errorf("access denied: %v after %d requests; want an error and no create", err, len(fake.got))
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
