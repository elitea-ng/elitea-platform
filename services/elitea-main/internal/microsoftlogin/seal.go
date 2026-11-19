package microsoftlogin

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

// KeyLabel is the HKDF label of the sign-in custody key, recorded on every row
// (ADR-0057 decision 4). The stored login (story S1h) seals under the same label.
const KeyLabel = "elitea/microsoft-logins/v1"

// ErrCustodyIntegrity means a ciphertext did not open under its row's AAD.
var ErrCustodyIntegrity = errors.New("microsoftlogin: ciphertext does not match its record")

// binding is everything a sealed value is bound to. A ciphertext copied to another
// row, another user, another client or another scope does not open.
type binding struct {
	userID       int64
	projectID    int64
	credentialID string
	credType     string
	appRef       string
	clientKind   string
	challenge    string
	fingerprint  string
	authority    string
	scope        string
}

func (b binding) fields() []any {
	return []any{b.userID, b.projectID, b.credentialID, b.credType, b.appRef, b.clientKind, b.challenge,
		b.fingerprint, b.authority, b.scope}
}

// stateAAD binds a state's sealed PKCE verifier.
type stateAAD struct {
	digest      [32]byte
	redirectURI string
	expiresAt   time.Time
	binding
}

func (a stateAAD) bytes() []byte {
	fields := append([]any{"microsoft_login_state", KeyLabel, "verifier", a.digest[:], a.redirectURI,
		a.expiresAt.UTC().UnixMicro()}, a.fields()...)
	encoded, _ := json.Marshal(fields)
	return encoded
}

// handoffAAD binds a handoff's sealed access token to its row and to the person
// Microsoft reported.
type handoffAAD struct {
	handoffDigest [32]byte
	stateDigest   [32]byte
	tid, oid      string
	expiresAt     time.Time
	binding
}

func (a handoffAAD) bytes() []byte {
	fields := append([]any{"microsoft_login_handoff", KeyLabel, "access", a.handoffDigest[:], a.stateDigest[:],
		a.tid, a.oid, a.expiresAt.UTC().UnixMicro()}, a.fields()...)
	encoded, _ := json.Marshal(fields)
	return encoded
}

type aadSource interface{ bytes() []byte }

// sealer is the custody cipher: AES-256-GCM, random nonce, under a key derived by
// HKDF-SHA256 from the deployment master key.
type sealer struct{ aead cipher.AEAD }

func newSealer(masterKey []byte) (*sealer, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("microsoftlogin: a 32-byte master key is required")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, KeyLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("microsoftlogin: derive the custody key: %w", err)
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

func (s *sealer) seal(plain string, aad aadSource) []byte {
	plaintext := []byte(plain)
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

// digestOf is the stored form of a state value or a handoff code: only the digest
// is ever written, so a read of the table yields nothing redeemable.
func digestOf(purpose, value string) [32]byte {
	return sha256.Sum256([]byte("elitea/microsoft-login/" + purpose + "/v1\x00" + value))
}
