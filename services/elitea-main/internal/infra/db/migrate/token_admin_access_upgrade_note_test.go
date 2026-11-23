package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The upgrade note of shared 0265 (issue 2505, ADR-0059 R1.1) exists, names
// the migration and says that a new token is recorded as `none` and, once
// enforced, has no administration rights until scopes are chosen. scripts/ci/check-upgrading-notes.mjs checks the note's
// shape; this test checks that this note says what the story promised.
func TestTokenAdminAccessUpgradeNoteStatesTheNewDefault(t *testing.T) {
	t.Parallel()
	notes, err := filepath.Glob("../../../../../../docs/upgrading/*-2505-token-admin-access-columns.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("found %d upgrade notes for #2505 under docs/upgrading, want 1: %v", len(notes), notes)
	}
	body, err := os.ReadFile(notes[0]) //nolint:gosec // fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, want := range []string{
		"A token created after the upgrade is recorded as `none`. Once a later ADR-0059 release enforces the value, such a token has no administration rights until scopes are chosen.",
		"`inherit`",
		"0265_token_admin_access.sql",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q", notes[0], want)
		}
	}
}
