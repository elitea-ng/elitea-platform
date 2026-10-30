package executioninterrupt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// display.argument_digest is optional (a card from an earlier worker has
// none) and, when present, is "sha256:" and 64 lower-case hex digits.
func TestParseCardAcceptsOnlyAWellFormedArgumentDigest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "fanout-interrupt-card-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseCard(raw); err != nil {
		t.Fatalf("a card without a digest is refused: %v", err)
	}
	withDigest := func(digest string) []byte {
		var card map[string]any
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatal(err)
		}
		card["display"].(map[string]any)["argument_digest"] = digest
		encoded, err := json.Marshal(card)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if _, _, err := ParseCard(withDigest("sha256:" + strings.Repeat("a", 64))); err != nil {
		t.Fatalf("a card with a digest is refused: %v", err)
	}
	for _, bad := range []string{"sha256:" + strings.Repeat("A", 64), "sha256:abc", "md5:" + strings.Repeat("a", 64)} {
		if _, _, err := ParseCard(withDigest(bad)); !errors.Is(err, ErrInvalidCard) {
			t.Fatalf("digest %q accepted: %v", bad, err)
		}
	}
}
