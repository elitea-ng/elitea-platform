package audit

import (
	"os"
	"strings"
	"testing"
)

// The role-use columns of shared 0268 are in the row hash of canonical form v2
// (shared 0276, ADR-0059 R1.4b) and outside the hash of a v1 row, so
// docs/ARCHITECTURE.md must say which form covers them. The test reads the
// section by its anchor; a section that is moved or loses the statement
// fails it.
func TestArchitectureSaysWhichFormCoversRoleUse(t *testing.T) {
	const anchor = `<a id="admin-audit-role-use-tamper-evidence"></a>`
	body, err := os.ReadFile("../../../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	at := strings.Index(text, anchor)
	if at < 0 {
		t.Fatalf("docs/ARCHITECTURE.md has no %s anchor", anchor)
	}
	section := text[at:]
	if next := strings.Index(section[len(anchor):], "\n## "); next >= 0 {
		section = section[:len(anchor)+next]
	}
	for _, want := range []string{
		"centry.audit_event_canonical",
		"audit_events_chain_forms", "centry.audit_event_canonical_v1", "_v2",
		"never hashed", "elevation_flagged",
		"acting_roles", "elevation_id", "credential_kind", "central_override",
		"reason_code", "reason_missing",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the role-use section of docs/ARCHITECTURE.md does not say %q", want)
		}
	}
}
