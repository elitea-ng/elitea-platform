// Package runtoken is the run token of the egress proxy (ADR-0034 decision 9,
// "Host allowlist: the egress proxy").
//
// At claim time elitea-main signs a short-lived token with a dedicated
// Ed25519 key. The token names one execution, its project, the run's host
// allowlist, a validity window and a nonce. The worker gives it to the
// sandbox only as the proxy credential (HTTPS_PROXY=http://run:<token>@...).
// Any proxy replica verifies it with the public key set, so there is no
// registration call and no shared state. A stolen token allows egress within
// that run's allowlist only, until it expires.
//
// # Wire format (version 1)
//
//		ert1.<kid>.<payload>.<signature>
//
//	  - kid is KeyID of the signing public key: 16 lower-case hex characters.
//	  - payload is the unpadded base64url encoding of the JSON claims.
//	  - signature is the unpadded base64url Ed25519 signature over the exact
//	    bytes "ert1.<kid>.<payload>".
//
// The verifier selects the key by kid, checks the signature over the bytes it
// received, and only then decodes the JSON (AGENTS.md: verify before decode).
// Every character is in the base64url alphabet or '.', so the token is safe as
// the password of a proxy URL without escaping.
//
// The package uses the standard library and egresslib only, because
// services/elitea-llm-gateway (outside go.work) requires this module by path.
package runtoken

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Prefix names the format version. A new format takes a new prefix.
	Prefix = "ert1"
	// MaxTokenBytes bounds a token on the wire. A larger token is refused
	// before any decoding.
	MaxTokenBytes = 8 << 10
	// MaxLifetime bounds the validity window a verifier accepts (expiry minus
	// not-before). It caps what a mis-minted or stolen token allows.
	MaxLifetime = 24 * time.Hour
	// ClockSkew is the tolerance for a not-before time in the future, so a
	// proxy whose clock runs a little behind Main accepts a fresh token. An
	// expiry gets no tolerance.
	ClockSkew = 30 * time.Second
	// NonceBytes is the size of a nonce NewNonce makes.
	NonceBytes = 16
	// maxIDLen bounds the execution id.
	maxIDLen = 128
)

// The refusals Verify returns. Each wraps one of these, so a caller can
// errors.Is it. None of them carries token text.
var (
	ErrMalformed     = errors.New("run token is malformed")
	ErrTooLarge      = errors.New("run token is too large")
	ErrUnknownKey    = errors.New("run token names an unknown key")
	ErrBadSignature  = errors.New("run token signature is not valid")
	ErrExpired       = errors.New("run token has expired")
	ErrNotYetValid   = errors.New("run token is not valid yet")
	ErrInvalidClaims = errors.New("run token claims are not valid")
)

// Claims is what a run token asserts.
type Claims struct {
	// ExecutionID names the run. It holds 1 to 128 characters from
	// [A-Za-z0-9._:-], so it is safe in a log line.
	ExecutionID string
	// ProjectID is the run's project. It is positive.
	ProjectID int64
	// Allowlist is the run's host allowlist, in the grammar of Allowlist.
	Allowlist []string
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time
	// Nonce makes two tokens for the same run distinct. It holds 16 to 64
	// base64url characters.
	Nonce string
	// KeyID names the signing key. Sign sets it; Verify checks that it equals
	// the kid of the key that verified the signature.
	KeyID string
}

// wireClaims is the JSON form. Times are Unix seconds.
type wireClaims struct {
	Version     int      `json:"v"`
	KeyID       string   `json:"kid"`
	ExecutionID string   `json:"eid"`
	ProjectID   int64    `json:"pid"`
	Allowlist   []string `json:"allow"`
	IssuedAt    int64    `json:"iat"`
	NotBefore   int64    `json:"nbf"`
	ExpiresAt   int64    `json:"exp"`
	Nonce       string   `json:"nonce"`
}

// Verified is a token that passed Verify: its claims and its parsed allowlist.
type Verified struct {
	Claims    Claims
	Allowlist *Allowlist
}

var b64 = base64.RawURLEncoding

// KeyID is the key id of a public key: the first 8 bytes of its SHA-256, in
// hex. It is derived, never configured, so a key and its id cannot disagree.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// NewNonce returns NonceBytes random bytes in base64url.
func NewNonce() (string, error) {
	buf := make([]byte, NonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("runtoken: read nonce: %w", err)
	}
	return b64.EncodeToString(buf), nil
}

// Signer signs run tokens with one Ed25519 private key.
type Signer struct {
	key ed25519.PrivateKey
	kid string
}

// NewSigner wraps a private key.
func NewSigner(key ed25519.PrivateKey) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("runtoken: the signing key is not an Ed25519 private key")
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("runtoken: the signing key has no Ed25519 public key")
	}
	return &Signer{key: key, kid: KeyID(pub)}, nil
}

// KeyID is the id of the signer's public key.
func (s *Signer) KeyID() string { return s.kid }

// PublicKey is the signer's public key, for the proxy's key set.
func (s *Signer) PublicKey() ed25519.PublicKey {
	pub, _ := s.key.Public().(ed25519.PublicKey)
	return pub
}

// Sign validates the claims and returns the token. It sets KeyID to the
// signer's kid. It refuses claims that Verify would refuse, apart from the
// validity window against the current time.
func (s *Signer) Sign(c Claims) (string, error) {
	c.KeyID = s.kid
	if err := checkClaims(c); err != nil {
		return "", err
	}
	if _, err := ParseAllowlist(c.Allowlist); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidClaims, err)
	}
	allow := c.Allowlist
	if allow == nil {
		allow = []string{}
	}
	payload, err := json.Marshal(wireClaims{
		Version:     1,
		KeyID:       c.KeyID,
		ExecutionID: c.ExecutionID,
		ProjectID:   c.ProjectID,
		Allowlist:   allow,
		IssuedAt:    c.IssuedAt.Unix(),
		NotBefore:   c.NotBefore.Unix(),
		ExpiresAt:   c.ExpiresAt.Unix(),
		Nonce:       c.Nonce,
	})
	if err != nil {
		return "", fmt.Errorf("runtoken: encode claims: %w", err)
	}
	signed := Prefix + "." + s.kid + "." + b64.EncodeToString(payload)
	token := signed + "." + b64.EncodeToString(ed25519.Sign(s.key, []byte(signed)))
	if len(token) > MaxTokenBytes {
		return "", fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, len(token), MaxTokenBytes)
	}
	return token, nil
}

// checkClaims is the claim shape both Sign and Verify require.
func checkClaims(c Claims) error {
	switch {
	case !validID(c.ExecutionID):
		return fmt.Errorf("%w: the execution id must be 1 to %d characters from [A-Za-z0-9._:-]", ErrInvalidClaims, maxIDLen)
	case c.ProjectID <= 0:
		return fmt.Errorf("%w: the project id must be positive", ErrInvalidClaims)
	case !validNonce(c.Nonce):
		return fmt.Errorf("%w: the nonce must be 16 to 64 base64url characters", ErrInvalidClaims)
	case c.NotBefore.IsZero() || c.ExpiresAt.IsZero() || c.IssuedAt.IsZero():
		return fmt.Errorf("%w: the issue, not-before and expiry times are required", ErrInvalidClaims)
	case !c.ExpiresAt.After(c.NotBefore):
		return fmt.Errorf("%w: the expiry must be after the not-before time", ErrInvalidClaims)
	case c.ExpiresAt.Sub(c.NotBefore) > MaxLifetime:
		return fmt.Errorf("%w: the validity window is longer than %s", ErrInvalidClaims, MaxLifetime)
	}
	return nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func validNonce(n string) bool {
	if len(n) < 16 || len(n) > 64 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// KeySet is the public keys a verifier accepts, by kid. It holds more than
// one key during a rotation. It is immutable.
type KeySet struct {
	keys map[string]ed25519.PublicKey
}

// NewKeySet builds a key set. It refuses an empty set and a key that is not
// an Ed25519 public key.
func NewKeySet(keys ...ed25519.PublicKey) (*KeySet, error) {
	if len(keys) == 0 {
		return nil, errors.New("runtoken: the key set is empty")
	}
	ks := &KeySet{keys: make(map[string]ed25519.PublicKey, len(keys))}
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return nil, errors.New("runtoken: a key in the set is not an Ed25519 public key")
		}
		ks.keys[KeyID(k)] = append(ed25519.PublicKey(nil), k...)
	}
	return ks, nil
}

// KeyIDs returns the kids in the set, for a startup log line.
func (ks *KeySet) KeyIDs() []string {
	if ks == nil {
		return nil
	}
	out := make([]string, 0, len(ks.keys))
	for kid := range ks.keys {
		out = append(out, kid)
	}
	return out
}

// Verify checks a token at time now and returns its claims and allowlist. It
// refuses a token that is too large, malformed, signed by a key outside the
// set, carries a bad signature, is expired or not yet valid, or carries
// claims that Sign would refuse. The error never holds token text.
func (ks *KeySet) Verify(token string, now time.Time) (*Verified, error) {
	if len(token) > MaxTokenBytes {
		return nil, ErrTooLarge
	}
	if ks == nil || len(ks.keys) == 0 {
		return nil, ErrUnknownKey
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != Prefix {
		return nil, ErrMalformed
	}
	kid := parts[1]
	pub, ok := ks.keys[kid]
	if !ok {
		return nil, ErrUnknownKey
	}
	sig, err := b64.Strict().DecodeString(parts[3])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrMalformed
	}
	signed := token[:len(token)-len(parts[3])-1]
	if !ed25519.Verify(pub, []byte(signed), sig) {
		return nil, ErrBadSignature
	}

	// Only signed bytes are decoded from here on.
	payload, err := b64.Strict().DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	var w wireClaims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil || dec.More() {
		return nil, ErrMalformed
	}
	if w.Version != 1 {
		return nil, fmt.Errorf("%w: version %d", ErrMalformed, w.Version)
	}
	if subtle.ConstantTimeCompare([]byte(w.KeyID), []byte(kid)) != 1 {
		return nil, fmt.Errorf("%w: the kid in the claims is not the signing kid", ErrInvalidClaims)
	}
	c := Claims{
		ExecutionID: w.ExecutionID,
		ProjectID:   w.ProjectID,
		Allowlist:   w.Allowlist,
		IssuedAt:    time.Unix(w.IssuedAt, 0),
		NotBefore:   time.Unix(w.NotBefore, 0),
		ExpiresAt:   time.Unix(w.ExpiresAt, 0),
		Nonce:       w.Nonce,
		KeyID:       w.KeyID,
	}
	if w.IssuedAt == 0 || w.NotBefore == 0 || w.ExpiresAt == 0 {
		return nil, fmt.Errorf("%w: the issue, not-before and expiry times are required", ErrInvalidClaims)
	}
	if err := checkClaims(c); err != nil {
		return nil, err
	}
	if !now.Before(c.ExpiresAt) {
		return nil, ErrExpired
	}
	if now.Add(ClockSkew).Before(c.NotBefore) {
		return nil, ErrNotYetValid
	}
	allow, err := ParseAllowlist(c.Allowlist)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClaims, err)
	}
	return &Verified{Claims: c, Allowlist: allow}, nil
}
