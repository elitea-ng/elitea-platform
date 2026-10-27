// Package delegationstore holds Entra user assertions in encrypted custody
// (ADR-0039 decisions 3 and 4, issue #1117).
//
// Two stages, both in elitea_identity (shared migration 0185):
//
//  1. A governed Entra sign-in retains the middle-tier access token as a
//     sign-in assertion, bound to the browser session that was created for
//     it (RetainSignIn). No agent binding exists yet.
//  2. When a governed run needs interactive delegation, Delegate copies that
//     assertion into a delegation row of one tenant project and one agent
//     binding (the 0181 table), re-sealed under the delegation's own AAD.
//
// Load returns the assertion of an active delegation as an obo.Assertion,
// the only form an exchanger accepts. Revoke and RevokeSession set state
// 'revoked', clear the ciphertext and bump revocation_epoch in one
// transaction, with a security audit event in the same transaction.
//
// Ciphertext is AES-256-GCM under a key derived by HKDF-SHA256 from the
// deployment master key with the label KeyLabel. The AAD binds every
// identity column, so a ciphertext moved to another row, or a row whose
// tenant, user, binding, audience or expiry was edited, does not decrypt.
//
// No token is logged or put in an error, and a browser session id is stored
// only as its SHA-256 digest.
package delegationstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KeyLabel is the HKDF label of the custody key, recorded on every row.
const KeyLabel = "elitea/entra-delegations/v1"

// ErrCustodyIntegrity means a ciphertext did not open under its row's AAD.
var ErrCustodyIntegrity = errors.New("delegationstore: assertion ciphertext does not match its record")

// DelegationAAD is the additional data of a delegation ciphertext:
// tenant_project_id | delegation_id | tid | oid | agent_binding_id |
// audience | expires_at.
type DelegationAAD struct {
	TenantProjectID int64
	DelegationID    string
	TenantID        string
	ObjectID        string
	AgentBindingID  string
	Audience        string
	ExpiresAt       time.Time
}

func (a DelegationAAD) bytes() []byte {
	encoded, _ := json.Marshal([]any{
		"delegation", KeyLabel, a.TenantProjectID, a.DelegationID, a.TenantID, a.ObjectID,
		a.AgentBindingID, a.Audience, a.ExpiresAt.UTC().UnixMicro(),
	})
	return encoded
}

// SignInAAD is the additional data of a sign-in assertion ciphertext.
type SignInAAD struct {
	AssertionID   string
	SessionDigest [32]byte
	UserID        int64
	TenantID      string
	ObjectID      string
	Audience      string
	ExpiresAt     time.Time
}

func (a SignInAAD) bytes() []byte {
	encoded, _ := json.Marshal([]any{
		"signin", KeyLabel, a.AssertionID, a.SessionDigest[:], a.UserID, a.TenantID, a.ObjectID,
		a.Audience, a.ExpiresAt.UTC().UnixMicro(),
	})
	return encoded
}

type aadSource interface{ bytes() []byte }

// sealer is the custody cipher.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(masterKey []byte) (*sealer, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("delegationstore: a 32-byte master key is required")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, KeyLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("delegationstore: derive the custody key: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(token string, aad aadSource) []byte {
	plaintext := []byte(token)
	defer clear(plaintext)
	return s.aead.Seal(nil, nil, plaintext, aad.bytes())
}

func (s *sealer) open(ciphertext []byte, aad aadSource) (string, error) {
	plaintext, err := s.aead.Open(nil, nil, ciphertext, aad.bytes())
	if err != nil {
		return "", ErrCustodyIntegrity
	}
	defer clear(plaintext)
	return string(plaintext), nil
}

// SessionDigest is the stored form of a browser session id.
func SessionDigest(sessionID string) [32]byte {
	return sha256.Sum256([]byte("elitea/browser-session/v1\x00" + sessionID))
}
