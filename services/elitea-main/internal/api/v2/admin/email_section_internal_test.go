package admin

// The `email` SECTION's contract, and the guard that keeps the SMTP password
// out of a plaintext row (gap G7).
//
// This is an INTERNAL test because `rejectCredentialField` and the section
// table are unexported, and both halves belong together: the section is
// withheld from the generic value endpoints precisely because those endpoints
// refuse a credential, and a change that relaxed either half alone would move
// the SMTP password into `centry.platform_config`, readable by every holder of
// `runtime.plugins`.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmailSectionShape(t *testing.T) {
	var found map[string]any
	for _, section := range configSections() {
		if section["id"] == "email" {
			found = section
		}
	}
	if found == nil {
		t.Fatal("no email section: the operator has no entry point to the relay settings")
	}
	if found["managed_surface"] != "email" {
		t.Errorf("managed_surface = %v; the SPA registry keys on this exact string", found["managed_surface"])
	}
	// The reason STAYS. It is still true of the plugin-config value endpoints,
	// and a client that cannot render the managed surface would otherwise get
	// a blank pane.
	reason, _ := found["unavailable_reason"].(string)
	if reason == "" {
		t.Fatal("no unavailable_reason")
	}
	if !strings.Contains(reason, "vault") {
		t.Errorf("the reason does not say where the password goes instead: %q", reason)
	}
	// No fields. One of the eight values is a credential, and a schema that
	// declared the other seven would render a form that saves a host and a
	// user name, reports success, and sends nothing.
	fields, _ := found["fields"].([]map[string]any)
	if len(fields) != 0 {
		t.Errorf("section declares %d field(s); the value endpoints cannot serve any of them", len(fields))
	}
	// The permission is one this platform already issues. A new string here
	// would need a new grant, and an ungranted gate is a page that disappears
	// silently — see adminNavItems.ts's header.
	if found["required_permission"] != "runtime.plugins" {
		t.Errorf("required_permission = %v, want the permission the Configuration page already carried",
			found["required_permission"])
	}
	if _, err := json.Marshal(found); err != nil {
		t.Errorf("section is not JSON-encodable: %v", err)
	}
}

// The generic value endpoints must still refuse this section. `email` is
// reached through resolveWritableSection, which answers 501 for a section that
// declares an unavailable_reason — so the section's own declaration is the
// gate, and this test pins that it is present and load-bearing.
func TestEmailSectionIsUnavailableOnTheGenericEndpoints(t *testing.T) {
	section, ok := findConfigSection("email")
	if !ok {
		t.Fatal("findConfigSection could not resolve the email section")
	}
	if reason, _ := section.raw["unavailable_reason"].(string); reason == "" {
		t.Fatal("the email section would be WRITABLE through the plugin-config value endpoints, " +
			"which store values as plaintext rows")
	}
}

// rejectCredentialField is the backstop under all of it: if a future change
// ever declared the password on a section, the write path must still refuse
// it. The check is on the FIELD SPEC rather than on a section list exactly so
// that a section which becomes writable later cannot quietly acquire a
// plaintext-secret column.
func TestRejectCredentialFieldStillRefusesAnSMTPPassword(t *testing.T) {
	refusal := rejectCredentialField("smtp_password", map[string]any{
		"key": "smtp_password", "type": "string", "format": "password",
	})
	if refusal == "" {
		t.Fatal("a password-format field was accepted into a plaintext platform-configuration row")
	}
	if !strings.Contains(refusal, "smtp_password") {
		t.Errorf("the refusal does not name the field: %q", refusal)
	}
	// A non-credential field on the same shape of section is untouched.
	if got := rejectCredentialField("smtp_host", map[string]any{"key": "smtp_host", "type": "string"}); got != "" {
		t.Errorf("a plain field was refused: %q", got)
	}
}
