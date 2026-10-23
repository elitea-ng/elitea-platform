package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

// The on-disk project key must be the representation centry writes and the
// current generation reads: `cryptography.fernet.Fernet.generate_key()`'s
// 44-byte URL-safe base64 ENCODING of the 32 key bytes
// (legacy/…/secret_engines/database.py `_write_key`).
//
// This handler used to persist the raw 32 bytes instead, which
// `centrysecrets.OpenUnwrapped` — the reader behind the current chat-config
// route (#194) and the Configurations vault paths — rejects outright, and
// which this handler could not have written over a centry-created vault
// either (`fernetEncrypt` would have sliced a 28-byte AES key out of the 44).
// The two vault implementations could not share a database; found while making
// `GET /elitea_core/chat_config/prompt_lib/{projectID}` reachable.
func TestProjectKeyIsStoredInTheFormatCentrysecretsReads(t *testing.T) {
	t.Parallel()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{}

	stored, err := handler.encryptKey(raw)
	if err != nil {
		t.Fatalf("encryptKey: %v", err)
	}
	if len(stored) != 44 {
		t.Fatalf("stored key is %d bytes; centrysecrets.decodeFernetKey requires the 44-byte base64 form", len(stored))
	}

	// The decisive assertion: a vault this handler encrypts under that stored
	// key must open through centrysecrets, which is what the chat-config
	// reader uses.
	token, err := fernetEncrypt(raw, []byte(`{"secrets":{"chat_max_file_upload_size_mb":"1"},"hidden_secrets":{}}`))
	if err != nil {
		t.Fatalf("fernetEncrypt: %v", err)
	}
	vault, err := centrysecrets.OpenUnwrapped(stored, token)
	if err != nil {
		t.Fatalf("centrysecrets could not open a vault this handler wrote: %v", err)
	}
	secret, err := vault.LookupPythonInteger("chat_max_file_upload_size_mb")
	if err != nil || secret.Value != "1" {
		t.Fatalf("lookup = %q, %v; want \"1\"", secret.Value, err)
	}
}

// A database written by an earlier build of this handler holds the raw 32
// bytes. Those rows must keep opening, or upgrading would lock every existing
// project out of its own secrets.
func TestDecryptKeyAcceptsBothStoredRepresentations(t *testing.T) {
	t.Parallel()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{}

	for name, stored := range map[string][]byte{
		"centry base64 form":  []byte(base64.URLEncoding.EncodeToString(raw)),
		"legacy raw 32 bytes": raw,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := handler.decryptKey(stored)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("decryptKey(%s) = %x, %v; want the 32 key bytes", name, got, err)
			}
		})
	}

	if _, err := handler.decryptKey([]byte("not-a-key")); err == nil {
		t.Fatal("a key that is neither representation was accepted")
	}
}

// With a master key set, the stored row is the master-Fernet-wrapped base64
// key — centry's `Fernet(master).encrypt(generate_key())`. The round trip has
// to survive the extra layer, and centrysecrets.OpenWrapped has to agree.
func TestMasterKeyWrappedProjectKeyRoundTripsThroughCentrysecrets(t *testing.T) {
	t.Parallel()

	master := make([]byte, 32)
	raw := make([]byte, 32)
	for _, buffer := range [][]byte{master, raw} {
		if _, err := rand.Read(buffer); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{masterKey: master}

	stored, err := handler.encryptKey(raw)
	if err != nil {
		t.Fatalf("encryptKey: %v", err)
	}
	got, err := handler.decryptKey(stored)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("decryptKey = %x, %v; want the 32 key bytes", got, err)
	}

	token, err := fernetEncrypt(raw, []byte(`{"secrets":{},"hidden_secrets":{}}`))
	if err != nil {
		t.Fatalf("fernetEncrypt: %v", err)
	}
	encodedMaster := []byte(base64.URLEncoding.EncodeToString(master))
	if _, err := centrysecrets.OpenWrapped(encodedMaster, stored, token); err != nil {
		t.Fatalf("centrysecrets could not open a wrapped vault this handler wrote: %v", err)
	}
}
