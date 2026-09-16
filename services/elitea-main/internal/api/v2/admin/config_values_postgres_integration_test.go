package admin_test

// Unit A14 acceptance for the admin CONFIGURATION surface (issue #200).
//
// Every write below is asserted by WRITING and then RE-READING through the
// product's own GET, and separately by reading the row out of
// `centry.platform_config` with SQL — so a handler that synthesised the expected
// answer could not pass both. A status code proves nothing on this endpoint in
// particular: the PUT it replaces already answered 200, with the request body
// never read.
//
// The shapes being guarded against, all of which were present at once:
//
//   - A SAVE INTO A VOID. `PluginConfigValuesSave` returned
//     `{"values":{},"requires_restart":[]}` and did not decode the body.
//     TestSavedValuesSurviveAReReadAndAreInTheTable.
//   - A READ THAT ANSWERS A DIFFERENT QUESTION. `PluginConfigValues` returned
//     the schema DEFAULTS for every section at once, ignoring the `{plugin}`
//     segment — so a configured platform and a fresh install were indistinguishable.
//     TestReadIsPerSectionAndOverlaysStoredValuesOnDefaults.
//   - 200-WITH-EMPTY FOR A SURFACE THAT DOES NOT EXIST. Every Pylon-runtime
//     endpoint answered success. TestUnavailableSectionsRefuseWithAReason.
//   - THE HELP CENTER'S OWN READ ANSWERING SOMETHING ELSE ENTIRELY.
//     `/plugin_config_values/prompt_lib/resources` returned chat and upload
//     limits under no `values` wrapper (issue #26).
//     TestPromptLibResourcesServesWhatTheAdminSaved.
//
// Plus the two refusals this unit adds deliberately: a link URL that would
// execute in every user's browser, and a credential that belongs in the vault.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type configValuesBody struct {
	Values     map[string]any `json:"values"`
	FieldsMeta map[string]struct {
		Path            string `json:"path"`
		RequiresRestart bool   `json:"requires_restart"`
	} `json:"fields_meta"`
	Saved   bool             `json:"saved"`
	Restart []map[string]any `json:"requires_restart"`
	Error   string           `json:"error"`
}

// configRouter mounts the three routes exactly as internal/api/router.go does.
//
// It mounts the `administration` pair as STATIC segments, which is the point:
// a static segment binds no `{mode}` URL parameter, so a handler that inferred
// its mode from `chi.URLParam(r, "mode")` would see `""` on precisely the
// requests that are administration requests. That trap cost #207 a round; these
// handlers state their mode as a Go value and this mounting is what would catch
// a regression.
func configRouter(handler *admin.Handler, principal *auth.User) chi.Router {
	router := chi.NewRouter()
	if principal != nil {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), *principal)))
			})
		})
	}
	router.Get("/admin/plugin_config_schemas/{mode}", handler.PluginConfigSchemas)
	router.Get("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValues)
	router.Put("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValuesSave)
	router.Get("/admin/plugin_config_values/prompt_lib/resources", handler.PromptLibResourcesValues)
	return router
}

func configDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeConfigBody(t *testing.T, recorder *httptest.ResponseRecorder) configValuesBody {
	t.Helper()
	var body configValuesBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// readResources re-reads through the SAME GET handler the admin page calls.
func readResources(t *testing.T, router chi.Router) configValuesBody {
	t.Helper()
	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_values/administration/resources", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET resources status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	return decodeConfigBody(t, recorder)
}

// storedValueSQL bypasses the handlers entirely. The GET re-read proves the
// product agrees with itself; this proves a ROW exists with those bytes.
func storedValueSQL(t *testing.T, pool *pgxpool.Pool, section, key string) (string, bool) {
	t.Helper()
	var raw string
	err := pool.QueryRow(context.Background(),
		`SELECT value::text FROM centry.platform_config WHERE section = $1 AND key = $2`,
		section, key).Scan(&raw)
	if err != nil {
		return "", false
	}
	return raw, true
}

func saveResources(t *testing.T, router chi.Router, values map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return configDo(t, router, http.MethodPut,
		"/admin/plugin_config_values/administration/resources",
		map[string]any{"values": values})
}

func newConfigEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newConfigPool(t)
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	return pool, configRouter(admin.NewHandler(pool), &principal)
}

/* ── the read ──────────────────────────────────────────────────────────── */

// TestReadIsPerSectionAndOverlaysStoredValuesOnDefaults is the guard for the
// read that answered a different question: the handler this replaces flattened
// EVERY section's defaults into one map and never looked at the `{plugin}`
// segment, so the response was identical for `resources`, `guardrails` and a
// section that did not exist.
func TestReadIsPerSectionAndOverlaysStoredValuesOnDefaults(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	body := readResources(t, router)

	// Declared keys are all present on a fresh install, at their defaults —
	// otherwise the form renders empty and the operator cannot tell "unset" from
	// "the read failed".
	if got, ok := body.Values["resources_documentation_title"]; !ok || got != "Documentation" {
		t.Fatalf("resources_documentation_title = %v, want the schema default", got)
	}
	// A key belonging to ANOTHER section must not appear. This is the assertion
	// the old flatten-everything read fails.
	if _, present := body.Values["auth_provider"]; present {
		t.Error("the resources section returned auth_provider; the read is not per-section")
	}
	if _, present := body.Values["banner_message"]; present {
		t.Error("the resources section returned banner_message; the read is not per-section")
	}

	// Now store one value and re-read: the STORED value must win over the
	// default, and every other key must keep its default.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO centry.platform_config (section, key, value) VALUES ($1, $2, $3::jsonb)`,
		"resources", "resources_documentation_title", `"Handbook"`); err != nil {
		t.Fatalf("seed stored value: %v", err)
	}
	body = readResources(t, router)
	if got := body.Values["resources_documentation_title"]; got != "Handbook" {
		t.Errorf("stored value did not win: got %v, want %q", got, "Handbook")
	}
	if got := body.Values["resources_tutorials_title"]; got != "Tutorials" {
		t.Errorf("unstored key lost its default: got %v", got)
	}
}

// TestReadDropsStoredKeysTheSchemaNoLongerDeclares pins the overlay DIRECTION.
// Merging the other way round would surface an orphaned row the form has no
// field for and no way to remove.
func TestReadDropsStoredKeysTheSchemaNoLongerDeclares(t *testing.T) {
	pool, router := newConfigEnvironment(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO centry.platform_config (section, key, value) VALUES ($1, $2, $3::jsonb)`,
		"resources", "resources_retired_card_title", `"Ghost"`); err != nil {
		t.Fatalf("seed orphan value: %v", err)
	}
	body := readResources(t, router)
	if _, present := body.Values["resources_retired_card_title"]; present {
		t.Error("a stored key the schema no longer declares was returned to the form")
	}
}

/* ── the write ─────────────────────────────────────────────────────────── */

// TestSavedValuesSurviveAReReadAndAreInTheTable is the assertion the no-op PUT
// cannot pass. It checks BOTH the product's own read and the row itself.
func TestSavedValuesSurviveAReReadAndAreInTheTable(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := saveResources(t, router, map[string]any{
		"resources_documentation_title":   "Platform Handbook",
		"resources_documentation_enabled": true,
		"resources_documentation_links": []any{
			map[string]any{"title": "Getting started", "url": "https://docs.example.com/start"},
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	saved := decodeConfigBody(t, recorder)
	if !saved.Saved {
		t.Error("PUT did not report saved")
	}
	// The response is a RE-READ, not an echo: a write that failed to persist
	// must not be able to answer with the values it was handed.
	if saved.Values["resources_documentation_title"] != "Platform Handbook" {
		t.Errorf("PUT response value = %v", saved.Values["resources_documentation_title"])
	}

	body := readResources(t, router)
	if got := body.Values["resources_documentation_title"]; got != "Platform Handbook" {
		t.Errorf("re-read title = %v, want %q", got, "Platform Handbook")
	}
	links, ok := body.Values["resources_documentation_links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("re-read links = %v, want one entry", body.Values["resources_documentation_links"])
	}
	link, _ := links[0].(map[string]any)
	if link["url"] != "https://docs.example.com/start" {
		t.Errorf("re-read link url = %v", link["url"])
	}

	raw, present := storedValueSQL(t, pool, "resources", "resources_documentation_title")
	if !present {
		t.Fatal("no centry.platform_config row for resources_documentation_title")
	}
	if raw != `"Platform Handbook"` {
		t.Errorf("stored JSONB = %s", raw)
	}

	// updated_by records WHO changed platform configuration. Without it a
	// configuration change is the one admin action with no attribution.
	var author string
	if err := pool.QueryRow(context.Background(),
		`SELECT updated_by FROM centry.platform_config WHERE section = 'resources' AND key = 'resources_documentation_title'`).
		Scan(&author); err != nil {
		t.Fatalf("read updated_by: %v", err)
	}
	if author != "operator@example.com" {
		t.Errorf("updated_by = %q, want the acting principal", author)
	}
}

// TestSaveOverwritesRatherThanDuplicating — the primary key is (section, key),
// and a second save of the same field must update the row, not fail or grow the
// table. A table that accumulated one row per save would make the read
// non-deterministic.
func TestSaveOverwritesRatherThanDuplicating(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	for _, title := range []string{"First", "Second", "Third"} {
		if recorder := saveResources(t, router, map[string]any{
			"resources_tutorials_title": title,
		}); recorder.Code != http.StatusOK {
			t.Fatalf("PUT %q status = %d (%s)", title, recorder.Code, recorder.Body.String())
		}
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM centry.platform_config WHERE section = 'resources' AND key = 'resources_tutorials_title'`).
		Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("row count = %d, want 1", rows)
	}
	if got := readResources(t, router).Values["resources_tutorials_title"]; got != "Third" {
		t.Errorf("value after three saves = %v, want the last one", got)
	}
}

// TestSaveLeavesUnmentionedKeysAlone — the page sends the whole section, but a
// partial body must not blank the rest. Toggling one card off must not erase
// another card's links.
func TestSaveLeavesUnmentionedKeysAlone(t *testing.T) {
	_, router := newConfigEnvironment(t)

	if recorder := saveResources(t, router, map[string]any{
		"resources_tutorials_title": "Walkthroughs",
	}); recorder.Code != http.StatusOK {
		t.Fatalf("first PUT: %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder := saveResources(t, router, map[string]any{
		"resources_video_library_enabled": false,
	}); recorder.Code != http.StatusOK {
		t.Fatalf("second PUT: %d (%s)", recorder.Code, recorder.Body.String())
	}

	body := readResources(t, router)
	if got := body.Values["resources_tutorials_title"]; got != "Walkthroughs" {
		t.Errorf("first save was lost: %v", got)
	}
	if got := body.Values["resources_video_library_enabled"]; got != false {
		t.Errorf("second save did not land: %v", got)
	}
}

// TestSaveRequiresRestartIsEmptyAndHonest — pylon returns the pylons whose
// plugins must reload. There are none here, and `plugin_config_restart` answers
// 501, so a non-empty list would put a reload button on screen with nothing
// behind it.
func TestSaveRequiresRestartIsEmptyAndHonest(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := saveResources(t, router, map[string]any{"resources_tutorials_title": "Guides"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := decodeConfigBody(t, recorder).Restart; len(got) != 0 {
		t.Errorf("requires_restart = %v, want empty", got)
	}
}

/* ── refusals ──────────────────────────────────────────────────────────── */

// TestSaveRefusesAScriptURLInALink is the security boundary of the one writable
// section. These links become anchors on a page every authenticated user opens,
// so an accepted `javascript:` href is stored XSS with an administrator's
// blessing — and the value must not merely be dropped, because a caller that
// believes it saved a link and got a 200 will not look again.
func TestSaveRefusesAScriptURLInALink(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	for _, hostile := range []string{
		"javascript:alert(document.cookie)",
		"JavaScript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
	} {
		recorder := saveResources(t, router, map[string]any{
			"resources_documentation_links": []any{
				map[string]any{"title": "Docs", "url": hostile},
			},
		})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q status = %d, want 400 (body %s)", hostile, recorder.Code, recorder.Body.String())
		}
		if reason := decodeConfigBody(t, recorder).Error; reason == "" {
			t.Errorf("PUT %q was refused without a reason", hostile)
		}
	}

	// Nothing was written by any of them.
	if raw, present := storedValueSQL(t, pool, "resources", "resources_documentation_links"); present {
		t.Errorf("a refused link was stored anyway: %s", raw)
	}
	// And the whole save was refused, not partially applied.
	recorder := saveResources(t, router, map[string]any{
		"resources_documentation_title": "Should not land",
		"resources_documentation_links": []any{
			map[string]any{"title": "Docs", "url": "javascript:alert(1)"},
		},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mixed PUT status = %d, want 400", recorder.Code)
	}
	if _, present := storedValueSQL(t, pool, "resources", "resources_documentation_title"); present {
		t.Error("a refused save partially applied its valid keys")
	}
}

// TestSaveAcceptsOrdinaryHTTPLinks — the refusal above must not be a blanket
// one. An on-premises deployment's documentation genuinely lives on an intranet
// host, so the check is on the SCHEME, never on the host.
func TestSaveAcceptsOrdinaryHTTPLinks(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := saveResources(t, router, map[string]any{
		"resources_documentation_links": []any{
			map[string]any{"title": "Intranet", "url": "http://wiki.internal.corp/elitea"},
			map[string]any{"title": "Public", "url": "https://docs.example.com"},
			map[string]any{"title": "Placeholder", "url": ""},
			// A pasted URL routinely arrives with surrounding whitespace. It is
			// TRIMMED before the scheme is read, and the trim cuts both ways:
			// without it `url.Parse` sees no scheme at all and this legitimate
			// link would be refused, while a leading space would equally hide a
			// `javascript:` one from the check (asserted just above).
			map[string]any{"title": "Pasted", "url": "  https://docs.example.com/pasted \t"},
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	links, _ := readResources(t, router).Values["resources_documentation_links"].([]any)
	if len(links) != 4 {
		t.Fatalf("stored links = %v, want four", links)
	}
}

// TestSaveRefusesAnUnknownKey — silently dropping a field a caller believes it
// set is the failure this unit exists to remove.
func TestSaveRefusesAnUnknownKey(t *testing.T) {
	pool, router := newConfigEnvironment(t)
	recorder := saveResources(t, router, map[string]any{"resources_secret_backdoor": "x"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if _, present := storedValueSQL(t, pool, "resources", "resources_secret_backdoor"); present {
		t.Error("an unknown key was stored")
	}
}

// TestSaveRefusesTheWrongType — the schema declares the type and the store is
// JSONB, so without this a boolean field could hold the string "false", which
// every JavaScript consumer reads as true.
func TestSaveRefusesTheWrongType(t *testing.T) {
	_, router := newConfigEnvironment(t)
	for key, value := range map[string]any{
		"resources_documentation_enabled": "false",
		"resources_documentation_title":   42,
		"resources_documentation_links":   "https://example.com",
	} {
		recorder := saveResources(t, router, map[string]any{key: value})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT %s=%v status = %d, want 400 (body %s)",
				key, value, recorder.Code, recorder.Body.String())
		}
	}
}

// TestSaveRefusesACredentialSection — every `format: password` field in the
// schema is a real secret (an OIDC client secret, a LiteLLM master key, a
// Postgres URL carrying a password). This service has a vault for those; a
// plaintext JSONB column readable by every holder of `runtime.plugins` is not
// it. The outer guard is that the sections carrying them are unavailable; the
// validator itself is exercised directly in config_values_internal_test.go,
// because no AVAILABLE section declares such a field to route one through.
func TestSaveRefusesACredentialSection(t *testing.T) {
	pool, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodPut,
		"/admin/plugin_config_values/administration/auth",
		map[string]any{"values": map[string]any{"oidc_client_secret": "hunter2"}})
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("auth PUT status = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	var stored int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM centry.platform_config`).Scan(&stored); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != 0 {
		t.Error("a credential reached the platform-configuration table")
	}
}

/* ── unavailable sections ──────────────────────────────────────────────── */

// TestUnavailableSectionsRefuseWithAReason — the correction that matters most on
// this page. Before this unit every one of these answered 200: the GET with the
// schema's defaults, the PUT with an empty object and a success flag. "This
// deployment does not configure that" and "that is configured to its defaults"
// rendered identically, and the save reported success either way.
func TestUnavailableSectionsRefuseWithAReason(t *testing.T) {
	_, router := newConfigEnvironment(t)

	for _, section := range []string{
		// `governance` and `service_descriptors` are omitted here because they
		// additionally declare a `required_permission`, which is checked FIRST —
		// they are covered by the permission cases below. `advanced` used to be
		// the third name in that group and is gone from the schema entirely.
		// `voice_features` was in this list until unit A14's Features page:
		// #217 marked it unavailable because "nothing reads this setting yet",
		// which was true of the components it had looked at
		// (`features/chat-input`'s VoiceControlButton and VoiceMiniPlayer, both
		// unmounted) and false of the one it had not — `widgets/chat`'s
		// VoiceButton, which `/chat` mounts through ChatBox's slot bundle. It
		// is live now and is covered by the Features suite instead.
		//
		// `guardrails` left this list the same way, and for a reason of the same
		// shape: it was withheld as "Pylon plugin configuration nothing here
		// reads", which was true of the mechanism and stopped being true of the
		// claim once the toolkit surfaces, the write paths and the agent tool
		// freeze started reading it. Its consumers are recorded in
		// TestSchemaDeclaresAvailabilityForEverySection below, and its own
		// round trip is asserted in guardrails_postgres_integration_test.go.
		//
		// `dedicated_banner` and `maintenance` left this list together, and each
		// for the reason it was withheld under. The banner was "nothing reads
		// this setting yet" — true twice over, since the legacy SPA took it from
		// a BUILD-TIME variable, so the rows this form wrote were read nowhere;
		// `platform_settings` publishes them now and widgets/app-shell renders
		// them. Maintenance was "the switch would toggle a setting nothing
		// enforces"; internal/api/middleware's Maintenance gate enforces it.
		// Both are enumerated with their consumers in
		// TestSchemaDeclaresAvailabilityForEverySection below.
		//
		// `advanced` is not here either, and unlike every other name that left
		// this list it did not become available — it was REMOVED. Its subject is
		// Pylon plugin loading, which the target architecture drops on purpose,
		// so there was never going to be a consumer; a section withheld forever
		// is a promise that cannot be kept.
		//
		// `observability`, `runtime` and `admin_panel` are gone from this list
		// for the same reason `advanced` is: they were REMOVED, not ported. See
		// config_schemas.go's "Observability, Runtime and Admin Panel are GONE
		// too" note. The one field worth keeping — `analytics_enabled` — moved
		// to the Features page instead and is covered by the Features suite.
		"mcp_servers", "llm_proxy", "auth",
	} {
		target := "/admin/plugin_config_values/administration/" + section
		read := configDo(t, router, http.MethodGet, target, nil)
		if read.Code != http.StatusNotImplemented {
			t.Errorf("GET %s status = %d, want 501 (body %s)", section, read.Code, read.Body.String())
		}
		if reason := decodeConfigBody(t, read).Error; reason == "" {
			t.Errorf("GET %s was refused without a reason", section)
		}
		write := configDo(t, router, http.MethodPut, target, map[string]any{"values": map[string]any{}})
		if write.Code != http.StatusNotImplemented {
			t.Errorf("PUT %s status = %d, want 501", section, write.Code)
		}
	}
}

// TestGovernanceIsWithheldWithItsOwnReason — governance is not a Pylon surface;
// it has a real CRUD elsewhere. Collapsing it into the generic Pylon reason
// would tell the operator something false about where to go.
func TestGovernanceIsWithheldWithItsOwnReason(t *testing.T) {
	pool := newConfigPool(t)
	principal := auth.User{ID: "7", Email: "operator@example.com"}
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(
		grantingResolver("configuration.governance"),
	))
	router := configRouter(handler, &principal)

	recorder := configDo(t, router, http.MethodGet,
		"/admin/plugin_config_values/administration/governance", nil)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	reason := decodeConfigBody(t, recorder).Error
	if !bytes.Contains([]byte(reason), []byte("/admin/gateway/governance")) {
		t.Errorf("governance reason does not point at the surface that owns it: %q", reason)
	}
}

// TestUnknownSectionIs404 — distinct from 501. A typo in a URL and a section
// this deployment cannot serve are different facts.
func TestUnknownSectionIs404(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodGet,
		"/admin/plugin_config_values/administration/not_a_section", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

/* ── the section permission ────────────────────────────────────────────── */

// TestSectionPermissionFailsClosedWithoutAResolver — a section that declares a
// `required_permission` is a section whose contents are privileged. Answering it
// unchecked because a dependency was not wired is how implicit-admin bugs ship;
// `NewHandler` without `WithPermissionResolver` must refuse.
//
// `service_descriptors` declares `configuration.service_descriptors`. The 403
// must arrive even though the section is ALSO unavailable: the permission is
// checked first, because which sections a deployment can serve is itself
// information about the deployment.
func TestSectionPermissionFailsClosedWithoutAResolver(t *testing.T) {
	pool := newConfigPool(t)
	principal := auth.User{ID: "7", Email: "operator@example.com"}
	router := configRouter(admin.NewHandler(pool), &principal)

	for _, section := range []string{"service_descriptors", "governance"} {
		recorder := configDo(t, router, http.MethodGet,
			"/admin/plugin_config_values/administration/"+section, nil)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("GET %s status = %d, want 403 with no resolver wired (body %s)",
				section, recorder.Code, recorder.Body.String())
		}
	}
}

// TestSectionPermissionRefusesACallerWithoutIt — the same section with a
// resolver that grants something else.
func TestSectionPermissionRefusesACallerWithoutIt(t *testing.T) {
	pool := newConfigPool(t)
	principal := auth.User{ID: "7", Email: "operator@example.com"}
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(
		grantingResolver("runtime.plugins"),
	))
	router := configRouter(handler, &principal)

	recorder := configDo(t, router, http.MethodGet,
		"/admin/plugin_config_values/administration/governance", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("governance status = %d, want 403 for a caller without configuration.governance (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

/* ── the schema ────────────────────────────────────────────────────────── */

// TestSchemaDeclaresAvailabilityForEverySection — the page renders the reason it
// is given rather than deciding locally, so a section that reaches the client
// with neither fields nor a reason is a blank pane with no explanation.
func TestSchemaDeclaresAvailabilityForEverySection(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var schema struct {
		Sections []struct {
			ID                string           `json:"id"`
			Title             string           `json:"title"`
			UnavailableReason string           `json:"unavailable_reason"`
			Fields            []map[string]any `json:"fields"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if len(schema.Sections) == 0 {
		t.Fatal("no sections")
	}
	available := map[string]bool{}
	for _, section := range schema.Sections {
		if section.Title == "" {
			t.Errorf("section %q has no title", section.ID)
		}
		if section.UnavailableReason != "" {
			continue
		}
		available[section.ID] = true
		if len(section.Fields) == 0 {
			t.Errorf("section %q is offered as editable but declares no fields", section.ID)
		}
	}
	// The live set is enumerated, not counted. #217 pinned the COUNT at one, and
	// a count cannot tell "a section was made live with a consumer behind it"
	// from "a section lost its reason by accident" — both read as 2. Naming them
	// means a section becoming live has to be written down here, next to the
	// consumer that justifies it:
	//
	//	resources         → apps/elitea-web pages/help-center, through
	//	                    GET /admin/plugin_config_values/prompt_lib/resources
	//	mcp_configuration → eliteacore PlatformSettings (mcp_enabled,
	//	                    mcp_in_menu_enabled) and the 403 on the three MCP
	//	                    proxy/sync routes
	//	agent_publishing  → eliteacore Publish (the guardrail) and
	//	                    AgentCategories (the extra categories)
	//	voice_features    → eliteacore PlatformSettings (voice_features_enabled,
	//	                    voice_features_temporarily_disabled), read by
	//	                    widgets/chat's VoiceButton, which /chat mounts
	//	guardrails        → four readers, all of them enforcing rather than
	//	                    displaying: the toolkit TYPE surfaces
	//	                    (api/v2/toolkits/guardrails.go) drop blocked types
	//	                    and tools; the toolkit WRITE paths refuse a blocked
	//	                    type with a 403; the agent tool freeze
	//	                    (application/agentexecution/tools.go) strips them out
	//	                    of the execution input; and eliteacore
	//	                    PlatformSettings publishes blocked_toolkits so the
	//	                    product UI can mark an existing toolkit blocked
	//	dedicated_banner  → platformconfig.LoadBanner, marshalled by eliteacore
	//	                    PlatformSettings as `dedicated_banner` and rendered
	//	                    by apps/elitea-web widgets/app-shell's PlatformBanner
	//	skill_publishing  → skillpublish's publishBlocked (the guardrail, on both
	//	                    publish_skill and publish_skill_validate) and
	//	                    activeCategories, which merges the operator's extras
	//	                    into GET /skill_categories AND into the list publish
	//	                    validates `category` against; eliteacore
	//	                    PlatformSettings publishes the pair as
	//	                    is_skill_publish_blocked /
	//	                    skill_publish_whitelist_project_ids, which
	//	                    apps/elitea-web's skill editor gates its Publish
	//	                    control on
	//	maintenance       → platformconfig.LoadMaintenance, ENFORCED by
	//	                    internal/api/middleware's Maintenance gate (503 for
	//	                    every caller without runtime.plugins) and published
	//	                    on PlatformSettings so the SPA paints the splash
	//	                    rather than a wall of failed requests
	//	support_assistant → platformconfig.LoadSupportAssistant, read by
	//	                    internal/api/v2/supportassistant on every request it
	//	                    serves — the switch and the agent decide whether the
	//	                    widget renders at all, and the three display strings
	//	                    are echoed to it by GET /support_assistant/config.
	//	                    Rendered by apps/elitea-web widgets/support-assistant,
	//	                    which widgets/app-shell mounts
	//	analytics         → eliteacore PlatformSettings (analytics_enabled),
	//	                    read by apps/elitea-web's Settings page to hide its
	//	                    Analytics tab, AND enforced server-side by
	//	                    internal/api/router.go's requireAnalyticsEnabled on
	//	                    every `/analytics*` route. Ported off the withheld
	//	                    `observability` section rather than newly written —
	//	                    see config_schemas.go's "Observability, Runtime and
	//	                    Admin Panel are GONE too" note.
	//	branding          → platformconfig.LoadBranding, read by
	//	                    internal/api/v2/branding's Resolver on every
	//	                    bootstrap.js request (cached 15 s), merged over the
	//	                    file layer and served as window.elitea_brand to the
	//	                    web app, the admin console and the login page
	//	                    (ADR-0024 WP1)
	want := map[string]bool{
		"resources": true, "mcp_configuration": true, "branding": true,
		"agent_publishing": true, "skill_publishing": true,
		"voice_features": true, "support_assistant": true,
		"guardrails": true, "dedicated_banner": true,
		"maintenance": true, "analytics": true,
	}
	for id := range want {
		if !available[id] {
			t.Errorf("section %q should be available and is not", id)
		}
	}
	for id := range available {
		if !want[id] {
			t.Errorf("section %q became available with no consumer recorded here", id)
		}
	}
}

// TestEverySectionDeclaresWhichPageItBelongsTo — the placement the reference
// keeps in two client-side lists that must stay each other's complement
// (`FeaturesPage.jsx`'s six-section array and `ConfigurationPage.jsx`'s
// `MOVED_TO_FEATURES` plus three path prefixes). Here the server says it, so a
// section cannot end up on both pages or on neither.
func TestEverySectionDeclaresWhichPageItBelongsTo(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
	var schema struct {
		Sections []struct {
			ID   string `json:"id"`
			Page string `json:"page"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	features := map[string]bool{}
	for _, section := range schema.Sections {
		switch section.Page {
		case "features":
			features[section.ID] = true
		case "":
			// Configuration, by omission.
		default:
			t.Errorf("section %q declares unknown page %q", section.ID, section.Page)
		}
	}

	// The reference's five (was six — `resources` moved back to Configuration,
	// see below), in the reference's own order.
	for _, id := range []string{
		"mcp_configuration", "agent_publishing", "skill_publishing",
		"support_assistant", "voice_features",
	} {
		if !features[id] {
			t.Errorf("section %q is not on the Features page", id)
		}
	}
	// `analytics` is the reference's five PLUS one: it did not exist as a
	// Features section in the reference at all, and arrives here as a field
	// ported off the withheld Configuration `observability` section (see
	// config_schemas.go's "Observability, Runtime and Admin Panel are GONE
	// too" note), not as something the reference ever offered on this page.
	if !features["analytics"] {
		t.Error("analytics is not on the Features page")
	}
	if len(features) != 6 {
		t.Errorf("Features page has %d sections, want the reference's 5 plus analytics (6)", len(features))
	}
	// A2 (ELITEA-0032, onetest "Resources Section Appears in Admin
	// Configuration Before Banner"): `resources` is back on Configuration.
	// #217 originally put it there ("that is where the server's schema had
	// it"); a later unit moved it to Features, reasoning from the reference's
	// CLIENT-SIDE routing (`FeaturesPage.jsx`/`ConfigurationPage.jsx`); the
	// onetest suite — a behavioural record of the reference, not an inference
	// from its source — puts it on Configuration instead, before Banner. This
	// assertion is what stops it drifting back to Features.
	if features["resources"] {
		t.Error("resources must be on the Configuration page, not Features")
	}
}

// TestResourcesOrdersBeforeBannerOnConfiguration — A2 (ELITEA-0032)'s literal
// assertion: "Resources Section Appears in Admin Configuration Before Banner".
// Page placement alone (the test above) is not the same claim; an operator
// reads the Configuration sidebar top to bottom, so this pins the ORDER too.
func TestResourcesOrdersBeforeBannerOnConfiguration(t *testing.T) {
	_, router := newConfigEnvironment(t)
	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
	var schema struct {
		Sections []struct {
			ID    string `json:"id"`
			Page  string `json:"page"`
			Order int    `json:"order"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	var resourcesOrder, bannerOrder int
	var haveResources, haveBanner bool
	for _, section := range schema.Sections {
		switch section.ID {
		case "resources":
			if section.Page != "" {
				t.Fatalf("resources section.Page = %q, want Configuration (empty)", section.Page)
			}
			resourcesOrder, haveResources = section.Order, true
		case "dedicated_banner":
			if section.Page != "" {
				t.Fatalf("dedicated_banner section.Page = %q, want Configuration (empty)", section.Page)
			}
			bannerOrder, haveBanner = section.Order, true
		}
	}
	if !haveResources || !haveBanner {
		t.Fatalf("expected both resources (found=%v) and dedicated_banner (found=%v) in the schema", haveResources, haveBanner)
	}
	if resourcesOrder >= bannerOrder {
		t.Errorf("resources.order = %d, dedicated_banner.order = %d — resources must sort before Banner", resourcesOrder, bannerOrder)
	}
}

/* ── the Help Center's own read ────────────────────────────────────────── */

// TestPromptLibResourcesServesWhatTheAdminSaved closes issue #26 end to end:
// the value the Configuration page writes is the value the Help Center reads.
//
// The route this replaces returned `max_file_size`, `max_context_length` and
// friends — chat and upload limits — under no `values` wrapper, so a Help Center
// that called it received nothing it could use and every card rendered "No links
// configured".
func TestPromptLibResourcesServesWhatTheAdminSaved(t *testing.T) {
	_, router := newConfigEnvironment(t)

	if recorder := saveResources(t, router, map[string]any{
		"resources_release_notes_title": "What's new",
		"resources_release_notes_links": []any{
			map[string]any{"title": "8.2", "url": "https://example.com/releases/8.2"},
		},
		"resources_video_library_enabled": false,
	}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_values/prompt_lib/resources", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("public GET status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decodeConfigBody(t, recorder)
	if body.Values == nil {
		t.Fatal("public read has no `values` wrapper; that is the shape the Help Center indexes into")
	}
	if got := body.Values["resources_release_notes_title"]; got != "What's new" {
		t.Errorf("public title = %v, want the value the admin saved", got)
	}
	links, ok := body.Values["resources_release_notes_links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("public links = %v", body.Values["resources_release_notes_links"])
	}
	if got := body.Values["resources_video_library_enabled"]; got != false {
		t.Errorf("public enabled flag = %v, want the saved false", got)
	}
	// The chat/upload limits the old handler returned must be gone: they were
	// never resources configuration, and leaving them would keep the endpoint
	// answering two questions at once.
	if _, present := body.Values["max_file_size"]; present {
		t.Error("the public resources read still returns chat/upload limits")
	}
	// It must NOT leak another section, since it is ungated beyond authentication.
	if _, present := body.Values["oidc_client_id"]; present {
		t.Error("the public read leaked the auth section")
	}
}

/* ── pool ──────────────────────────────────────────────────────────────── */

func newConfigPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_config_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	return pool
}
