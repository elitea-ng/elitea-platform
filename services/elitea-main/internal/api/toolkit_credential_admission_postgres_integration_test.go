package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// A toolkit save may not store a credential reference that does not resolve for
// the caller (#613).
//
// # WHY THIS FILE IS IN PACKAGE api AND NOT IN THE TOOLKITS PACKAGE
//
// A test inside internal/api/v2/toolkits can only prove that the handler
// FUNCTION checks something. It cannot see whether anything composes the
// checker, and an option that exists but is never appended at the composition
// root is this repository's single most repeated defect (#128's unwired
// `.WithPool`, #301/#314/#370's nil principal validator). So this runs through
// the real NewRouter, with the real chi routes, the real permission gate and a
// real PostgreSQL database.
//
// # WHAT MAKES IT DISCRIMINATE
//
// Before the fix, the first request below answered 201 and left a row in
// p_1.elitea_tools naming a credential that only exists in p_2 — verified by
// running this file against the unfixed handler. Each refusal therefore asserts
// four things, because three of them can be satisfied for the wrong reason:
//
//   - the exact status 400. 403 (no permission), 404 (route not mounted) and
//     500 (missing tenant schema) are all "not 201" and would otherwise pass;
//   - the field the error is keyed to, in the two-element `loc` shape the web
//     client's parseValidationErrors requires — a one-element `loc` is silently
//     dropped there, so a refusal the user never sees is not a fix;
//   - that the message leaks neither the credential title nor its owning
//     project, which is what the resolver's violation type is designed for;
//   - that NOTHING was written. The status alone does not say that.
//
// Two negative controls sit beside them, because a gate written slightly too
// strictly is worse than no gate: an unknown toolkit type (`custom`, which the
// create form serves and the pinned SDK snapshot does not describe) and a
// credential-free `openapi` toolkit (an anonymous API) must both still save.
// A github toolkit naming a credential that DOES exist in project 1 must save
// too, or the "refusal" is just a broken endpoint.
//
// mcp deliberately appears nowhere here: its pinned schema has an empty
// properties block, so it can never produce a violation and a green mcp save
// would prove nothing.
//
// # WHAT THE THIRD TEST ADDS
//
// TestTheCredentialGateCannotBeSwitchedOffByTheRequestBody covers the way the
// first version of this gate could be turned off from the request body, which
// none of the assertions above could see: they only ever send bodies the
// resolver is willing to walk. See its own comment.
//
// The branch-level triage (refuse / proceed / unavailable), the two-element
// `loc` wire shape and the discard invariant are ALSO pinned without a database
// in internal/api/v2/toolkits/settings_validation_test.go, because everything in
// this file t.Skip()s wholesale without ELITEA_TEST_DATABASE_URL.

// The credential seeded into the OTHER project. It exists, it is a real github
// credential, and the caller cannot see it: p_1 has no row with this title.
const foreignCredentialTitle = "admission-foreign-credential"

// The credential seeded into the caller's own project, for the control.
const localCredentialTitle = "admission-local-credential"

func TestSavingAToolkitWhoseCredentialLivesInAnotherProjectIsRefused(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)
	seedAdmissionCredentials(t, pool)

	router := newToolkitAdmissionRouter(t, pool)

	// ── the refusal, on create ─────────────────────────────────────────────
	status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", githubToolkitBody("foreign-toolkit", foreignCredentialTitle))
	if status != http.StatusBadRequest {
		t.Fatalf("creating a toolkit that names project 2's credential answered %d, want 400.\n"+
			"  201 means the save-time credential gate did not run at all.\n"+
			"  403/404 mean the request never reached the handler, so this test proves nothing.\n"+
			"  Body: %s", status, body)
	}
	assertAdmissionFieldError(t, body, "github_configuration", "configuration_not_found")
	assertNoToolkitRows(t, pool, "a refused create")

	// ── the same body, with a credential the caller CAN see ────────────────
	// Without this the test above is satisfied by an endpoint that refuses
	// everything.
	accepted := githubToolkitBody("local-toolkit", localCredentialTitle)
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", accepted)
	if status != http.StatusCreated {
		t.Fatalf("creating a toolkit that names project 1's own credential answered %d, want 201.\n"+
			"  The gate is refusing a credential the caller can see. Body: %s", status, body)
	}
	created := decodeJourneyJSON(t, body)
	toolkitID := fmt.Sprintf("%v", created["id"])

	// ── the ACCEPTED save stored the reference, not the expansion ──────────
	// refuseUnresolvableToolkitSettings uses only the resolver's ERROR and
	// discards its returned map. That map is the configuration EXPANDED and
	// stamped with __elitea_frozen_configuration_v1; persisting it would replace
	// a live reference with a frozen snapshot and forge a vault owner through
	// configuration_project_id.
	//
	// Nothing pinned that until now: turning the discard into a copy-back left
	// the whole suite green while this row silently became
	// {"github_configuration":{"__elitea_frozen_configuration_v1":true,
	// "configuration_project_id":1,…},"pgvector_configuration":{}}. The
	// before/after comparison further down cannot see it — it brackets a
	// REFUSED update, so both sides move together.
	if stored, want := storedToolkitSettings(t, pool, toolkitID), canonicalAdmissionJSON(t, accepted["settings"]); stored != want {
		t.Fatalf("an ACCEPTED save did not persist the settings the request sent.\n"+
			"  sent:      %s\n"+
			"  persisted: %s\n"+
			"  The resolver's OUTPUT must never be written back.", want, stored)
	}

	// ── the refusal, on update ─────────────────────────────────────────────
	// Update replaces the whole settings column, so an accepted update is how a
	// good reference becomes a foreign one after the fact.
	before := storedToolkitSettings(t, pool, toolkitID)
	status, body = serveJourney(t, router, http.MethodPut,
		"/api/v2/elitea_core/tool/prompt_lib/1/"+toolkitID, githubToolkitBody("local-toolkit", foreignCredentialTitle))
	if status != http.StatusBadRequest {
		t.Fatalf("repointing a saved toolkit at project 2's credential answered %d, want 400. Body: %s", status, body)
	}
	assertAdmissionFieldError(t, body, "github_configuration", "configuration_not_found")
	if after := storedToolkitSettings(t, pool, toolkitID); after != before {
		t.Fatalf("a refused update still rewrote the stored settings.\n  before: %s\n  after:  %s", before, after)
	}

	// ── a settings-free PATCH still works ──────────────────────────────────
	// The stored type is what a settings-only body is validated against; a body
	// with no settings at all must not be validated as an empty one, or an
	// already-broken toolkit could never be renamed.
	if status, body := serveJourney(t, router, http.MethodPatch,
		"/api/v2/elitea_core/tool/prompt_lib/1/"+toolkitID,
		map[string]any{"description": "renamed"}); status != http.StatusOK {
		t.Fatalf("a settings-free PATCH answered %d, want 200. Body: %s", status, body)
	}
}

// The gate must stay silent about everything it cannot speak for.
func TestToolkitSavesTheCredentialGateCannotJudgeStillSucceed(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)
	seedAdmissionCredentials(t, pool)

	router := newToolkitAdmissionRouter(t, pool)

	// `custom` is served by the create form's type list and is absent from the
	// pinned SDK snapshot, so the resolver answers "no schema". Refusing on that
	// would make most of the create form unusable.
	if status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", map[string]any{
			"name":     "custom-toolkit",
			"type":     "custom",
			"settings": map[string]any{"anything": map[string]any{"elitea_title": foreignCredentialTitle, "private": false}},
		}); status != http.StatusCreated {
		t.Fatalf("creating a toolkit of a type the pinned snapshot does not describe answered %d, want 201. Body: %s", status, body)
	}

	// An openapi toolkit with NO credential is an anonymous API, which the
	// resolver deliberately rewrites to `{}` rather than refusing.
	if status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", map[string]any{
			"name":     "anonymous-openapi",
			"type":     "openapi",
			"settings": map[string]any{"spec": "{}"},
		}); status != http.StatusCreated {
		t.Fatalf("creating an openapi toolkit with no credential answered %d, want 201. Body: %s", status, body)
	}
}

// THE GATE MUST NOT HAVE AN OFF SWITCH THE REQUEST BODY CAN REACH.
//
// The resolver raises ErrInvalidCurrentToolkitSettings for input the body fully
// controls — a key named __elitea_frozen_configuration_v1 at any depth, or a
// blob past the depth-32 / 16384-node / 4 MB budgets. While the handler triaged
// that sentinel as "proceed", appending either of those to a body whose
// credential lives in another project turned the 400 below into a 201 that
// persisted the foreign reference AND, for the marker, the marker itself.
//
// Both bodies here are byte-identical to the one
// TestSavingAToolkitWhoseCredentialLivesInAnotherProjectIsRefused sends, plus
// exactly one addition. That is what makes this discriminating: the 400 is
// already established for the body without the addition, so a 2xx here can only
// mean the addition disabled the gate.
//
// The third case is the negative control. Padding that stays INSIDE the depth
// budget must still save, or these two refusals would be indistinguishable from
// "refuse anything unfamiliar".
func TestTheCredentialGateCannotBeSwitchedOffByTheRequestBody(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)
	seedAdmissionCredentials(t, pool)

	router := newToolkitAdmissionRouter(t, pool)

	// ── the marker ─────────────────────────────────────────────────────────
	marked := githubToolkitBody("marker-bypass", foreignCredentialTitle)
	marked["settings"].(map[string]any)[configurationapp.CurrentFrozenConfigurationMarker] = true
	status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", marked)
	if status != http.StatusBadRequest {
		t.Fatalf("a foreign-credential save carrying %s answered %d, want 400.\n"+
			"  201 means one extra key in the body switched the whole credential gate off,\n"+
			"  and persisted a forged vault owner alongside it. Body: %s",
			configurationapp.CurrentFrozenConfigurationMarker, status, body)
	}
	if message, _ := decodeJourneyJSON(t, body)["error"].(string); !strings.Contains(message, configurationapp.CurrentFrozenConfigurationMarker) {
		t.Fatalf("the marker refusal does not name the reserved key, so the caller cannot tell "+
			"which key to drop: %q", message)
	}
	assertNoToolkitRows(t, pool, "a save carrying the frozen-configuration marker")

	// ── past the depth budget ──────────────────────────────────────────────
	deep := githubToolkitBody("depth-bypass", foreignCredentialTitle)
	deep["settings"].(map[string]any)["padding"] = nestedPadding(40)
	status, body = serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", deep)
	if status != http.StatusBadRequest {
		t.Fatalf("a foreign-credential save padded past the depth budget answered %d, want 400.\n"+
			"  201 means a blob the resolver refuses to walk switched the credential gate off. Body: %s",
			status, body)
	}
	assertNoToolkitRows(t, pool, "a save padded past the depth budget")

	// ── the control ────────────────────────────────────────────────────────
	shallow := githubToolkitBody("padded-but-legal", localCredentialTitle)
	shallow["settings"].(map[string]any)["padding"] = nestedPadding(8)
	if status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/elitea_core/tools/prompt_lib/1", shallow); status != http.StatusCreated {
		t.Fatalf("a padded body that stays inside the depth budget answered %d, want 201.\n"+
			"  The two refusals above must be about the marker and the budget, not about padding. Body: %s",
			status, body)
	}
}

// nestedPadding builds `{"n":{"n":{…}}}` `levels` deep.
func nestedPadding(levels int) map[string]any {
	padding := map[string]any{"n": "leaf"}
	for range levels {
		padding = map[string]any{"n": padding}
	}
	return padding
}

// canonicalAdmissionJSON re-encodes through encoding/json, whose map keys are
// sorted, so the comparison sees content and not key ordering — the same
// normalization storedToolkitSettings applies to the row it reads.
func canonicalAdmissionJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return string(encoded)
}

// newToolkitAdmissionRouter builds the router WITH the save-time validator, out
// of the same composition cmd/elitea-main uses.
func newToolkitAdmissionRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	configurations, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, 1, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(configurations.Destroy)
	validator, err := configurations.NewToolkitSettingsValidator(pool)
	if err != nil {
		t.Fatalf("compose the toolkit settings validator: %v", err)
	}
	return NewRouter(RouterConfig{
		Pool:                     pool,
		AuthValidator:            apimw.TokenValidator(credentialJourneyValidator{}),
		PrincipalValidator:       testPrincipalValidator{},
		WebhookRepo:              emptyWebhookRepo{},
		EventSource:              newEventSource(),
		ToolkitSettingsValidator: validator,
	})
}

func githubToolkitBody(name, credentialTitle string) map[string]any {
	return map[string]any{
		"name": name,
		"type": "github",
		"settings": map[string]any{
			"repository": "octocat/hello-world",
			// The exact two-key shape the web credential picker writes
			// (apps/elitea-web/src/pages/toolkits/lib/credentialPicker.tsx's
			// toStoredValue). The resolver refuses any other key count, so a
			// third key here would make this test pass for the wrong reason.
			"github_configuration": map[string]any{"elitea_title": credentialTitle, "private": false},
		},
	}
}

// seedAdmissionCredentials puts one github credential in the caller's project
// and one, with a different title, in a project the caller is not a member of.
func seedAdmissionCredentials(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()

	for _, seed := range []struct {
		schema    string
		projectID int
		title     string
	}{
		{"p_2", 2, foreignCredentialTitle},
		{"p_1", 1, localCredentialTitle},
	} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.configuration (project_id, label, elitea_title, type, section, data, shared, created_at)
VALUES ($1, $2, $2, 'github', 'credentials', '{"base_url": "https://api.github.com"}'::jsonb, false, now() AT TIME ZONE 'UTC')`,
			seed.schema), seed.projectID, seed.title); err != nil {
			t.Fatalf("seed the %s credential: %v", seed.schema, err)
		}
	}

	// The fixture must not be satisfiable from project 1, or the refusal below
	// would be measuring nothing.
	var visible int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.configuration WHERE elitea_title = $1`, foreignCredentialTitle).
		Scan(&visible); err != nil {
		t.Fatalf("count the foreign credential in p_1: %v", err)
	}
	if visible != 0 {
		t.Fatalf("p_1 already holds %q; the cross-project fixture is not what it claims", foreignCredentialTitle)
	}
}

// assertAdmissionFieldError pins the whole client contract for one refusal.
func assertAdmissionFieldError(t *testing.T, body, field, code string) {
	t.Helper()
	decoded := decodeJourneyJSON(t, body)
	raw, ok := decoded["settings_errors"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("the refusal carries no settings_errors, so the toolkit form has nothing to show. Body: %s", body)
	}
	entry, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("settings_errors[0] is not an object. Body: %s", body)
	}
	loc, ok := entry["loc"].([]any)
	if !ok || len(loc) != 2 || loc[0] != "settings" || loc[1] != field {
		// A one-element loc is what this handler's older /toolkit_validator
		// route emits, and the web client's locFieldKey drops it on the floor.
		t.Fatalf("settings_errors[0].loc is %v, want [\"settings\" %q].\n"+
			"  The web client keys each error by loc[1] and DISCARDS entries without one, so the "+
			"user would see a failed save with no message anywhere.", entry["loc"], field)
	}
	if entry["code"] != code {
		t.Fatalf("settings_errors[0].code is %v, want %q. Body: %s", entry["code"], code, body)
	}
	if message, _ := entry["msg"].(string); message == "" {
		t.Fatalf("settings_errors[0].msg is empty; the violation carries no text of its own, so the handler must supply one. Body: %s", body)
	}
	if strings.Contains(body, foreignCredentialTitle) {
		t.Fatalf("the refusal names the credential the caller may not see (%q). Body: %s", foreignCredentialTitle, body)
	}
}

func assertNoToolkitRows(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.elitea_tools`).Scan(&rows); err != nil {
		t.Fatalf("count p_1.elitea_tools: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%s still wrote %d row(s) into p_1.elitea_tools. The status is not the assertion that matters here.", what, rows)
	}
}

func storedToolkitSettings(t *testing.T, pool *pgxpool.Pool, toolkitID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), credentialJourneyDeadline)
	defer cancel()
	var settings string
	if err := pool.QueryRow(ctx,
		`SELECT settings::text FROM p_1.elitea_tools WHERE id = $1::int`, toolkitID).Scan(&settings); err != nil {
		t.Fatalf("read the stored settings of toolkit %s: %v", toolkitID, err)
	}
	// Normalized so a jsonb key reordering cannot fail the comparison.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		t.Fatalf("decode the stored settings: %v", err)
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-encode the stored settings: %v", err)
	}
	return string(normalized)
}
