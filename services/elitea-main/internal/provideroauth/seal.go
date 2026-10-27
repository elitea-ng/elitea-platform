// Package provideroauth holds provider delegated OAuth grants in encrypted
// custody (ADR-0039 decision 3, issue #1121 items 3 and 4): the user
// consented to the provider directly, for example with Atlassian OAuth 2.0
// (3LO) or Aha! OAuth2. That consent is its own authority source; no Entra
// delegation is involved.
//
// Three parts, all in shared migration 0190:
//
//   - Consent: the start/callback flow behind
//     /api/v2/credential_consent/{provider}:start|callback. The state is
//     random, bound server side to the user, tenant project, agent binding,
//     target binding and provider, short lived and single use. PKCE is S256.
//     The redirect URI is the configured one, sent byte for byte in the
//     authorization request and the code exchange.
//   - Grants: one row per (tenant project, provider, target binding, subject
//     user, agent binding) with the sealed refresh token and current access
//     token.
//   - TokenSource: answers a valid access token for a grant, refreshing it
//     once across every Main replica (internal/credentialbroker/refresh)
//     when it is within the skew of its expiry. A rotated refresh token is
//     written in the same fenced transaction as the new access token.
//
// Ciphertext is AES-256-GCM under a key derived by HKDF-SHA256 from the
// deployment master key with the label KeyLabel. The AAD binds tenant |
// grant id | provider | target | subject | agent binding, and which token it
// is, so a ciphertext moved to another row or column does not decrypt.
//
// No token, code, verifier or provider response body is logged or put in an
// error.
package provideroauth

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
const KeyLabel = "elitea/provider-oauth-grants/v1"

// ErrCustodyIntegrity means a ciphertext did not open under its row's AAD.
var ErrCustodyIntegrity = errors.New("provideroauth: ciphertext does not match its record")

// tokenPurpose names which token a grant ciphertext holds.
type tokenPurpose string

const (
	purposeAccess  tokenPurpose = "access"
	purposeRefresh tokenPurpose = "refresh"
)

// GrantAAD is the additional data of a grant ciphertext: tenant | grant id |
// provider | target | subject | agent binding, plus the token's purpose.
type GrantAAD struct {
	TenantProjectID int64
	GrantID         string
	Provider        string
	TargetBindingID string
	SubjectUserID   int64
	AgentBindingID  string
	purpose         tokenPurpose
}

func (a GrantAAD) bytes() []byte {
	encoded, _ := json.Marshal([]any{
		"provider_grant", KeyLabel, string(a.purpose), a.TenantProjectID, a.GrantID, a.Provider,
		a.TargetBindingID, a.SubjectUserID, a.AgentBindingID,
	})
	return encoded
}

func (a GrantAAD) with(p tokenPurpose) GrantAAD {
	a.purpose = p
	return a
}

// stateAAD is the additional data of a consent state's sealed PKCE verifier.
type stateAAD struct {
	StateDigest     [32]byte
	Provider        string
	TenantProjectID int64
	UserID          int64
	AgentBindingID  string
	TargetBindingID string
	RedirectURI     string
	ExpiresAt       time.Time
}

func (a stateAAD) bytes() []byte {
	encoded, _ := json.Marshal([]any{
		"consent_state", KeyLabel, a.StateDigest[:], a.Provider, a.TenantProjectID, a.UserID,
		a.AgentBindingID, a.TargetBindingID, a.RedirectURI, a.ExpiresAt.UTC().UnixMicro(),
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
		return nil, errors.New("provideroauth: a 32-byte master key is required")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, KeyLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("provideroauth: derive the custody key: %w", err)
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

func (s *sealer) seal(secret string, aad aadSource) []byte {
	plaintext := []byte(secret)
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

// stateDigest is the stored form of a consent state value.
func stateDigest(state string) [32]byte {
	return sha256.Sum256([]byte("elitea/provider-oauth-state/v1\x00" + state))
}
