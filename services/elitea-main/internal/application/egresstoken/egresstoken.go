// Package egresstoken mints the egress proxy's run tokens (ADR-0034 decision
// 9, "Host allowlist: the egress proxy").
//
// At claim time, Main signs a short-lived token for one cloud-executor run
// whose project restricts network access to a host allowlist. The worker
// gives the token to the sandbox only as the proxy credential
// (HTTPS_PROXY=http://run:<token>@elitea-egress-proxy:3128). The proxy
// verifies it with the public key set and enforces the allowlist it carries.
// The token format and the allowlist grammar are libs/go/egresslib/runtoken.
//
// The signing key is a dedicated Ed25519 key, read from a file (a mounted
// Secret) named by ELITEA_EGRESS_TOKEN_SIGNING_KEY_FILE. It is not the key of
// any other signature in Main. Its key id is derived from the public key, so
// no key id is configured.
//
// The package is not wired yet. Phase B calls Load at the composition root
// and Mint at claim time.
package egresstoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
)

// EnvSigningKeyFile names the file that holds the PKCS#8 PEM Ed25519 signing
// key.
const EnvSigningKeyFile = "ELITEA_EGRESS_TOKEN_SIGNING_KEY_FILE"

// MinTTL is the shortest token Mint issues. Token times are whole seconds.
const MinTTL = time.Second

// MaxTTL is the longest token Mint issues: the verifier's limit.
const MaxTTL = runtoken.MaxLifetime

// ErrNotConfigured reports that no signing key file is configured. A caller
// that needs a token treats it as "allowlist mode is not available", never as
// "no restriction".
var ErrNotConfigured = errors.New("egresstoken: no signing key is configured (" + EnvSigningKeyFile + ")")

// Config is the minter's configuration.
type Config struct {
	// SigningKeyFile is the path of the PKCS#8 PEM Ed25519 private key.
	SigningKeyFile string
}

// ConfigFromEnv reads Config from the environment through getenv (os.Getenv
// in production, a map in tests).
func ConfigFromEnv(getenv func(string) string) Config {
	return Config{SigningKeyFile: strings.TrimSpace(getenv(EnvSigningKeyFile))}
}

// Minter mints run tokens. It is safe for concurrent use.
type Minter struct {
	signer *runtoken.Signer
	now    func() time.Time
	nonce  func() (string, error)
}

// Option configures a Minter.
type Option func(*Minter)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(m *Minter) {
		if now != nil {
			m.now = now
		}
	}
}

// New builds a minter over a private key.
func New(key ed25519.PrivateKey, opts ...Option) (*Minter, error) {
	signer, err := runtoken.NewSigner(key)
	if err != nil {
		return nil, err
	}
	m := &Minter{signer: signer, now: time.Now, nonce: runtoken.NewNonce}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Load builds a minter from the key file in cfg. It returns ErrNotConfigured
// when cfg names no file, and an error that names the file (never its
// content) when the file is missing or holds no Ed25519 private key.
func Load(cfg Config, opts ...Option) (*Minter, error) {
	if cfg.SigningKeyFile == "" {
		return nil, ErrNotConfigured
	}
	key, err := runtoken.LoadPrivateKeyFile(cfg.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("egresstoken: load the signing key from %s: %w", cfg.SigningKeyFile, err)
	}
	return New(key, opts...)
}

// KeyID is the id of the signing key. The proxy's key set must hold its
// public key; log it at startup so an operator can compare the two.
func (m *Minter) KeyID() string { return m.signer.KeyID() }

// PublicKey is the public half of the signing key, for the proxy's key set.
func (m *Minter) PublicKey() ed25519.PublicKey { return m.signer.PublicKey() }

// Mint signs a token for one run. The token is valid from now for ttl, which
// is the run's wall time. It refuses a ttl outside MinTTL..MaxTTL, an empty
// allowlist (a run with nothing to reach takes network `none`, not the proxy)
// and every claim the verifier would refuse. The token is a credential: the
// caller passes it to the sandbox only in the proxy URL and never logs it.
func (m *Minter) Mint(executionID string, projectID int64, allowlist []string, ttl time.Duration) (string, error) {
	if ttl < MinTTL || ttl > MaxTTL {
		return "", fmt.Errorf("egresstoken: the ttl %s is outside %s..%s", ttl, MinTTL, MaxTTL)
	}
	if len(allowlist) == 0 {
		return "", errors.New("egresstoken: the allowlist is empty; a run with no host to reach uses network none")
	}
	nonce, err := m.nonce()
	if err != nil {
		return "", err
	}
	now := m.now().Truncate(time.Second)
	return m.signer.Sign(runtoken.Claims{
		ExecutionID: executionID,
		ProjectID:   projectID,
		Allowlist:   append([]string(nil), allowlist...),
		IssuedAt:    now,
		NotBefore:   now,
		ExpiresAt:   now.Add(ttl.Truncate(time.Second)),
		Nonce:       nonce,
	})
}
