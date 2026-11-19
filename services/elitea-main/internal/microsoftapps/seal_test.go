package microsoftapps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const plaintextSecret = "s3cr3t~value.from.Entra"

func testSealer(t *testing.T) *sealer {
	t.Helper()
	s, err := newSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealRoundTripAndTamper(t *testing.T) {
	s := testSealer(t)
	aad := secretAAD{AppID: "app-1", ClientID: testClientID, KeyVersion: 1}
	sealed := s.seal(plaintextSecret, aad)
	if bytes.Contains(sealed, []byte(plaintextSecret)) {
		t.Fatal("the ciphertext holds the plaintext")
	}
	opened, err := s.open(sealed, aad)
	if err != nil || opened.Reveal() != plaintextSecret {
		t.Fatalf("round trip = %v, %v", opened, err)
	}
	for name, moved := range map[string]secretAAD{
		"another app":    {AppID: "app-2", ClientID: testClientID, KeyVersion: 1},
		"another client": {AppID: "app-1", ClientID: "7f8d0c1e-0000-4000-8000-000000000002", KeyVersion: 1},
		"another key":    {AppID: "app-1", ClientID: testClientID, KeyVersion: 2},
	} {
		if _, err := s.open(sealed, moved); !errors.Is(err, ErrCustodyIntegrity) {
			t.Errorf("a ciphertext moved to %s opened: %v", name, err)
		}
	}
	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 1
	if _, err := s.open(flipped, aad); !errors.Is(err, ErrCustodyIntegrity) {
		t.Errorf("a modified ciphertext opened: %v", err)
	}
	other, _ := newSealer(bytes.Repeat([]byte{0x43}, 32))
	if _, err := other.open(sealed, aad); !errors.Is(err, ErrCustodyIntegrity) {
		t.Errorf("another master key opened it: %v", err)
	}
	if _, err := newSealer([]byte("short")); err == nil {
		t.Error("a short master key was accepted")
	}
	// Sealing twice must not repeat: a random nonce per seal.
	if bytes.Equal(sealed, s.seal(plaintextSecret, aad)) {
		t.Error("two seals of one secret are identical")
	}
}

// The Secret type is what keeps the plaintext out of every response, log and
// export, so each way a struct reaches a writer is asserted.
func TestSecretRedactsInEveryFormat(t *testing.T) {
	type holder struct {
		ClientID string `json:"client_id"`
		Secret   Secret `json:"secret"`
	}
	value := holder{ClientID: testClientID, Secret: NewSecret(plaintextSecret)}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("resolved", "secret", value.Secret, "holder", value)
	outputs := map[string]string{
		"json":    string(encoded),
		"%v":      fmt.Sprintf("%v", value),
		"%+v":     fmt.Sprintf("%+v", value),
		"%#v":     fmt.Sprintf("%#v", value),
		"%s":      fmt.Sprintf("%s", value.Secret),
		"%q":      fmt.Sprintf("%q", value.Secret),
		"error":   fmt.Errorf("exchange failed: %v", value).Error(),
		"slog":    logged.String(),
		"Println": fmt.Sprintln(value),
	}
	for name, out := range outputs {
		if strings.Contains(out, plaintextSecret) {
			t.Errorf("%s output carries the secret: %s", name, out)
		}
	}
	if value.Secret.Reveal() != plaintextSecret || value.Secret.Empty() {
		t.Error("Reveal lost the value")
	}
}
