package admin

// The console confirms every dangerous and critical action in one dialog and
// sends the reason the server records (ADR-0059 decision 6, "Dangerous
// actions"; R3.8b, issue 2614). Which actions are marked is the action table's
// (internal/adminaccess/action_rows.go), not a second list in the web app:
// this test writes the marked entries, their danger level, the closed list of
// reason codes and the header bound into a checked-in JSON beside the admin
// pages. The web app reads it for its types and its source guard
// (apps/elitea-web/src/pages/admin/__tests__/dangerousAction.guard.test.ts),
// so a new marked entry fails there until the console wires it or says why it
// has no control.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// adminDangerousActionsFile is the JSON, relative to this package.
const adminDangerousActionsFile = "../../../../../../apps/elitea-web/src/pages/admin/adminDangerousActions.json"

type adminDangerousActions struct {
	GeneratedBy string `json:"generated_by"`
	// ReasonCodes is adminaccess.ActionReasonCodes, in the console's order.
	ReasonCodes []string `json:"reason_codes"`
	// MaxReasonHeaderBytes bounds a reason header after percent-decoding.
	MaxReasonHeaderBytes int `json:"max_reason_header_bytes"`
	// Actions maps the key of each entry that asks for a reason ("METHOD
	// pattern" or "METHOD pattern action") to its danger level.
	Actions map[string]adminaccess.Danger `json:"actions"`
	// HeadersOnly lists the "METHOD pattern" routes whose body keys are data:
	// the reason travels in the headers even though the request has a body.
	HeadersOnly []string `json:"headers_only"`
}

func TestAdminDangerousActionsFileMatchesTheActionTable(t *testing.T) {
	t.Parallel()
	marked := map[string]adminaccess.Danger{}
	var headersOnly []string
	for _, entry := range adminaccess.ActionTable().Entries() {
		if !entry.Reason {
			continue
		}
		marked[entry.Key()] = entry.Danger
		route := entry.Method + " " + entry.Pattern
		if adminaccess.ReasonBodyOf(entry.Method, entry.Pattern) == adminaccess.ReasonBodyHeadersOnly &&
			!slices.Contains(headersOnly, route) {
			headersOnly = append(headersOnly, route)
		}
	}
	slices.Sort(headersOnly)
	want := adminDangerousActions{
		GeneratedBy:          "UPDATE_GOLDEN=1 go test ./internal/api/v2/admin/ -run TestAdminDangerousActionsFileMatchesTheActionTable",
		ReasonCodes:          adminaccess.ActionReasonCodes(),
		MaxReasonHeaderBytes: adminaccess.MaxReasonHeaderBytes,
		Actions:              marked,
		HeadersOnly:          headersOnly,
	}
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.FromSlash(adminDangerousActionsFile)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", path, err)
	}
	if !bytes.Equal(got, encoded) {
		t.Errorf("%s drifted from the action table; regenerate with UPDATE_GOLDEN=1 and wire the new "+
			"marked actions through the console's dangerous-action dialog", path)
	}
}
