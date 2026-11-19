package microsoftapps

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// KeyLabel is the HKDF label of the registry's custody key, recorded on every
// row (ADR-0057 decision 4).
const KeyLabel = "elitea/microsoft-oauth-apps/v1"

// CurrentKeyVersion is the key version new ciphertext is sealed under. Key
// rotation follows ADR-0039 A1.2: a row records its version and a background job
// re-seals; until then the version is 1.
const CurrentKeyVersion = 1

// Secret is a client secret in memory. It redacts itself in every format, log
// and JSON encoding, so a struct that carries one cannot leak it through a
// handler that marshals the struct, a %v in an error, or a slog attribute. The
// only way to the plaintext is Reveal, and its callers are the code exchange and
// the refresh in this process.
type Secret struct{ value string }

// NewSecret wraps a plaintext.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the plaintext. Call it only to build the request to Entra's
// token endpoint.
func (s Secret) Reveal() string { return s.value }

// Empty reports no secret.
func (s Secret) Empty() bool { return s.value == "" }

const redacted = "[redacted]"

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// secretAAD is the additional data of a sealed client secret. It binds the
// ciphertext to its row, its client and its key: copied to another app, kept after
// the client id changed, or opened under another key version, it does not open. The
// authority is not bound: an Entra secret belongs to the app registration, not to
// the tenant that signs in, so moving an app between tenants keeps a valid secret.
type secretAAD struct {
	AppID      string
	ClientID   string
	KeyVersion int
}

func (a secretAAD) bytes() []byte {
	encoded, _ := json.Marshal([]any{
		"microsoft_oauth_app", KeyLabel, "client_secret", a.AppID, a.ClientID, a.KeyVersion,
	})
	return encoded
}

// sealer is the custody cipher: AES-256-GCM, random nonce, under a key derived by
// HKDF-SHA256 from the deployment master key.
type sealer struct {
	aead   cipher.AEAD
	macKey []byte
}

func newSealer(masterKey []byte) (*sealer, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("microsoftapps: a 32-byte master key is required")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, KeyLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("microsoftapps: derive the custody key: %w", err)
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
	macKey, err := hkdf.Key(sha256.New, masterKey, nil, SeedDigestLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("microsoftapps: derive the digest key: %w", err)
	}
	return &sealer{aead: aead, macKey: macKey}, nil
}

// SeedDigestLabel is the HKDF label of the key that digests a Helm seed entry.
const SeedDigestLabel = "elitea/microsoft-oauth-apps/seed-digest/v1"

// digest is a keyed digest of data: the same input gives the same digest, and the
// digest reveals nothing about a secret in it to anyone without the master key.
func (s *sealer) digest(data []byte) string {
	mac := hmac.New(sha256.New, s.macKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *sealer) seal(secret string, aad secretAAD) []byte {
	plaintext := []byte(secret)
	defer clear(plaintext)
	return s.aead.Seal(nil, nil, plaintext, aad.bytes())
}

func (s *sealer) open(ciphertext []byte, aad secretAAD) (Secret, error) {
	plaintext, err := s.aead.Open(nil, nil, ciphertext, aad.bytes())
	if err != nil {
		return Secret{}, ErrCustodyIntegrity
	}
	defer clear(plaintext)
	return NewSecret(string(plaintext)), nil
}
