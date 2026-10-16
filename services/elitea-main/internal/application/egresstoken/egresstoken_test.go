package egresstoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

func writeKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := runtoken.MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "egress-token.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pub
}

func TestMintVerifiesAtTheProxy(t *testing.T) {
	path, pub := writeKey(t)
	now := time.Unix(1_800_000_000, 500_000_000)
	m, err := Load(ConfigFromEnv(func(k string) string {
		if k == EnvSigningKeyFile {
			return " " + path + " "
		}
		return ""
	}), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if m.KeyID() != runtoken.KeyID(pub) || !m.PublicKey().Equal(pub) {
		t.Fatal("the minter must sign with the key in the file")
	}
	tok, err := m.Mint("exec-1", 7, []string{"api.github.com", "*.pypi.org"}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ks, _ := runtoken.NewKeySet(pub)
	v, err := ks.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	c := v.Claims
	if c.ExecutionID != "exec-1" || c.ProjectID != 7 || c.KeyID != m.KeyID() || len(c.Nonce) < 16 {
		t.Fatalf("claims: %+v", c)
	}
	if !c.NotBefore.Equal(now.Truncate(time.Second)) || c.ExpiresAt.Sub(c.NotBefore) != 15*time.Minute {
		t.Fatalf("window: %s .. %s", c.NotBefore, c.ExpiresAt)
	}
	if _, err := ks.Verify(tok, now.Add(15*time.Minute)); !errors.Is(err, runtoken.ErrExpired) {
		t.Fatalf("the token must expire after the ttl: %v", err)
	}
	// Two tokens for one run are distinct.
	again, _ := m.Mint("exec-1", 7, []string{"api.github.com"}, time.Minute)
	if again == tok {
		t.Fatal("the nonce must make each token distinct")
	}
}

func TestMintRefusals(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	m, err := New(priv)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() (string, error){
		"zero ttl":        func() (string, error) { return m.Mint("e", 1, []string{"a.example"}, 0) },
		"sub-second ttl":  func() (string, error) { return m.Mint("e", 1, []string{"a.example"}, time.Millisecond) },
		"ttl over max":    func() (string, error) { return m.Mint("e", 1, []string{"a.example"}, MaxTTL+time.Second) },
		"empty allowlist": func() (string, error) { return m.Mint("e", 1, nil, time.Minute) },
		"private entry":   func() (string, error) { return m.Mint("e", 1, []string{"10.0.0.1"}, time.Minute) },
		"bad entry":       func() (string, error) { return m.Mint("e", 1, []string{"https://a.example/"}, time.Minute) },
		"no execution":    func() (string, error) { return m.Mint("", 1, []string{"a.example"}, time.Minute) },
		"no project":      func() (string, error) { return m.Mint("e", 0, []string{"a.example"}, time.Minute) },
	}
	for name, mint := range cases {
		if tok, err := mint(); err == nil || tok != "" {
			t.Errorf("%s: want a refusal, got %q %v", name, tok, err)
		}
	}
	m.nonce = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := m.Mint("e", 1, []string{"a.example"}, time.Minute); err == nil {
		t.Error("a nonce failure must fail the mint")
	}
}

func TestLoadRefusals(t *testing.T) {
	if _, err := Load(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no file: %v", err)
	}
	dir := t.TempDir()
	if _, err := Load(Config{SigningKeyFile: filepath.Join(dir, "missing.pem")}); err == nil {
		t.Fatal("a missing file must fail")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubPEM, _ := runtoken.MarshalPublicKeyPEM(pub)
	path := filepath.Join(dir, "public.pem")
	_ = os.WriteFile(path, pubPEM, 0o600)
	_, err := Load(Config{SigningKeyFile: path})
	if err == nil {
		t.Fatal("a public key is not a signing key")
	}
	if strings.Contains(err.Error(), "BEGIN") {
		t.Fatal("the error must not echo the file content")
	}
}
