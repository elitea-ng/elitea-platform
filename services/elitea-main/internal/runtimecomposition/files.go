package runtimecomposition

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	maxSigningKeyFileBytes = 64 * 1024
	encodedFernetKeyBytes  = 44
	// fernetMasterKeyBytes is what encodedFernetKeyBytes decodes to.
	fernetMasterKeyBytes = 32
)

func loadEd25519PrivateKey(path string) (ed25519.PrivateKey, error) {
	contents, err := securefile.Read(path, maxSigningKeyFileBytes, securefile.PrivateMaterial)
	if err != nil {
		return nil, fmt.Errorf("load command-signing key: %w", err)
	}
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("command-signing key must be one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse command-signing key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("command-signing key is not Ed25519")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), nil
}

func loadOptionalFernetMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	contents, err := securefile.Read(path, encodedFernetKeyBytes, securefile.PrivateMaterial)
	if err != nil {
		return nil, fmt.Errorf("load current secret-vault master key: %w", err)
	}
	var decoded [33]byte
	n, decodeErr := base64.URLEncoding.Decode(decoded[:], contents)
	clear(decoded[:])
	if decodeErr != nil || n != fernetMasterKeyBytes || len(contents) != encodedFernetKeyBytes {
		clear(contents)
		return nil, errors.New("current secret-vault master key is not a Fernet key")
	}
	return contents, nil
}
