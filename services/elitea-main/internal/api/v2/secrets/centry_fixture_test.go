package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

// centryVaultFixture is testdata/centry/vault-key-format.json: a project vault
// written by the real Python implementation (cryptography.fernet, driven the way
// centry's secret engine drives it — see the generator script beside the file).
//
// The tests in key_format_test.go check this handler against centrysecrets, but
// both are Go code in this repository agreeing with each other. The fixture is
// the ground truth neither of them authors: if the two Go implementations ever
// drift together away from what Python writes, only these tests notice. The
// gateway's internal/account carries the same loader — it is a separate module
// (outside go.work), so the code cannot be shared, only the fixture.
type centryVaultFixture struct {
	VaultPlaintext         string `json:"vault_plaintext"`
	SecretsKeyRowUnwrapped string `json:"secrets_key_row_unwrapped"`
	SecretsKeyRowWrapped   string `json:"secrets_key_row_wrapped"`
	SecretsDataRow         string `json:"secrets_data_row"`
	MasterKeyEnvValue      string `json:"master_key_env_value"`
	ProjectKeyRawB64Std    string `json:"project_key_raw_bytes_b64std"`
}

// loadCentryVaultFixture walks up from the package directory to the repository
// root to find the shared fixture. Walking beats a counted "../../../.." chain:
// the other reader sits at a different depth, and a miscounted path would fail
// identically to a missing file.
func loadCentryVaultFixture(t *testing.T) centryVaultFixture {
	t.Helper()

	const rel = "testdata/centry/vault-key-format.json"
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err == nil {
			var fixture centryVaultFixture
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			return fixture
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found in any parent of the package directory", rel)
		}
		dir = parent
	}
}

// projectKeyRaw is the fixture's 32 key bytes. The b64std field is only a
// transport encoding for JSON — unlike the key row, which is base64 because
// that is genuinely what centry stores.
func (f centryVaultFixture) projectKeyRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(f.ProjectKeyRawB64Std)
	if err != nil {
		t.Fatalf("decode project_key_raw_bytes_b64std: %v", err)
	}
	return raw
}

// Direction one: this handler reads a vault Python wrote.
func TestHandlerReadsPythonWrittenVault(t *testing.T) {
	t.Parallel()

	fixture := loadCentryVaultFixture(t)
	handler := &Handler{}

	key, err := handler.decryptKey([]byte(fixture.SecretsKeyRowUnwrapped))
	if err != nil || !bytes.Equal(key, fixture.projectKeyRaw(t)) {
		t.Fatalf("decryptKey on a Python-written key row = %x, %v; want the fixture's 32 key bytes", key, err)
	}

	plaintext, err := fernetDecrypt(key, []byte(fixture.SecretsDataRow))
	if err != nil {
		t.Fatalf("fernetDecrypt on a Python-written data row: %v", err)
	}
	if string(plaintext) != fixture.VaultPlaintext {
		t.Fatalf("decrypted vault = %q, want %q", plaintext, fixture.VaultPlaintext)
	}
}

// Direction two: what this handler writes is byte-for-byte what Python wrote.
// Unlike the Fernet tokens (random IV, current timestamp), the key row is a
// plain base64 encoding, so an exact comparison is meaningful here — this is
// the assertion that would have failed against the raw-32-byte bug outright.
func TestHandlerWritesTheKeyRowPythonWrites(t *testing.T) {
	t.Parallel()

	fixture := loadCentryVaultFixture(t)
	handler := &Handler{}

	stored, err := handler.encryptKey(fixture.projectKeyRaw(t))
	if err != nil {
		t.Fatalf("encryptKey: %v", err)
	}
	if string(stored) != fixture.SecretsKeyRowUnwrapped {
		t.Fatalf("encryptKey wrote %q; Python wrote %q", stored, fixture.SecretsKeyRowUnwrapped)
	}
}

// The master-key layer: centry stores Fernet(master).encrypt(key), wrapping the
// base64 form. Unwrapping therefore yields 44 bytes, not 32.
func TestHandlerReadsPythonWrittenWrappedKeyRow(t *testing.T) {
	t.Parallel()

	fixture := loadCentryVaultFixture(t)
	masterKey, err := fernetDecodeKey(fixture.MasterKeyEnvValue)
	if err != nil {
		t.Fatalf("decode master key env value: %v", err)
	}
	handler := &Handler{masterKey: masterKey}

	key, err := handler.decryptKey([]byte(fixture.SecretsKeyRowWrapped))
	if err != nil || !bytes.Equal(key, fixture.projectKeyRaw(t)) {
		t.Fatalf("decryptKey on a wrapped Python-written key row = %x, %v; want the fixture's 32 key bytes", key, err)
	}
}

// centrysecrets is the reader behind the chat-config route and the
// Configurations vault paths. It must open the Python-written rows directly,
// both wrapped and not.
func TestCentrysecretsOpensPythonWrittenVault(t *testing.T) {
	t.Parallel()

	fixture := loadCentryVaultFixture(t)

	unwrapped, err := centrysecrets.OpenUnwrapped(
		[]byte(fixture.SecretsKeyRowUnwrapped), []byte(fixture.SecretsDataRow))
	if err != nil {
		t.Fatalf("OpenUnwrapped on a Python-written vault: %v", err)
	}
	secret, err := unwrapped.LookupPythonInteger("chat_max_file_upload_size_mb")
	if err != nil || secret.Value != "25" {
		t.Fatalf("LookupPythonInteger = %q, %v; want \"25\"", secret.Value, err)
	}
	hidden, err := unwrapped.Lookup("HIDDEN_TOKEN")
	if err != nil || !hidden.Hidden || hidden.Value != "hidden-fixture-not-a-real-credential" {
		t.Fatalf("Lookup(HIDDEN_TOKEN) = %+v, %v; want the fixture's hidden secret", hidden, err)
	}

	wrapped, err := centrysecrets.OpenWrapped(
		[]byte(fixture.MasterKeyEnvValue),
		[]byte(fixture.SecretsKeyRowWrapped),
		[]byte(fixture.SecretsDataRow))
	if err != nil {
		t.Fatalf("OpenWrapped on a Python-written vault: %v", err)
	}
	if secret, err := wrapped.Lookup("OPENAI_API_KEY"); err != nil || secret.Value != "sk-fixture-not-a-real-credential" {
		t.Fatalf("Lookup(OPENAI_API_KEY) = %q, %v; want the fixture's secret", secret.Value, err)
	}
}
