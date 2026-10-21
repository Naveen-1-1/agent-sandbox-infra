package token_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func keyring(t *testing.T, seed byte) token.Keyring {
	t.Helper()
	k, err := token.NewKeyring(bytes.Repeat([]byte{seed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTokensAreScoped(t *testing.T) {
	k := keyring(t, 1)
	tok := k.Derive(token.Broker, "ws-a", 3)
	if err := k.Verify(tok, token.Broker, "ws-a", 3); err != nil {
		t.Fatalf("own token rejected: %v", err)
	}
	for name, c := range map[string]struct {
		p     token.Purpose
		ws    string
		epoch int64
	}{
		"other purpose":   {token.Sidecar, "ws-a", 3},
		"other workspace": {token.Broker, "ws-b", 3},
		"older epoch":     {token.Broker, "ws-a", 2},
		"newer epoch":     {token.Broker, "ws-a", 4},
	} {
		if err := k.Verify(tok, c.p, c.ws, c.epoch); !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if err := keyring(t, 2).Verify(tok, token.Broker, "ws-a", 3); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("token from another master key accepted: %v", err)
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := token.NewKeyring([]byte("short")); err == nil {
		t.Fatal("NewKeyring accepted a 5-byte key")
	}
}

func TestEmptyWantNeverMatches(t *testing.T) {
	if err := token.Equal("", ""); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("empty tokens matched: %v", err)
	}
}

func TestFromHeader(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer ": ""} {
		got, ok := token.FromHeader(in)
		if got != want || ok != (want != "") {
			t.Errorf("FromHeader(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestSignedTokensAreScopedAndExpire(t *testing.T) {
	k := keyring(t, 1)
	now := time.Unix(1_800_000_000, 0)
	tok := k.Sign(token.Session, "uid-a", "demo.team.sbx.localhost", now.Add(time.Hour))
	if err := k.Check(tok, token.Session, "uid-a", "demo.team.sbx.localhost", now); err != nil {
		t.Fatalf("own token: %v", err)
	}
	for name, err := range map[string]error{
		"another purpose":   k.Check(tok, token.Ticket, "uid-a", "demo.team.sbx.localhost", now),
		"another workspace": k.Check(tok, token.Session, "uid-b", "demo.team.sbx.localhost", now),
		"another host":      k.Check(tok, token.Session, "uid-a", "3000.demo.team.sbx.localhost", now),
		"tampered expiry":   k.Check("9999999999"+tok[strings.Index(tok, "."):], token.Session, "uid-a", "demo.team.sbx.localhost", now),
		"garbage":           k.Check("not-a-token", token.Session, "uid-a", "demo.team.sbx.localhost", now),
	} {
		if !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if err := k.Check(tok, token.Session, "uid-a", "demo.team.sbx.localhost", now.Add(2*time.Hour)); !errors.Is(err, token.ErrExpired) {
		t.Errorf("after expiry: %v, want ErrExpired", err)
	}
}
