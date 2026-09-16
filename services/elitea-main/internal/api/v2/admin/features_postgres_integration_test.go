package admin_test

// Unit A14 acceptance for the admin FEATURES page's write surface (issue #200).
//
// This is the page's own end. The consumer end — that these rows change what the
// platform does — is
// `internal/api/v2/eliteacore/platform_flags_postgres_integration_test.go`, and
// the two are deliberately not allowed to share a helper: a single round-trip
// helper spanning both would pass on a page that writes rows nothing reads,
// which is the exact failure this unit exists to remove.
//
// Every write below is checked through the product's own GET *and* by SQL
// against `centry.platform_config`, for the reason the sibling file's header
// gives: this endpoint's broken predecessor answered 200 without decoding its
// body, so a status code proves nothing here.
//
// The harness (`configDo`, `newConfigPool`, `storedValueSQL`, `configRouter`)
// is the one `config_values_postgres_integration_test.go` already builds.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func saveSection(t *testing.T, router chi.Router, section string, values map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return configDo(t, router, http.MethodPut,
		"/admin/plugin_config_values/administration/"+section,
		map[string]any{"values": values})
}

func readSection(t *testing.T, router chi.Router, section string) configValuesBody {
	t.Helper()
	recorder := configDo(t, router, http.MethodGet,
		"/admin/plugin_config_values/administration/"+section, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (body %s)", section, recorder.Code, recorder.Body.String())
	}
	return decodeConfigBody(t, recorder)
}

/* ── MCP Configuration ─────────────────────────────────────────────────── */

func TestMCPConfigurationSectionIsWritableAndPersists(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	// Fresh: the schema's defaults, both true.
	body := readSection(t, router, "mcp_configuration")
	if body.Values["mcp_enabled"] != true || body.Values["mcp_in_menu"] != true {
		t.Fatalf("defaults = %v, want both true", body.Values)
	}

	recorder := saveSection(t, router, "mcp_configuration", map[string]any{"mcp_enabled": false})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	// Through the product's own read…
	if got := readSection(t, router, "mcp_configuration").Values["mcp_enabled"]; got != false {
		t.Errorf("re-read mcp_enabled = %v, want false", got)
	}
	// …and as a row.
	raw, ok := storedValueSQL(t, pool, "mcp_configuration", "mcp_enabled")
	if !ok || raw != "false" {
		t.Errorf("stored row = %q (present=%v), want false", raw, ok)
	}
	// The key it was NOT given keeps its default rather than being blanked.
	if got := readSection(t, router, "mcp_configuration").Values["mcp_in_menu"]; got != true {
		t.Errorf("mcp_in_menu = %v after saving only mcp_enabled, want its default true", got)
	}
}

// TestMCPConfigurationRefusesANonBoolean — the guardrail against a row the
// consumer would silently read as its fallback.
func TestMCPConfigurationRefusesANonBoolean(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "mcp_configuration", map[string]any{"mcp_enabled": "false"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT string \"false\" = %d, want 400", recorder.Code)
	}
	if got := decodeConfigBody(t, recorder).Error; !strings.Contains(got, "mcp_enabled") {
		t.Errorf("error %q does not name the field", got)
	}
	if _, ok := storedValueSQL(t, pool, "mcp_configuration", "mcp_enabled"); ok {
		t.Error("a refused value was written anyway")
	}
}

/* ── Agent Publishing ──────────────────────────────────────────────────── */

func TestAgentPublishingSectionIsWritableAndPersists(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "agent_publishing", map[string]any{
		"is_publish_blocked":            true,
		"publish_whitelist_project_ids": []any{4, 9},
		"agent_categories":              []any{"Security Review"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	values := readSection(t, router, "agent_publishing").Values
	if values["is_publish_blocked"] != true {
		t.Errorf("is_publish_blocked = %v", values["is_publish_blocked"])
	}
	raw, ok := storedValueSQL(t, pool, "agent_publishing", "publish_whitelist_project_ids")
	if !ok || raw != "[4, 9]" {
		t.Errorf("stored whitelist = %q (present=%v)", raw, ok)
	}
	raw, ok = storedValueSQL(t, pool, "agent_publishing", "agent_categories")
	if !ok || raw != `["Security Review"]` {
		t.Errorf("stored categories = %q (present=%v)", raw, ok)
	}
}

// TestSkillPublishingSectionIsWritableAndPersists — the section that used to be
// withheld.
//
// It is asserted separately from the agent one, rather than folded into a table
// with it, because the pair's INDEPENDENCE is the behaviour: they are different
// section rows with different field keys, and a test that drove both through one
// helper would still pass if the skill save silently landed in the agent
// section's rows.
func TestSkillPublishingSectionIsWritableAndPersists(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "skill_publishing", map[string]any{
		"is_skill_publish_blocked":            true,
		"skill_publish_whitelist_project_ids": []any{7},
		"skill_categories":                    []any{"Security"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	values := readSection(t, router, "skill_publishing").Values
	if values["is_skill_publish_blocked"] != true {
		t.Errorf("is_skill_publish_blocked = %v", values["is_skill_publish_blocked"])
	}
	raw, ok := storedValueSQL(t, pool, "skill_publishing", "skill_publish_whitelist_project_ids")
	if !ok || raw != "[7]" {
		t.Errorf("stored whitelist = %q (present=%v)", raw, ok)
	}
	raw, ok = storedValueSQL(t, pool, "skill_publishing", "skill_categories")
	if !ok || raw != `["Security"]` {
		t.Errorf("stored categories = %q (present=%v)", raw, ok)
	}

	// The agent section must not have moved: a skill save that landed there
	// would freeze agent publishing on a deployment that only asked to freeze
	// skills.
	if _, ok := storedValueSQL(t, pool, "agent_publishing", "is_publish_blocked"); ok {
		t.Error("saving the skill section wrote into agent_publishing")
	}
}

// TestSkillPublishValidationRulesIsRefusedByName — the field-level reason, the
// skill-side twin of the agent one below.
func TestSkillPublishValidationRulesIsRefusedByName(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "skill_publishing", map[string]any{
		"skill_publish_validation_rules": "reject anything mentioning production",
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	reason := decodeConfigBody(t, recorder).Error
	if !strings.Contains(reason, "skill_publish_validation_rules") {
		t.Errorf("error %q does not name the field", reason)
	}
	if !strings.Contains(reason, "deterministic") {
		t.Errorf("error %q does not carry the server's reason", reason)
	}
	if _, ok := storedValueSQL(t, pool, "skill_publishing", "skill_publish_validation_rules"); ok {
		t.Error("the refused field was written anyway")
	}
}

// TestArrayElementTypesAreEnforced.
//
// Both consumers type-assert their elements and SKIP what does not match
// (`AgentCategories` takes `e.(string)`, the guardrail takes `float64`), so an
// element of the wrong type is accepted, persisted, echoed by the GET, rendered
// in the form — and ignored. That is "saves into a void" one level down, and the
// operator has every reason to believe the category exists.
func TestArrayElementTypesAreEnforced(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	cases := []struct {
		name  string
		key   string
		value any
	}{
		{"a category that is not a string", "agent_categories", []any{map[string]any{"name": "x"}}},
		{"a project id that is not a number", "publish_whitelist_project_ids", []any{"4"}},
		{"a project id that is not integral", "publish_whitelist_project_ids", []any{4.5}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := saveSection(t, router, "agent_publishing", map[string]any{testCase.key: testCase.value})
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if got := decodeConfigBody(t, recorder).Error; !strings.Contains(got, testCase.key) {
				t.Errorf("error %q does not name the field", got)
			}
			if _, ok := storedValueSQL(t, pool, "agent_publishing", testCase.key); ok {
				t.Error("a refused value was written anyway")
			}
		})
	}
}

// TestAnIntegerWhitelistEntryIsAccepted — the refusal above must not have made
// the field unusable. A validator that rejects everything passes every "is it
// refused" test and none of these.
func TestAnIntegerWhitelistEntryIsAccepted(t *testing.T) {
	_, router := newConfigEnvironment(t)
	// 4.0 is what a JSON integer decodes to; it must be accepted, not caught by
	// the integrality check.
	recorder := saveSection(t, router, "agent_publishing", map[string]any{
		"publish_whitelist_project_ids": []any{4.0, 0},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	// And an empty array — the "block everyone" state — is a legitimate value.
	if got := saveSection(t, router, "agent_publishing", map[string]any{
		"publish_whitelist_project_ids": []any{},
	}).Code; got != http.StatusOK {
		t.Errorf("empty whitelist = %d, want 200", got)
	}
}

/* ── Voice Features ────────────────────────────────────────────────────── */

func TestVoiceFeaturesSectionIsWritableAndPersists(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	values := readSection(t, router, "voice_features").Values
	if values["vite_voice_features_enabled"] != true ||
		values["vite_voice_features_temporarily_disabled"] != false {
		t.Fatalf("defaults = %v", values)
	}

	if got := saveSection(t, router, "voice_features", map[string]any{
		"vite_voice_features_temporarily_disabled": true,
	}).Code; got != http.StatusOK {
		t.Fatalf("PUT = %d", got)
	}

	if got := readSection(t, router, "voice_features").Values["vite_voice_features_temporarily_disabled"]; got != true {
		t.Errorf("re-read = %v, want true", got)
	}
	raw, ok := storedValueSQL(t, pool, "voice_features", "vite_voice_features_temporarily_disabled")
	if !ok || raw != "true" {
		t.Errorf("stored row = %q (present=%v)", raw, ok)
	}
}

/* ── field-level unavailability ────────────────────────────────────────── */

// TestAnUnavailableFieldInAnAvailableSectionIsRefusedByName.
//
// `agent_publishing` is live; `publish_validation_rules` inside it is not,
// because publish validation here is deterministic and has no evaluator for a
// custom prompt to reach. Withholding the whole section would have taken away
// three working controls to disclose one broken one; accepting the field would
// have stored a prompt nothing runs.
func TestAnUnavailableFieldInAnAvailableSectionIsRefusedByName(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "agent_publishing", map[string]any{
		"publish_validation_rules": "reject anything mentioning production",
	})
	// 400, not 501: the SECTION is implemented, so "not implemented" would be
	// the wrong thing to say about the request. What is wrong is the field.
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	reason := decodeConfigBody(t, recorder).Error
	if !strings.Contains(reason, "publish_validation_rules") {
		t.Errorf("error %q does not name the field", reason)
	}
	if !strings.Contains(reason, "deterministic") {
		t.Errorf("error %q does not carry the server's reason", reason)
	}
	if _, ok := storedValueSQL(t, pool, "agent_publishing", "publish_validation_rules"); ok {
		t.Error("the refused field was written anyway")
	}
	// The sibling fields in the same section are unaffected.
	if got := saveSection(t, router, "agent_publishing",
		map[string]any{"is_publish_blocked": true}).Code; got != http.StatusOK {
		t.Errorf("a working field in the same section = %d, want 200", got)
	}
}

// TestTheUnavailableFieldIsStillDECLARED — the page renders it read-only with
// the reason. Dropping it from the schema would make the control vanish
// relative to the reference, which reads as a page that lost a feature.
func TestTheUnavailableFieldIsStillDECLARED(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
	var schema struct {
		Sections []struct {
			ID     string `json:"id"`
			Fields []struct {
				Key               string `json:"key"`
				UnavailableReason string `json:"unavailable_reason"`
			} `json:"fields"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, section := range schema.Sections {
		if section.ID != "agent_publishing" {
			continue
		}
		for _, field := range section.Fields {
			if field.Key == "publish_validation_rules" {
				if field.UnavailableReason == "" {
					t.Error("publish_validation_rules is declared with no reason; the page would render it editable")
				}
				return
			}
		}
		t.Fatal("publish_validation_rules is not declared at all")
	}
	t.Fatal("agent_publishing section missing")
}

/* ── the sections that stay unavailable ────────────────────────────────── */

/*
 * TestFeaturesSectionsWithNoConsumerRefuseBothVerbs USED TO STAND HERE, and is
 * DELETED rather than skipped, because it has no instances left.
 *
 * It held two: `skill_publishing` ("no publish endpoint, no catalog, no
 * categories") and `support_assistant` ("the widget has no render site"). #585
 * built the skill publishing pipeline and #588 built the support assistant, so
 * every section on the Features page now has a consumer and the server withholds
 * none of them.
 *
 * The intermediate version of this test read the withheld set from the schema
 * and `t.Skip`ped when it was empty. `scripts/go/declared-skips.txt` rejected
 * that, correctly and by design: its rule 3 says "delete the test if nobody
 * intends to run it — a test that always skips is not coverage, it is the
 * appearance of coverage", and its ledger is reserved for tests CI genuinely
 * cannot supply an environment for. "Nothing to assert" is not that.
 *
 * NOTHING IS LOST. The refusal contract — 501 on both verbs, with the server's
 * own reason in the body — is exercised by three live instances on the
 * Configuration side, in config_values_postgres_integration_test.go's
 * `mcp_servers`, `llm_proxy` and `auth`. (`observability`, `runtime` and
 * `admin_panel` used to bring the count to six; they were REMOVED rather than
 * ported — see config_schemas.go's "Observability, Runtime and Admin Panel are
 * GONE too" note — so they no longer exist to refuse anything.) And
 * TestSchemaDeclaresAvailabilityForEverySection still holds every section to
 * being either available WITH FIELDS or unavailable WITH A REASON, so a
 * Features section that loses its consumer cannot quietly become a blank pane.
 * When one is withheld again, this test comes back with it.
 */

/* ── the move(s) ──────────────────────────────────────────────────────────── */

// TestResourcesStillServesTheHelpCenterAcrossPageMoves.
//
// This is the assertion that makes EITHER move safe. #217 built `resources` on
// the Configuration page and wired `/help-center` to it; a later unit moved the
// section to Features citing the reference's own client routing; A2 (ELITEA-0032)
// moved it back to Configuration on the onetest suite's evidence instead (see
// `config_schemas.go`'s `resourcesSection` for the full history). None of those
// moves may touch the Help Center's own read, because it calls a SEPARATE route
// (`prompt_lib`) that has no notion of which admin page authored the row.
// Proved by writing through the admin PUT and reading back through the public
// route, rather than by inspecting that the code was not edited.
func TestResourcesStillServesTheHelpCenterAcrossPageMoves(t *testing.T) {
	_, router := newConfigEnvironment(t)

	recorder := saveSection(t, router, "resources", map[string]any{
		"resources_documentation_links": []any{
			map[string]any{"title": "Handbook", "url": "https://docs.example.com/handbook"},
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("save = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	public := configDo(t, router, http.MethodGet, "/admin/plugin_config_values/prompt_lib/resources", nil)
	if public.Code != http.StatusOK {
		t.Fatalf("public read = %d (body %s)", public.Code, public.Body.String())
	}
	links, ok := decodeConfigBody(t, public).Values["resources_documentation_links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("Help Center read lost the links: %v", decodeConfigBody(t, public).Values["resources_documentation_links"])
	}
	entry, _ := links[0].(map[string]any)
	if entry["url"] != "https://docs.example.com/handbook" {
		t.Errorf("link url = %v", entry["url"])
	}
}
