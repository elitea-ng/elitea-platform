package eliteanative

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// LocalSigner holds one private key in this process. The key is parsed from
// the material once and never returned; every formatting path prints the
// key id only.
type LocalSigner struct {
	kid    string
	alg    Algorithm
	key    crypto.Signer
	public crypto.PublicKey
}

// NewLocalSigner parses one PKCS #8 "PRIVATE KEY" PEM block. ES256 needs a
// P-256 key, RS256 an RSA key of at least 2048 bits. material is destroyed
// before return, whatever the outcome.
func NewLocalSigner(kid string, alg Algorithm, material secretstore.RuntimeSecret) (*LocalSigner, error) {
	defer material.Destroy()
	block, rest := pem.Decode(material.UnsafeBytes())
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, invalid("key %q must be one PKCS#8 PRIVATE KEY PEM block", kid)
	}
	defer clear(block.Bytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, invalid("key %q is not a PKCS#8 private key", kid)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, invalid("key %q cannot sign", kid)
	}
	switch parsed.(type) {
	case *ecdsa.PrivateKey:
		if alg != ES256 {
			return nil, invalid("key %q is an EC key; its alg must be ES256", kid)
		}
	case *rsa.PrivateKey:
		if alg != RS256 {
			return nil, invalid("key %q is an RSA key; its alg must be RS256", kid)
		}
	default:
		// Ed25519 and anything else: EdDSA is not offered.
		return nil, invalid("key %q must be a P-256 (ES256) or RSA (RS256) key", kid)
	}
	if _, err := publicJWK(kid, alg, signer.Public()); err != nil {
		return nil, err
	}
	return &LocalSigner{kid: kid, alg: alg, key: signer, public: signer.Public()}, nil
}

func (s *LocalSigner) KeyID() string            { return s.kid }
func (s *LocalSigner) Algorithm() Algorithm     { return s.alg }
func (s *LocalSigner) Public() crypto.PublicKey { return s.public }

// Sign answers the JWS signature of digest.
func (s *LocalSigner) Sign(_ context.Context, digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, errors.New("elitea-native agent identity: the digest must be SHA-256")
	}
	switch key := s.key.(type) {
	case *ecdsa.PrivateKey:
		r, sv, err := ecdsa.Sign(rand.Reader, key, digest)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		sv.FillBytes(out[32:])
		return out, nil
	case *rsa.PrivateKey:
		return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest)
	}
	return nil, errors.New("elitea-native agent identity: unsupported key")
}

func (s *LocalSigner) String() string {
	return fmt.Sprintf("LocalSigner{kid:%s alg:%s key:%s}", s.kid, s.alg, redacted)
}

// GoString keeps %#v from printing the key.
func (s *LocalSigner) GoString() string { return s.String() }

// Format makes every verb print the redacted form.
func (s *LocalSigner) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(s.String())) }

// LogValue keeps slog from walking into the key.
func (s *LocalSigner) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kid", s.kid), slog.String("alg", string(s.alg)), slog.String("key", redacted))
}

const redacted = "[REDACTED]"

var _ KeySigner = (*LocalSigner)(nil)
