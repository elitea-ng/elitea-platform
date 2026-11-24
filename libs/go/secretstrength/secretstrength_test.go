package secretstrength

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func randomSecret(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestCheckRefusesWeakValues(t *testing.T) {
	for name, value := range map[string]string{
		"empty":               "",
		"whitespace":          "   \t ",
		"ten bytes":           "0123456789",
		"31 bytes":            strings.Repeat("x", 31),
		"changeme":            "changeme",
		"changeme upper":      "  CHANGEME ",
		"padded placeholder":  "changeme-changeme-changeme-changeme",
		"secret":              "secret",
		"password":            "password",
		"elitea":              "elitea",
		"committed e2e value": "changeme-e2e",
	} {
		err := Check("SOME_SIGNING_SECRET", value)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, ErrWeak) {
			t.Errorf("%s: error is not ErrWeak: %v", name, err)
		}
		if !strings.Contains(err.Error(), "SOME_SIGNING_SECRET") {
			t.Errorf("%s: error does not name the variable: %v", name, err)
		}
		// Short dictionary words ("secret") occur in the message itself, so the
		// no-echo rule is checked on the values long enough to be distinctive.
		if len(strings.TrimSpace(value)) >= 10 && strings.Contains(err.Error(), strings.TrimSpace(value)) {
			t.Errorf("%s: error repeats the value: %v", name, err)
		}
	}
}

func TestCheckAcceptsRandom32Bytes(t *testing.T) {
	if err := Check("SOME_SIGNING_SECRET", randomSecret(t, 32)); err != nil {
		t.Fatalf("a random 32-byte secret was refused: %v", err)
	}
	// 32 raw printable bytes pass too: the rule is a length in bytes.
	if err := Check("SOME_SIGNING_SECRET", randomSecret(t, 24)); err != nil {
		t.Fatalf("a 32-character random secret was refused: %v", err)
	}
}

func TestCheckIfSet(t *testing.T) {
	if err := CheckIfSet("OPTIONAL", ""); err != nil {
		t.Fatalf("an unset optional secret was refused: %v", err)
	}
	if err := CheckIfSet("OPTIONAL", "  "); err == nil {
		t.Fatal("a blank optional secret was accepted")
	}
	if err := CheckIfSet("OPTIONAL", "short"); err == nil {
		t.Fatal("a short optional secret was accepted")
	}
	if err := CheckIfSet("OPTIONAL", randomSecret(t, 32)); err != nil {
		t.Fatalf("a strong optional secret was refused: %v", err)
	}
}

// TestBurnedValuesAreRefusedByDigest proves the digest list is consulted, for
// a long value that passes every other rule. The test does not hold a real
// burned value; it adds the digest of a fresh one for the duration of the
// test.
func TestBurnedValuesAreRefusedByDigest(t *testing.T) {
	value := randomSecret(t, 32)
	if Burned(value) {
		t.Fatal("a fresh random value reads as burned")
	}
	sum := digest(value)
	burnedDigests[sum] = struct{}{}
	t.Cleanup(func() { delete(burnedDigests, sum) })
	if !Burned(value) || !Burned(" "+value+"\n") {
		t.Fatal("a value on the burned list is not reported")
	}
	err := Check("SECRETS_MASTER_KEY", value)
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("a burned value: err = %v (must refuse without the value)", err)
	}
}

func TestBurnedDigestsAreWellFormed(t *testing.T) {
	if len(burnedDigests) == 0 {
		t.Fatal("the burned list is empty")
	}
	for d := range burnedDigests {
		if len(d) != 64 || strings.ToLower(d) != d {
			t.Errorf("digest %q is not 64 lower-case hex characters", d)
		}
	}
}
