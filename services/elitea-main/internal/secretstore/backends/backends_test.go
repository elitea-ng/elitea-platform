package backends

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/vault"
)

type nopStore struct{ secretstore.SecretStore }

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestUnconfiguredVaultBuildsOnlyPGVault(t *testing.T) {
	stores, prerequisites, err := NewFromEnv(lookup(nil), nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 1 || stores[secretstore.BackendPGVault] == nil {
		t.Fatalf("stores = %v", stores)
	}
	if _, ok := stores[secretstore.BackendVault]; ok {
		t.Fatal("an unconfigured Vault must not be in the map")
	}
	if len(prerequisites) != 1 {
		t.Fatalf("prerequisites = %d", len(prerequisites))
	}
	required := profile.PolicyRevision{Document: profile.PolicyDocument{VaultRequiredCredentialClasses: []string{"database"}}}
	if err := prerequisites[0].Check(context.Background(), required); !errors.Is(err, vault.ErrRequiredButAbsent) {
		t.Fatalf("required and unconfigured = %v", err)
	}
	if err := prerequisites[0].Check(context.Background(), profile.PolicyRevision{}); err != nil {
		t.Fatalf("not required and unconfigured = %v", err)
	}
}

func TestInvalidVaultConfigRefusesBoot(t *testing.T) {
	_, _, err := NewFromEnv(lookup(map[string]string{vault.EnvAddr: "http://vault:8200"}), nopStore{})
	if !errors.Is(err, vault.ErrInvalidConfig) {
		t.Fatalf("http:// = %v", err)
	}
}

func TestCompleteVaultConfigBuildsTheAdapter(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	// Any valid certificate works; the adapter does not dial at construction.
	if err := os.WriteFile(ca, testCertificatePEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	stores, prerequisites, err := NewFromEnv(lookup(map[string]string{
		vault.EnvAddr: "https://vault.invalid:8200", vault.EnvCAFile: ca, vault.EnvAuthMethod: "kubernetes",
		vault.EnvAuthMount: "kubernetes", vault.EnvRolePrefix: "elitea", vault.EnvKVMount: "secret",
		vault.EnvTokenFile: token, vault.EnvReadinessScope: "readiness",
	}), nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stores[secretstore.BackendVault].(*vault.Store); !ok || len(prerequisites) != 1 {
		t.Fatalf("stores = %v", stores)
	}
}

func TestPGVaultIsRequired(t *testing.T) {
	if _, _, err := NewFromEnv(lookup(nil), nil); err == nil {
		t.Fatal("a nil pgvault adapter must refuse")
	}
}

func testCertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestVaultFromEnv(t *testing.T) {
	store, prerequisite, err := VaultFromEnv(lookup(nil))
	if err != nil || store != nil || prerequisite == nil {
		t.Fatalf("unconfigured = %v, %v, %v", store, prerequisite, err)
	}
	if _, _, err := VaultFromEnv(lookup(map[string]string{vault.EnvAddr: "http://vault:8200"})); !errors.Is(err, vault.ErrInvalidConfig) {
		t.Fatalf("http:// = %v", err)
	}
	if _, _, err := VaultFromEnv(nil); err == nil {
		t.Fatal("a nil lookup must refuse")
	}
}
