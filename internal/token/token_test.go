package token_test

import (
	"bytes"
	"errors"
	"testing"

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
