// Package secretstrength decides, at start-up, whether a signing secret is
// strong enough to use.
//
// A signing secret (an HMAC key, a token-signing root) is only as strong as
// it is unknown. A short value can be guessed, and a value that appears in a
// public repository is known to everyone. Every service that signs or
// verifies with one runs Check before it derives any key from it, so a
// derived key never exists from a weak root.
//
// No error carries the value. Each names the variable and the rule it broke.
package secretstrength

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MinBytes is the shortest signing secret Check accepts: 32 bytes, the size
// of an HMAC-SHA256 key. `openssl rand -base64 32` produces a 44-byte value.
const MinBytes = 32

// ErrWeak is the error class of every refusal, for errors.Is.
var ErrWeak = errors.New("weak signing secret")

// placeholders are well-known example values, compared after trimming and
// lower-casing. Most are shorter than MinBytes and the length rule refuses
// them too; the list keeps the refusal message specific, and catches the
// same words padded to length.
var placeholders = []string{
	"changeme", "change-me", "change_me", "replace-me", "replaceme",
	"secret", "password", "elitea", "placeholder", "example",
}

// placeholderMarkers refuse a value that CONTAINS one of these words, the
// usual shape of an example value padded to the minimum length.
var placeholderMarkers = []string{"changeme", "change-me", "change_me", "replace-me", "replaceme", "placeholder"}

// burnedDigests are the SHA-256 digests of values that were once committed
// under deploy/ in this repository, for any signing variable or the vault
// master key. They are public, so no deployment may use them. Only the
// digests are kept here, so this file does not publish the values again.
// Add the digest of any value that is committed by mistake.
var burnedDigests = map[string]struct{}{
	"985fd0d79911300ade63bbcefeb0680eaa75dae04d73139a64fc6b9437a79988": {}, // a vault master key, deploy/docker-compose.staging.yml
	"317454db4d4e898e5e733f3a0a524622fc37f801b96f584526d19ebd9450d237": {},
	"385c622fec13d09f1cb76bdab1efdab2d9cf14bf5600042d149a1b13ea96a1bd": {},
	"7c1ac23da031fd3ee313f80e3fe48936f487f39c85f6555ce60b7d0395cad710": {},
	"6516defc8f6b841cdf0e8035416df1e0a0d8009bd76c4a78e1ee9aa772a1728c": {},
	"0a0ae5942eb01198d8e33da8b8e1443d23c1cae2dd15ae61ad7d43a37854a02d": {},
	"cedf94ffa5b247c8c8ab51b05408e4e71bf885654ef506c95b71f633359d4b49": {},
	"30234c5d2d59846ca3b659e7144a0927ea2037fe51c0a68b5302d6370aa1f655": {},
	"2037a77394d6b1443626c481f7439f8e660b529f968e0129f7cc6a8451ec0701": {},
	"3ac696c515f9d0b709218abb6c9f369807f47094f5824a552e04bb7bc1837f91": {},
	"561ff0ece314a9686e04885abdb6cc84125e84562ae6b6be6062b5246163a045": {},
	"303f3b9691368c44e1185f88d0cff23df77db8152416fc7de195eb18ccf200e8": {},
	"4daab57012cbe2d10ff2f1fecb78ab4c1fecf245285b0f2725ee857a2234bd2d": {},
	"953796aa49fa7f48bc24b9b38707a114eb159c6d3766befde0a3c389833891ef": {},
}

// Burned reports whether value is one of the values published in this
// repository (see burnedDigests). It compares digests only.
func Burned(value string) bool {
	if _, burned := burnedDigests[digest(value)]; burned {
		return true
	}
	_, burned := burnedDigests[digest(strings.TrimSpace(value))]
	return burned
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Check refuses a signing secret that is empty, blank, shorter than MinBytes,
// a well-known placeholder, or a value published in this repository. name is
// the environment variable, and the only part of the input an error repeats.
func Check(name, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%w: %s is empty; set it to a random value of at least %d bytes (for example `openssl rand -base64 32`)",
			ErrWeak, name, MinBytes)
	}
	if Burned(value) {
		return fmt.Errorf("%w: %s holds a value that was published in the source repository and must be treated as known; "+
			"generate a new random value (for example `openssl rand -base64 32`) and rotate it", ErrWeak, name)
	}
	lower := strings.ToLower(trimmed)
	for _, p := range placeholders {
		if lower == p {
			return fmt.Errorf("%w: %s holds a placeholder value; set it to a random value of at least %d bytes (for example `openssl rand -base64 32`)",
				ErrWeak, name, MinBytes)
		}
	}
	for _, m := range placeholderMarkers {
		if strings.Contains(lower, m) {
			return fmt.Errorf("%w: %s holds a placeholder value; set it to a random value of at least %d bytes (for example `openssl rand -base64 32`)",
				ErrWeak, name, MinBytes)
		}
	}
	if len(trimmed) < MinBytes {
		return fmt.Errorf("%w: %s is %d bytes long and must be at least %d; set it to a random value (for example `openssl rand -base64 32`)",
			ErrWeak, name, len(trimmed), MinBytes)
	}
	if distinctBytes(trimmed) < MinDistinctBytes {
		return fmt.Errorf("%w: %s repeats too few characters to be random (fewer than %d different ones); set it to a random value (for example `openssl rand -base64 32`)",
			ErrWeak, name, MinDistinctBytes)
	}
	return nil
}

// MinDistinctBytes is a cheap floor on randomness: a value such as 32 copies
// of one character, or the all-"A" Fernet key of the examples, is long
// enough and still known. Random base64 or hex of 32 bytes always has far
// more different characters than this.
const MinDistinctBytes = 8

func distinctBytes(value string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(value); i++ {
		if !seen[value[i]] {
			seen[value[i]] = true
			n++
		}
	}
	return n
}

// CheckIfSet is Check for an optional secret: an unset or empty value passes,
// and any other value must pass Check. A blank (whitespace-only) value is
// refused: it is set, and it is not a secret.
func CheckIfSet(name, value string) error {
	if value == "" {
		return nil
	}
	return Check(name, value)
}
