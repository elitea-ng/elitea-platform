package eliteanative

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// KeySigner signs with one key of the ring. The private key never leaves
// it: a KMS-backed implementation calls the KMS sign operation, and
// LocalSigner holds a key loaded through the SecretStore seam or a mounted
// Secret.
type KeySigner interface {
	KeyID() string
	Algorithm() Algorithm
	Public() crypto.PublicKey
	// Sign signs digest, the SHA-256 of the JWS signing input, and answers
	// the JWS signature: r||s for ES256, PKCS #1 v1.5 for RS256.
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// RingKey is one key of the ring with its publication time. A zero
// NotBefore is an established key.
type RingKey struct {
	Signer    KeySigner
	NotBefore time.Time
}

// ForeignKey is a key of another purpose (command signing, code-sandbox
// grants, egress tokens, a TLS identity). The ring refuses to share its key
// id or its material. A zero Fingerprint compares the key id only.
type ForeignKey struct {
	Purpose     string
	KeyID       string
	Fingerprint [32]byte
}

// Fingerprint is the SHA-256 of a public key's PKIX encoding.
func Fingerprint(public crypto.PublicKey) ([32]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(der), nil
}

// ErrKeyReuse is a ring that shares a key id or key material with another
// purpose. It refuses boot.
var ErrKeyReuse = errors.New("elitea-native agent identity: the agent-assertion key ring reuses another purpose's key")

// ErrNoSigningKey means no key of the requested algorithm is usable yet.
var ErrNoSigningKey = errors.New("elitea-native agent identity: no usable signing key")

type ringKey struct {
	signer      KeySigner
	notBefore   time.Time // zero: established
	usableFrom  time.Time // zero: established
	fingerprint [32]byte
	jwk         jwk
}

// KeyRing is the dedicated agent-assertion key ring. Its signing method is
// unexported: only this package's Provider signs with it. The public half
// (JWKS) is readable by anyone.
type KeyRing struct {
	keys      []ringKey
	retention time.Duration
}

// NewKeyRing builds the ring. overlap is how long a new key is published
// before it signs; retention is how long a superseded key stays published
// (the longest assertion it may have signed). foreign lists the keys of
// every other purpose.
func NewKeyRing(keys []RingKey, overlap, retention time.Duration, foreign []ForeignKey) (*KeyRing, error) {
	if len(keys) == 0 || len(keys) > maxKeys {
		return nil, invalid("1..%d keys are required", maxKeys)
	}
	if overlap < minRotationOverlap || overlap > maxRotationOverlap || retention <= 0 {
		return nil, invalid("rotation overlap and retention are out of bounds")
	}
	ring := &KeyRing{retention: retention}
	kids := map[string]bool{}
	prints := map[[32]byte]bool{}
	established := map[Algorithm]bool{}
	for _, k := range keys {
		if k.Signer == nil {
			return nil, invalid("a key has no signer")
		}
		kid, alg := k.Signer.KeyID(), k.Signer.Algorithm()
		if !keyIDPattern.MatchString(kid) || kids[kid] || !alg.valid() {
			return nil, invalid("key %q: an invalid or duplicate kid, or an algorithm other than ES256/RS256", kid)
		}
		key, err := publicJWK(kid, alg, k.Signer.Public())
		if err != nil {
			return nil, err
		}
		fp, err := Fingerprint(k.Signer.Public())
		if err != nil {
			return nil, invalid("key %q: the public key cannot be encoded", kid)
		}
		if prints[fp] {
			return nil, invalid("key %q: the same key material is configured twice", kid)
		}
		for _, f := range foreign {
			if f.KeyID != "" && f.KeyID == kid {
				return nil, fmt.Errorf("%w: key %q has the key id of the %s key", ErrKeyReuse, kid, f.Purpose)
			}
			if f.Fingerprint != ([32]byte{}) && f.Fingerprint == fp {
				return nil, fmt.Errorf("%w: key %q is the key material of the %s key", ErrKeyReuse, kid, f.Purpose)
			}
		}
		entry := ringKey{signer: k.Signer, fingerprint: fp, jwk: key}
		if k.NotBefore.IsZero() {
			if established[alg] {
				return nil, invalid("at most one %s key may be established (no not_before)", alg)
			}
			established[alg] = true
		} else {
			entry.notBefore = k.NotBefore.UTC()
			entry.usableFrom = entry.notBefore.Add(overlap)
		}
		kids[kid], prints[fp] = true, true
		ring.keys = append(ring.keys, entry)
	}
	// Oldest first per algorithm: an established key, then by publication.
	sort.SliceStable(ring.keys, func(i, j int) bool { return ring.keys[i].usableFrom.Before(ring.keys[j].usableFrom) })
	return ring, nil
}

// Fingerprints answers the ring's key ids and material fingerprints, for a
// caller that checks another purpose against this one.
func (r *KeyRing) Fingerprints() []ForeignKey {
	out := make([]ForeignKey, 0, len(r.keys))
	for _, k := range r.keys {
		out = append(out, ForeignKey{Purpose: Purpose, KeyID: k.signer.KeyID(), Fingerprint: k.fingerprint})
	}
	return out
}

// Algorithms lists the algorithms the ring holds a key for.
func (r *KeyRing) Algorithms() []Algorithm {
	var out []Algorithm
	for _, alg := range []Algorithm{ES256, RS256} {
		for _, k := range r.keys {
			if k.signer.Algorithm() == alg {
				out = append(out, alg)
				break
			}
		}
	}
	return out
}

func (k ringKey) usable(now time.Time) bool {
	return k.usableFrom.IsZero() || !now.Before(k.usableFrom)
}

// active is the key of alg that signs at now: the most recently usable one.
func (r *KeyRing) active(alg Algorithm, now time.Time) (ringKey, bool) {
	var found ringKey
	ok := false
	for _, k := range r.keys {
		if k.signer.Algorithm() == alg && k.usable(now) {
			found, ok = k, true
		}
	}
	return found, ok
}

// published reports whether k is in the JWKS at now: its publication time
// passed, and no successor took over more than the retention ago.
func (r *KeyRing) published(k ringKey, now time.Time) bool {
	if !k.notBefore.IsZero() && now.Before(k.notBefore) {
		return false
	}
	for _, s := range r.keys {
		if s.signer.Algorithm() == k.signer.Algorithm() && s.usableFrom.After(k.usableFrom) &&
			s.usable(now) && !now.Before(s.usableFrom.Add(r.retention)) {
			return false
		}
	}
	return true
}

// sign signs the JWS signing input with the active key of alg.
func (r *KeyRing) sign(ctx context.Context, alg Algorithm, now time.Time, header func(kid string) ([]byte, error), payload []byte) (string, error) {
	key, ok := r.active(alg, now)
	if !ok {
		return "", ErrNoSigningKey
	}
	head, err := header(key.signer.KeyID())
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := key.signer.Sign(ctx, digest[:])
	if err != nil {
		return "", errors.New("elitea-native agent identity: the key signer failed")
	}
	if alg == ES256 && len(signature) != 64 {
		return "", errors.New("elitea-native agent identity: the key signer answered a malformed ES256 signature")
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// jwk is one public JSON Web Key (RFC 7517, RFC 7518 section 6).
type jwk struct {
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Curve     string `json:"crv,omitempty"`
	X         string `json:"x,omitempty"`
	Y         string `json:"y,omitempty"`
	N         string `json:"n,omitempty"`
	E         string `json:"e,omitempty"`
}

const minRSABits = 2048

func publicJWK(kid string, alg Algorithm, public crypto.PublicKey) (jwk, error) {
	switch alg {
	case ES256:
		key, ok := public.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() {
			return jwk{}, invalid("key %q: ES256 needs a P-256 key", kid)
		}
		raw, err := key.Bytes() // 0x04 || X || Y
		if err != nil || len(raw) != 65 {
			return jwk{}, invalid("key %q: the P-256 key cannot be encoded", kid)
		}
		return jwk{KeyType: "EC", Use: "sig", Algorithm: string(alg), KeyID: kid, Curve: "P-256",
			X: base64.RawURLEncoding.EncodeToString(raw[1:33]), Y: base64.RawURLEncoding.EncodeToString(raw[33:])}, nil
	case RS256:
		key, ok := public.(*rsa.PublicKey)
		if !ok || key.N.BitLen() < minRSABits {
			return jwk{}, invalid("key %q: RS256 needs an RSA key of at least %d bits", kid, minRSABits)
		}
		return jwk{KeyType: "RSA", Use: "sig", Algorithm: string(alg), KeyID: kid,
			N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}, nil
	}
	return jwk{}, invalid("key %q: the algorithm must be ES256 or RS256", kid)
}

// JWKS is the public key set at now: every published key, oldest first.
func (r *KeyRing) JWKS(now time.Time) ([]byte, error) {
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{}}
	for _, k := range r.keys {
		if r.published(k, now) {
			set.Keys = append(set.Keys, k.jwk)
		}
	}
	return json.Marshal(set)
}

// String keeps the ring's shape, never a key, in a formatted value.
func (r *KeyRing) String() string {
	if r == nil {
		return "KeyRing(nil)"
	}
	return fmt.Sprintf("KeyRing{purpose:%s keys:%d}", Purpose, len(r.keys))
}

// GoString keeps %#v from walking into a signer.
func (r *KeyRing) GoString() string { return r.String() }
