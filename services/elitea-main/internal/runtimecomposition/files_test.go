package runtimecomposition

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOptionalFernetMasterKeyUsesBoundedPrivateFile(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if value, err := loadOptionalFernetMasterKey(""); err != nil || value != nil {
		t.Fatalf("absent optional key = %x, %v", value, err)
	}

	encoded := []byte(base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	path := filepath.Join(root, "vault-master-key")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOptionalFernetMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, encoded) {
		t.Fatalf("loaded Fernet key changed: %x", loaded)
	}
	clear(loaded)

	if err := os.WriteFile(path, []byte("not-a-fernet-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptionalFernetMasterKey(path); err == nil {
		t.Fatal("malformed Fernet key was accepted")
	}
	if err := os.WriteFile(path, encoded, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptionalFernetMasterKey(path); err == nil {
		t.Fatal("group-readable Fernet key was accepted")
	}
}

func TestLoadEd25519PrivateKeyAcceptsOnlyBoundedPKCS8File(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "signing-key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEd25519PrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Equal(privateKey) {
		t.Fatal("loaded signing key changed")
	}

	oversize := filepath.Join(root, "oversize.pem")
	if err := os.WriteFile(oversize, make([]byte, maxSigningKeyFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEd25519PrivateKey(oversize); err == nil {
		t.Fatal("oversize signing key file was accepted")
	}
}
