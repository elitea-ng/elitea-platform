package runtimecomposition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	ed25519KeyringSchemaVersion = "elitea.runtime-ed25519-keyring.v1"
	maxEd25519KeyringBytes      = 64 * 1024
	maxEd25519VerificationKeys  = 64
)

type verificationKeyringDocument struct {
	SchemaVersion string                     `json:"schema_version"`
	Keys          []verificationKeyringEntry `json:"keys"`
}

type verificationKeyringEntry struct {
	KeyID           string `json:"key_id"`
	PublicKeyBase64 string `json:"public_key_base64"`
}

type ed25519VerificationKeyring struct {
	keys map[string]ed25519.PublicKey
}

func loadEd25519VerificationKeyring(path string) (*ed25519VerificationKeyring, error) {
	raw, err := securefile.Read(path, maxEd25519KeyringBytes, securefile.PublicMaterial)
	if err != nil {
		return nil, fmt.Errorf("load runtime Ed25519 verification keyring: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document verificationKeyringDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("runtime Ed25519 verification keyring is invalid")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, errors.New("runtime Ed25519 verification keyring is invalid")
	}
	if document.SchemaVersion != ed25519KeyringSchemaVersion || len(document.Keys) == 0 || len(document.Keys) > maxEd25519VerificationKeys {
		return nil, errors.New("runtime Ed25519 verification keyring is invalid")
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, entry := range document.Keys {
		if entry.KeyID == "" || len(entry.KeyID) > 256 || strings.ContainsAny(entry.KeyID, "\r\n\x00") || hasUnicodeWhitespace(entry.PublicKeyBase64) {
			return nil, errors.New("runtime Ed25519 verification keyring is invalid")
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return nil, errors.New("runtime Ed25519 verification keyring is invalid")
		}
		publicKey, err := base64.StdEncoding.Strict().DecodeString(entry.PublicKeyBase64)
		if err != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, errors.New("runtime Ed25519 verification keyring is invalid")
		}
		keys[entry.KeyID] = append(ed25519.PublicKey(nil), publicKey...)
	}
	return &ed25519VerificationKeyring{keys: keys}, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("unexpected trailing JSON value")
}

func hasUnicodeWhitespace(value string) bool {
	return strings.IndexFunc(value, unicode.IsSpace) >= 0
}

func (r *ed25519VerificationKeyring) requireActiveSigningKey(keyID string, privateKey ed25519.PrivateKey) error {
	if r == nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("runtime active signing key is invalid")
	}
	publicKey, ok := r.keys[keyID]
	if !ok || !bytes.Equal(publicKey, privateKey.Public().(ed25519.PublicKey)) {
		return errors.New("runtime active signing key does not match its verification keyring entry")
	}
	return nil
}

func (r *ed25519VerificationKeyring) ResolveEd25519PublicKey(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("runtime command verification key is not available")
	}
	publicKey, ok := r.keys[keyID]
	if !ok {
		return nil, errors.New("runtime command verification key is not available")
	}
	return append(ed25519.PublicKey(nil), publicKey...), nil
}
