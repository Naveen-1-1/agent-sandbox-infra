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
)

// MinKeyBytes is the shortest accepted master key.
const MinKeyBytes = 32

// ErrInvalid means a presented token did not match.
var ErrInvalid = errors.New("token: invalid")

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
