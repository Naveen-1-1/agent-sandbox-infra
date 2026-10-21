// Package token derives the per-workspace, per-epoch bearer tokens that
// authenticate calls between the platform and workspace pods.
//
// Every token is HMAC-SHA256(master key, purpose|workspace UID|epoch). The
// controller and gateway hold the master key and can recompute any token
// without storing it; a pod only receives its own epoch's tokens, so it can't
// mint tokens for another workspace or for a later or earlier epoch.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Purpose scopes a token to one kind of caller and callee.
type Purpose string

const (
	// Sidecar authenticates the controller to a pod's sidecar.
	Sidecar Purpose = "sidecar"
	// Agent authenticates the controller and gateway to a pod's agent and to
	// upstream sandboxd's exec and files APIs.
	Agent Purpose = "agent"
	// Broker authenticates a pod's sidecar to the broker.
	Broker Purpose = "broker"
	// Gateway authenticates a client (an agent harness, a browser) to one
	// workspace through the gateway. It is derived with epoch 0, so it stays
	// valid across wakes.
	Gateway Purpose = "gateway"
	// Ticket is a short-lived login link to a workspace's editor or app host.
	Ticket Purpose = "ticket"
	// Session is a browser's cookie for one workspace host.
	Session Purpose = "session"
)

// MinKeyBytes is the shortest accepted master key.
const MinKeyBytes = 32

// ErrInvalid means a presented token did not match.
var ErrInvalid = errors.New("token: invalid")

// ErrExpired means a signed token is past its expiry.
var ErrExpired = errors.New("token: expired")

// Keyring derives tokens from a master key.
type Keyring struct{ key []byte }

// NewKeyring returns a Keyring for key, which must be at least MinKeyBytes.
func NewKeyring(key []byte) (Keyring, error) {
	if len(key) < MinKeyBytes {
		return Keyring{}, errors.New("token: master key must be at least 32 bytes")
	}
	return Keyring{key: append([]byte(nil), key...)}, nil
}

// Derive returns the token for purpose, workspace and epoch.
func (k Keyring) Derive(p Purpose, workspaceUID string, epoch int64) string {
	mac := hmac.New(sha256.New, k.key)
	mac.Write([]byte(strings.Join([]string{"sbx-v1", string(p), workspaceUID, strconv.FormatInt(epoch, 10)}, "|")))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks presented in constant time.
func (k Keyring) Verify(presented string, p Purpose, workspaceUID string, epoch int64) error {
	return Equal(presented, k.Derive(p, workspaceUID, epoch))
}

// Sign returns a token for purpose that is only good for one workspace, on
// one host (a browser origin, without the port: cookies ignore ports), until
// expires: "<unix expiry>.<mac>".
func (k Keyring) Sign(p Purpose, workspaceUID, host string, expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, k.key)
	mac.Write([]byte(strings.Join([]string{"sbx-v1", string(p), workspaceUID, host, exp}, "|")))
	return exp + "." + hex.EncodeToString(mac.Sum(nil))
}

// Check verifies a token from Sign and that it hasn't expired at now.
func (k Keyring) Check(presented string, p Purpose, workspaceUID, host string, now time.Time) error {
	exp, _, ok := strings.Cut(presented, ".")
	n, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil {
		return ErrInvalid
	}
	if err := Equal(presented, k.Sign(p, workspaceUID, host, time.Unix(n, 0))); err != nil {
		return err
	}
	if now.Unix() > n {
		return ErrExpired
	}
	return nil
}

// Equal compares two tokens in constant time.
func Equal(presented, want string) error {
	if want == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(want)) != 1 {
		return ErrInvalid
	}
	return nil
}

// SHA256 returns the digest upstream sandboxd expects for its bearer check.
func SHA256(tok string) []byte {
	s := sha256.Sum256([]byte(tok))
	return s[:]
}

// FromHeader extracts the token from an "Authorization: Bearer <token>" value.
func FromHeader(v string) (string, bool) {
	const prefix = "Bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(v[len(prefix):]), true
}
