package secrets

// What a database holding project key rows written WITHOUT a master key does
// once a master key is configured.
//
// Behaviour kept: such a row is NOT silently read as if it were wrapped, and it
// is not silently accepted either. decryptKey refuses it with an error, and the
// operator converts the rows once with deploy/scripts/rewrap-centry-vault.py
// (dry run by default; it changes only the key-row wrapping, never the vault
// data). After that conversion the same project key opens. The cases below pin
// both halves, so the migration path stays real rather than a statement in a
// document.

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestUnwrappedKeyRowsNeedARewrapOnceAMasterKeyIsSet(t *testing.T) {
	t.Parallel()

	master := bytes.Repeat([]byte{0x11}, 32)
	projectKey := bytes.Repeat([]byte{0x22}, 32)
	unwrappedEncoded := []byte(base64.URLEncoding.EncodeToString(projectKey)) // what a keyless deployment stores
	handler := &Handler{masterKey: master}

	for name, row := range map[string][]byte{
		"44-byte base64 key (current unwrapped form)": unwrappedEncoded,
		"raw 32-byte key (earlier builds)":            projectKey,
	} {
		got, err := handler.decryptKey(row)
		if err == nil {
			t.Fatalf("%s: decryptKey with a master key opened an unwrapped row (%d bytes); "+
				"it must refuse until the row is rewrapped", name, len(got))
		}
		if strings.Contains(err.Error(), string(unwrappedEncoded)) || strings.Contains(err.Error(), string(master)) {
			t.Fatalf("%s: the error carries key material: %v", name, err)
		}
	}

	// The rewrap script's output for that row: Fernet(master) over the
	// 44-byte encoding. The same project key must now open.
	rewrapped, err := fernetEncrypt(master, unwrappedEncoded)
	if err != nil {
		t.Fatalf("wrap the row as the rewrap script does: %v", err)
	}
	got, err := handler.decryptKey(rewrapped)
	if err != nil {
		t.Fatalf("a rewrapped row did not open: %v", err)
	}
	if !bytes.Equal(got, projectKey) {
		t.Fatal("a rewrapped row opened to a different project key")
	}
}
