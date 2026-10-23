package secrets

// SECRETS_MASTER_KEY: the ABSENT case and the MALFORMED case must not be the
// same case (#412).
//
// The old NewHandler answered both by keeping no master key, so a typo in one
// variable turned wrapped vault storage into plaintext storage. The service
// started, provisioning succeeded, and every later read succeeded too — which
// is why nobody would notice.
//
// These cases pin the RULE. The stored bytes are asserted separately, against a
// real PostgreSQL, in master_key_postgres_integration_test.go.

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// staticEnv is the injected getenv. MasterKeyFromEnv takes one so these cases
// need no t.Setenv, which would forbid t.Parallel.
func staticEnv(value string) func(string) string {
	return func(name string) string {
		if name == MasterKeyEnvVar {
			return value
		}
		return ""
	}
}

// An absent key is a supported shape: no compose file and no chart under
// deploy/ except the staging one supplies a key, and the E2E stack seeds
// unwrapped key rows on purpose.
func TestMasterKeyFromEnvAcceptsAnAbsentKey(t *testing.T) {
	t.Parallel()

	key, err := MasterKeyFromEnv(staticEnv(""))
	if err != nil {
		t.Fatalf("an absent %s must not be an error, got %v", MasterKeyEnvVar, err)
	}
	if key != nil {
		t.Fatalf("an absent %s must yield no key, got %x", MasterKeyEnvVar, key)
	}
}

func TestMasterKeyFromEnvAcceptsAValidKey(t *testing.T) {
	t.Parallel()

	raw := bytes.Repeat([]byte{7}, 32)
	key, err := MasterKeyFromEnv(staticEnv(base64.URLEncoding.EncodeToString(raw)))
	if err != nil {
		t.Fatalf("a valid %s: %v", MasterKeyEnvVar, err)
	}
	if !bytes.Equal(key, raw) {
		t.Fatalf("decoded key = %x, want %x", key, raw)
	}
}

// The ways an operator actually breaks this variable. The last two are the
// mounted-secret shape: a value that picked up a space or a tab.
func TestMasterKeyFromEnvRejectsAMalformedKeyAndNamesTheVariable(t *testing.T) {
	t.Parallel()

	valid := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

	for name, value := range map[string]string{
		"not base64":     "!!! this is not base64 !!!",
		"wrong length":   base64.URLEncoding.EncodeToString([]byte("too short")),
		"leading space":  " " + valid,
		"trailing space": valid + " ",
		"trailing tab":   valid + "\t",
		"padding only":   "=",
		"truncated":      valid[:20],
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			key, err := MasterKeyFromEnv(staticEnv(value))
			if err == nil {
				t.Fatalf("a %s %s was accepted; it must be an error, because the "+
					"handler would otherwise store every project vault key unwrapped",
					name, MasterKeyEnvVar)
			}
			if key != nil {
				t.Fatalf("a rejected key must yield no key bytes, got %x", key)
			}
			// The operator has to be able to find the variable from the
			// message alone. Naming it is an acceptance criterion of #412.
			if !strings.Contains(err.Error(), MasterKeyEnvVar) {
				t.Fatalf("the error must name %s, got %q", MasterKeyEnvVar, err)
			}
		})
	}
}

// The shape this change must NOT start rejecting.
//
// A key read from a mounted file usually ends with a newline. Go's base64
// decoder ignores "\r" and "\n", so that key decodes to the same 32 bytes and
// the deployment works. Python's base64.urlsafe_b64decode ignores them too, so
// the legacy secrets engine reads the same rows. Turning a newline into a
// start-up failure would stop a stack that is running correctly today, so this
// case pins the tolerance deliberately rather than leaving it to chance.
func TestMasterKeyFromEnvAcceptsAKeyWithATrailingNewline(t *testing.T) {
	t.Parallel()

	raw := bytes.Repeat([]byte{7}, 32)
	valid := base64.URLEncoding.EncodeToString(raw)

	for name, value := range map[string]string{
		"newline":         valid + "\n",
		"carriage return": valid + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			key, err := MasterKeyFromEnv(staticEnv(value))
			if err != nil {
				t.Fatalf("a %s must stay acceptable, got %v", name, err)
			}
			if !bytes.Equal(key, raw) {
				t.Fatalf("a %s changed the key: %x, want %x", name, key, raw)
			}
		})
	}
}

// Defence in depth for the constructor that cannot report an error.
// cmd/elitea-main stops before it builds any handler, but a handler built
// WITHOUT that gate — a second entrypoint, a future test harness — must still
// refuse to write rather than fall back to unwrapped storage.
func TestAHandlerWithAMalformedMasterKeyRefusesToWrapOrUnwrap(t *testing.T) {
	t.Parallel()

	_, keyErr := MasterKeyFromEnv(staticEnv("!!! not base64 !!!"))
	if keyErr == nil {
		t.Fatal("premise failed: the malformed key was accepted, so this case proves nothing")
	}
	handler := &Handler{masterKeyErr: keyErr}

	stored, err := handler.encryptKey(bytes.Repeat([]byte{9}, 32))
	if err == nil {
		t.Fatalf("encryptKey returned %q with a malformed master key; it must fail, "+
			"because returning the unwrapped encoding is exactly what wrote plaintext vaults", stored)
	}
	if !strings.Contains(err.Error(), MasterKeyEnvVar) {
		t.Fatalf("the encryptKey error must name %s, got %q", MasterKeyEnvVar, err)
	}

	if _, err := handler.decryptKey([]byte("anything")); err == nil {
		t.Fatal("decryptKey succeeded with a malformed master key; it must fail")
	}
}

// The zero-value Handler is what the key-format cases construct, and what the
// absent-key path produces. It must keep writing the unwrapped encoding, or
// this change would break every deployment that supplies no key.
func TestAHandlerWithNoMasterKeyStillWritesTheUnwrappedEncoding(t *testing.T) {
	t.Parallel()

	raw := bytes.Repeat([]byte{3}, 32)
	stored, err := (&Handler{}).encryptKey(raw)
	if err != nil {
		t.Fatalf("encryptKey with no master key: %v", err)
	}
	if string(stored) != base64.URLEncoding.EncodeToString(raw) {
		t.Fatalf("encryptKey with no master key wrote %q, want the base64 encoding of the key", stored)
	}
}
