package secrets

// The generated `X-SECRET` value (#408).

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewSecretsHeaderValueIsNotTheGuessableDefault(t *testing.T) {
	t.Parallel()

	value, err := NewSecretsHeaderValue()
	if err != nil {
		t.Fatalf("NewSecretsHeaderValue: %v", err)
	}
	// "secret" is what pylon's check_secret_header falls back to, and what the
	// version-details route still accepts from a project that has no value.
	if value == "secret" {
		t.Fatal("the generated value is the literal pylon default")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("the value is not base64url text: %v", err)
	}
	if len(raw) != secretsHeaderValueBytes {
		t.Fatalf("the value carries %d bytes of entropy, want %d", len(raw), secretsHeaderValueBytes)
	}
	// A header value crosses the SDK, the edge and pylon unchanged. Anything
	// that has to be quoted or folded would arrive as a different string.
	if strings.ContainsAny(value, " \t\r\n\"\\,;=+/") {
		t.Fatalf("the value %q holds a character an HTTP header does not carry verbatim", value)
	}
}

func TestNewSecretsHeaderValueIsRandom(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 64)
	for range 64 {
		value, err := NewSecretsHeaderValue()
		if err != nil {
			t.Fatalf("NewSecretsHeaderValue: %v", err)
		}
		if _, repeat := seen[value]; repeat {
			t.Fatalf("the generator repeated %q; one leaked value would open more than one project", value)
		}
		seen[value] = struct{}{}
	}
}

// TestSecretsHeaderValueNameMatchesPylon pins the name. The value is useless
// under any other one: pylon reads exactly this key, and so does the
// version-details route that compares the header against it.
func TestSecretsHeaderValueNameMatchesPylon(t *testing.T) {
	t.Parallel()

	if SecretsHeaderValueName != "secrets_header_value" {
		t.Fatalf("SecretsHeaderValueName = %q, want the name pylon reads", SecretsHeaderValueName)
	}
}
