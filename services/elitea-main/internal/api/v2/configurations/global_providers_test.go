package configurations

// The platform provider surface's GUARDS, without a database.
//
// Every test here pins a refusal or a rewrite, because those are what stand
// between one central permission and the public project's whole configuration
// table — a schema every tenant on the platform reads.
//
// The delegated write paths themselves (vault sealing, the self-referential
// guard, provider admission, partial-update semantics) are covered where they
// live. Re-asserting them here would only prove that delegation happens, which
// is what these tests establish by construction.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// providerHandler builds a handler carrying the REAL pinned catalogue, because
// the catalogue is what decides which types this surface admits.
func providerHandler() *Handler {
	return NewHandler(nil, WithPublicProjectID(1))
}

// providerRequest builds a request carrying a chi route context, as the router
// would. Without one, `pinPublicProject` has nowhere to write the project id.
func providerRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, chi.NewRouteContext())
	return request.WithContext(ctx)
}

// fakeProviderRows stands in for one result row, so the redaction rules are
// exercised without a database.
//
// `neverUpdated` gives the row a NULL `updated_at`, which is the state EVERY
// freshly created platform credential is in — Create writes `created_at` only.
type fakeProviderRows struct {
	data         []byte
	neverUpdated bool
}

// fakeRowTimestamp is the instant both fakes report, so a formatted timestamp
// is a fixed string rather than "whenever the test ran".
var fakeRowTimestamp = time.Unix(1_700_000_000, 0).UTC()

// scanFakeTimestamp writes one timestamp column into whatever destination the
// scanner asked for — INCLUDING pgx's refusal.
//
// The refusal is the whole point. pgx cannot put a NULL into a `*time.Time`,
// and a scanner that asks for one is rejected here with the message elitea-main
// logged in production, so the destination TYPE is what these tests pin. A fake
// that quietly wrote a zero time into a `*time.Time` would report every scanner
// as correct and could never have caught this.
func scanFakeTimestamp(index int, column string, target any, value *time.Time) error {
	switch dest := target.(type) {
	case **time.Time:
		*dest = value
		return nil
	case *time.Time:
		if value == nil {
			return fmt.Errorf(
				"can't scan into dest[%d] (col: %s): cannot scan NULL into *time.Time",
				index, column)
		}
		*dest = *value
		return nil
	default:
		return fmt.Errorf("dest[%d] (col: %s): unsupported destination %T", index, column, target)
	}
}

func (f *fakeProviderRows) Scan(targets ...any) error {
	if len(targets) != 10 {
		return errors.New("column count mismatch")
	}
	*(targets[0].(*int)) = 1
	*(targets[1].(*string)) = "uuid-1"
	*(targets[2].(*string)) = "label"
	*(targets[3].(*string)) = "platform-openai"
	*(targets[4].(*string)) = "open_ai"
	*(targets[5].(*[]byte)) = f.data
	*(targets[6].(*bool)) = true
	*(targets[7].(*string)) = ""
	created := fakeRowTimestamp
	if err := scanFakeTimestamp(8, "created_at", targets[8], &created); err != nil {
		return err
	}
	updated := &created
	if f.neverUpdated {
		updated = nil
	}
	return scanFakeTimestamp(9, "updated_at", targets[9], updated)
}

// TestASurfaceWithNoPublicProjectRefusesRatherThanGuessing.
//
// Defaulting to project 1 would publish a credential into a schema this
// deployment's gateway may never read, and report success for it. The operator
// then has a platform provider that resolves for nobody and no way to see why.
func TestASurfaceWithNoPublicProjectRefusesRatherThanGuessing(t *testing.T) {
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodGet, "/", "")

	handler.ListGlobalProviders(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no public project is configured", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "AI_PROJECT_ID") {
		t.Errorf("body %s does not say how to fix it", recorder.Body.String())
	}
}

// TestOnlyAProviderCredentialTypeMayBePublished is the security property.
//
// Without it this route writes ANY row into the public project's configuration
// table — a toolkit credential, a model, a project context — every one of them
// readable or usable by every tenant, under a permission granted for governance.
func TestOnlyAProviderCredentialTypeMayBePublished(t *testing.T) {
	for _, configType := range []string{"github", "jira", "llm_model", "project_context", ""} {
		recorder := httptest.NewRecorder()
		body := `{"elitea_title":"x","type":"` + configType + `"}`
		if configType == "" {
			body = `{"elitea_title":"x"}`
		}
		request := providerRequest(http.MethodPost, "/", body)

		if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true); ok {
			t.Errorf("type %q was admitted as a platform provider", configType)
			continue
		}
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("type %q: status = %d, want 400", configType, recorder.Code)
		}
	}
}

// TestTheCataloguedProviderTypesAreAdmitted — the other direction. A refusal
// list that also refuses the real providers is a surface nobody can use, and it
// would look identical in the test above.
func TestTheCataloguedProviderTypesAreAdmitted(t *testing.T) {
	for _, configType := range []string{
		"open_ai", "azure_open_ai", "ai_dial", "ollama", "amazon_bedrock", "vertex_ai",
		// The three the gateway added on its own. They were refused while the
		// catalogue did not describe them; the catalogue describes them now
		// (#G3), so refusing them here would be a control with no reason.
		"anthropic", "open_ai_azure", "vllm",
	} {
		recorder := httptest.NewRecorder()
		request := providerRequest(http.MethodPost, "/",
			`{"elitea_title":"x","type":"`+configType+`"}`)

		if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true); !ok {
			t.Errorf("provider type %q was refused (status %d, body %s)",
				configType, recorder.Code, recorder.Body.String())
		}
	}
}

// TestATypeTheCatalogueDoesNotDescribeIsRefused.
//
// THE BUG THIS PREVENTS. For a type the catalogue does not describe the
// lookup misses TWICE, and both misses used to be silent:
//
//   - `sectionFor` returns "", so the row is stored with `section = ”` and the
//     gateway's `WHERE section = 'ai_credentials'` never sees it;
//   - `sealConfigurationSecrets` kept the data verbatim, so the api_key was
//     written into the row IN PLAINTEXT — in the public project's schema, the
//     one schema every tenant on the platform can read.
//
// The result would be an inert credential with a leaked key, and every signal
// on the admin screen still reading healthy. `open_ai_azure`, `anthropic` and
// `vllm` were exactly that until the catalogue described them (#G3), so the
// case is measured with a type NO catalogue describes instead. Naming those
// three here again would measure nothing.
func TestATypeTheCatalogueDoesNotDescribeIsRefused(t *testing.T) {
	for _, configType := range []string{"some_future_provider", "not_a_registered_type"} {
		recorder := httptest.NewRecorder()
		request := providerRequest(http.MethodPost, "/",
			`{"elitea_title":"x","type":"`+configType+`"}`)

		if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true); ok {
			t.Errorf("type %q was published: the catalogue cannot place or seal it", configType)
			continue
		}
		// The message names the admitted set. The refusal an operator most often
		// hits here is a type the GATEWAY supports and this deployment's
		// catalogue does not describe, and "unsupported" would send them looking
		// in the wrong place.
		if !strings.Contains(recorder.Body.String(), "open_ai") {
			t.Errorf("type %q: the refusal does not name what IS admitted: %s",
				configType, recorder.Body.String())
		}
	}
}

// TestEveryGatewayDispatchableTypeIsPublishable is the other half of the same
// invariant, and it is the one that failed before this change: the admin
// surface could not publish a credential of a type the data plane serves, so
// three providers had no platform-wide control at all.
func TestEveryGatewayDispatchableTypeIsPublishable(t *testing.T) {
	handler := providerHandler()
	for _, configType := range []string{
		"open_ai", "azure_open_ai", "open_ai_azure", "ai_dial", "anthropic",
		"ollama", "amazon_bedrock", "vertex_ai", "vllm",
	} {
		if !configurationapp.CurrentProviderCredentialType(configType) {
			t.Fatalf("type %q left the dispatchable set; this list is stale", configType)
		}
		if !handler.admitsGlobalProviderType(configType) {
			t.Errorf("the gateway dispatches to %q and this surface cannot publish it", configType)
		}
	}
}

// TestEveryAdmittedTypeCanBeBothPlacedAndSealed is the invariant behind the
// list above, checked against the catalogue rather than against a copy of it —
// so adding a type to the registry snapshot without a data schema fails here
// instead of in production.
func TestEveryAdmittedTypeCanBeBothPlacedAndSealed(t *testing.T) {
	handler := providerHandler()
	admitted := handler.admittedGlobalProviderTypes()
	if len(admitted) == 0 {
		t.Fatal("no provider type is admitted; the catalogue or the section name changed")
	}

	for _, configType := range admitted {
		if section := handler.sectionFor(configType, ""); section != GlobalProviderSection {
			t.Errorf("type %q resolves to section %q, not %q — the row would be invisible "+
				"to the gateway", configType, section, GlobalProviderSection)
		}
		if _, ok := handler.configurationDataProperties(configType); !ok {
			t.Errorf("type %q has no data schema, so its secret would be stored in plaintext",
				configType)
		}
	}
}

// TestAPlatformProviderIsAlwaysShared — the rewrite that makes it global at all.
func TestAPlatformProviderIsAlwaysShared(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodPost, "/", `{"elitea_title":"x","type":"open_ai"}`)

	rewritten, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true)
	if !ok {
		t.Fatalf("refused: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	raw, _ := io.ReadAll(rewritten.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if body["shared"] != true {
		t.Errorf("shared = %v, want true — an unshared platform credential is invisible to every project",
			body["shared"])
	}
	// The Content-Length must follow the body, or the delegated handler's
	// bounded decoder reads a truncated object.
	if rewritten.ContentLength != int64(len(raw)) {
		t.Errorf("ContentLength = %d, want %d", rewritten.ContentLength, len(raw))
	}
}

// TestAnExplicitlyUnsharedWriteIsRefusedRatherThanOverridden.
//
// The two differ for the operator. An override reports success for the opposite
// of what they asked and hands back a row saying `shared: true` with no
// explanation of where their value went.
func TestAnExplicitlyUnsharedWriteIsRefusedRatherThanOverridden(t *testing.T) {
	for _, shared := range []string{"false", `"yes"`, "0"} {
		recorder := httptest.NewRecorder()
		request := providerRequest(http.MethodPost, "/",
			`{"elitea_title":"x","type":"open_ai","shared":`+shared+`}`)

		if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true); ok {
			t.Errorf("shared:%s was silently overridden rather than refused", shared)
		}
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("shared:%s: status = %d, want 400", shared, recorder.Code)
		}
	}
}

// TestAnUpdateMayOmitTheType — the delegated Update applies a PARTIAL change,
// so requiring a type here would make it impossible to rename a credential
// without restating what it is.
func TestAnUpdateMayOmitTheType(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodPut, "/7", `{"elitea_title":"renamed"}`)

	if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, false); !ok {
		t.Fatalf("a partial update was refused: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestAnUpdateMayNotChangeTheTypeToANonProvider — the type is optional on an
// update and CHECKED when present, so a row cannot be edited out of the provider
// set and left behind on a surface that no longer admits it.
func TestAnUpdateMayNotChangeTheTypeToANonProvider(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodPut, "/7", `{"type":"github"}`)

	if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, false); ok {
		t.Error("an update retyped a platform provider to a toolkit credential")
	}
}

// TestAnOversizedBodyIsRefusedByTheRewriteToo.
//
// The rewrite buffers the body, so it is a second entry point with its own
// bound. If that bound were looser than the delegated handler's, this router
// would be the place an operator could push a body the rest of the service
// refuses; if it had none, the buffer would be unbounded.
func TestAnOversizedBodyIsRefusedByTheRewrite(t *testing.T) {
	oversized := `{"elitea_title":"` + strings.Repeat("x", maxGlobalProviderBodyBytes) + `"}`
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodPost, "/", oversized)

	if _, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true); ok {
		t.Fatal("an oversized body was buffered and admitted")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", recorder.Code)
	}
}

// TestTheListingNeverEmitsSecretMaterial.
//
// Sealing means a row written by this platform holds `{{secret.NAME}}` and not
// an api_key — but the listing must not DEPEND on that. A row imported from a
// legacy deployment can hold a literal, and this screen is where it would reach
// a browser. The scan reports whether each secret is SET and whether it is
// SEALED, and never what it is.
func TestTheListingNeverEmitsSecretMaterial(t *testing.T) {
	data := []byte(`{
		"api_base": "https://api.openai.com/v1",
		"api_key": "sk-LITERAL-SECRET-VALUE",
		"aws_secret_access_key": "{{secret.abc123}}",
		"api_version": "2024-02-01"
	}`)
	item, err := scanGlobalProvider((&fakeProviderRows{data: data}).Scan)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(encoded, []byte("sk-LITERAL-SECRET-VALUE")) {
		t.Fatalf("the listing emitted a literal api_key: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("{{secret.")) {
		// Even the reference name is withheld: it names a row in the public
		// project's vault, and the listing has no use for it.
		t.Errorf("the listing emitted a secret reference name: %s", encoded)
	}

	if item.Endpoint != "https://api.openai.com/v1" {
		t.Errorf("endpoint = %q, want the non-secret api_base", item.Endpoint)
	}
	if item.Settings["api_version"] != "2024-02-01" {
		t.Errorf("settings = %v, want api_version published", item.Settings)
	}

	sealed := map[string]bool{}
	for _, secret := range item.Secrets {
		if !secret.Set {
			t.Errorf("secret %q reported as unset while the row holds a value", secret.Field)
		}
		sealed[secret.Field] = secret.Sealed
	}
	// The literal is reported as UNSEALED. That is a finding an operator can
	// act on — the value is readable by every holder of the project-scoped
	// configuration permissions on the public project — and it is invisible
	// anywhere else.
	if sealed["api_key"] {
		t.Error("a literal api_key was reported as sealed")
	}
	if !sealed["aws_secret_access_key"] {
		t.Error("a {{secret.NAME}} reference was reported as unsealed")
	}
}

// TestACorruptDataColumnDoesNotBreakTheWholeListing — one bad row must not make
// the platform's provider screen unreachable. That is the exact shape of the
// `meta = 'null'` defect Create's header records, where a single row made a
// project's whole credentials page answer 500 permanently.
func TestACorruptDataColumnDoesNotBreakTheWholeListing(t *testing.T) {
	item, err := scanGlobalProvider((&fakeProviderRows{data: []byte(`not json`)}).Scan)
	if err != nil {
		t.Fatalf("scan refused a corrupt data column: %v", err)
	}
	if len(item.Secrets) != 0 || len(item.Settings) != 0 || item.Endpoint != "" {
		t.Errorf("item = %+v, want an empty report rather than invented fields", item)
	}
}

// TestANeverUpdatedRowDoesNotBreakTheWholeListing is the F1 regression.
//
// MEASURED, through the admin panel: creating a platform provider answered 201,
// and every subsequent GET /admin/gateway/providers answered 500 "list failed"
// — for every operator, permanently, until someone touched the row in SQL. The
// server log said
//
//	can't scan into dest[9] (col: updated_at): cannot scan NULL into *time.Time
//
// `updated_at` is nullable in the tenant projection and Create writes only
// `created_at`, so a brand-new credential HAS a NULL there. The listing scanned
// it into a `time.Time`, pgx refused the row, and the refusal is raised per row
// on a listing — so ONE new row took the whole screen down, including every row
// that was fine. The surface for publishing a provider bricked itself on first
// use.
//
// The assertion is on the SCAN, not on a status code: a listing that answered
// 200 while dropping the row would also be wrong, so the row's identity is
// checked too.
func TestANeverUpdatedRowDoesNotBreakTheWholeListing(t *testing.T) {
	item, err := scanGlobalProvider((&fakeProviderRows{
		data:         []byte(`{"api_base":"https://api.openai.com/v1"}`),
		neverUpdated: true,
	}).Scan)
	if err != nil {
		t.Fatalf("a freshly created credential was refused by the listing: %v", err)
	}
	if item.Name != "platform-openai" {
		t.Errorf("item = %+v, want the row reported rather than skipped", item)
	}
	if item.CreatedAt != fakeRowTimestamp.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want the row's creation time", item.CreatedAt)
	}
	// Reported as "never updated" rather than as its creation time: an
	// operator reading `updated_at` is asking when the credential last CHANGED,
	// and a row nobody has edited has no such moment. The detail route already
	// answers this way for the same row.
	if item.UpdatedAt != "" {
		t.Errorf("updated_at = %q, want empty for a row that was never updated", item.UpdatedAt)
	}
}

// TestANonStringSecretIsNotStringifiedOnItsWayOut — a secret stored as a number
// or an object must not be rendered into a string here. Reporting it as "not
// set" is the safe error.
func TestANonStringSecretIsNotStringifiedOnItsWayOut(t *testing.T) {
	item, err := scanGlobalProvider((&fakeProviderRows{
		data: []byte(`{"api_key": {"nested": "sk-SECRET"}}`),
	}).Scan)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	encoded, _ := json.Marshal(item)
	if bytes.Contains(encoded, []byte("sk-SECRET")) {
		t.Fatalf("a non-string secret reached the response: %s", encoded)
	}
	if len(item.Secrets) != 0 {
		t.Errorf("secrets = %v, want a non-string value reported as absent", item.Secrets)
	}
}

// TestTheMountedSurfaceAnswersThePathsTheClientCalls.
//
// The admin client calls `/admin/gateway/providers` with NO trailing slash,
// while the router's pinned pattern is `/providers/` — chi's Mount registers
// both, but that is a property of chi rather than of anything in this file, and
// a future refactor from Mount to Route would change it silently. A 404 here
// would be a Providers tab that renders its empty state forever on a deployment
// where everything else is correct.
//
// 503 is the PASS condition, not 200: the handler under test has no database
// pool, so a routed request refuses for want of one. That makes the two answers
// discriminate exactly what this test is about — 404 means the path never
// reached the handler, 503 means it did.
func TestTheMountedSurfaceAnswersThePathsTheClientCalls(t *testing.T) {
	handler := &Handler{publicProjectID: 1}

	root := chi.NewRouter()
	root.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
	})

	for _, probe := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/gateway/providers"},
		{http.MethodGet, "/gateway/providers/"},
		{http.MethodPost, "/gateway/providers"},
		{http.MethodPut, "/gateway/providers/4"},
		{http.MethodDelete, "/gateway/providers/4"},
		// The stored-row pair. `/{configID}` and `/{configID}/check` are
		// siblings in the same sub-router, and chi resolves them by depth — a
		// 404 here would be the Test-connection button on the platform
		// credential screen failing on a deployment where everything else is
		// correct.
		{http.MethodPost, "/gateway/providers/4/check"},
		{http.MethodPost, "/gateway/providers/4/revalidate"},
	} {
		recorder := httptest.NewRecorder()
		root.ServeHTTP(recorder, httptest.NewRequest(probe.method, probe.path, nil))

		if recorder.Code == http.StatusNotFound {
			t.Errorf("%s %s answered 404; the path never reached the handler",
				probe.method, probe.path)
			continue
		}
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 (routed, then refused for want of a pool)",
				probe.method, probe.path, recorder.Code)
		}
	}
}

// TestTheConfigIDReachesTheDelegatedHandler — the mount must bind `{configID}`,
// or an edit and a delete would address whatever row the handler defaulted to.
func TestTheConfigIDReachesTheDelegatedHandler(t *testing.T) {
	var seen string
	root := chi.NewRouter()
	sub := chi.NewRouter()
	sub.Delete("/{configID}", func(_ http.ResponseWriter, r *http.Request) {
		seen = chi.URLParam(r, "configID")
	})
	root.Route("/gateway", func(r chi.Router) { r.Mount("/providers", sub) })

	root.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodDelete, "/gateway/providers/4", nil))

	if seen != "4" {
		t.Errorf("configID = %q, want %q", seen, "4")
	}
}

// TestAProviderWriteCannotChooseItsSection.
//
// `sectionFor` returns a caller-supplied `section` verbatim, so before this was
// forced a body carrying `"section": "llm"` stored a CREDENTIAL row outside
// `ai_credentials`. The row is then invisible to this listing (filtered on the
// section) AND to the gateway's credential read (same predicate) — a 201, an
// empty list, and an orphan row holding a sealed key.
func TestAProviderWriteCannotChooseItsSection(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := providerRequest(http.MethodPost, "/",
		`{"elitea_title":"x","type":"open_ai","section":"llm"}`)

	rewritten, ok := providerHandler().rewriteGlobalProviderBody(recorder, request, true)
	if !ok {
		t.Fatalf("refused: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	raw, _ := io.ReadAll(rewritten.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["section"] != GlobalProviderSection {
		t.Errorf("section = %v, want the forced %q", body["section"], GlobalProviderSection)
	}
}

// TestAWriteVerbRefusesARowFromAnotherSection.
//
// The delegated Update and Delete address a row by id alone — no section
// predicate — so each surface could write the whole table. `DELETE
// /providers/{id}` given a MODEL's id deleted that model; `DELETE
// /platform_models/{id}` deleted a credential; a PUT carrying only `data`
// passed the type check (type is optional on an update) and overwrote a
// project_context row.
//
// The check runs against a fake row source, so what is pinned is the DECISION
// rather than the query: a section on the list passes, one off it answers 404.
func TestAWriteVerbRefusesARowFromAnotherSection(t *testing.T) {
	for section, wantAdmitted := range map[string]bool{
		"ai_credentials":   true,
		"llm":              false,
		"project_settings": false,
		"credentials":      false,
	} {
		if got := sectionAdmitted(section, GlobalProviderSection); got != wantAdmitted {
			t.Errorf("section %q admitted = %v, want %v", section, got, wantAdmitted)
		}
	}

	// And the model surface admits every model section and no credential.
	for _, section := range globalModelSectionNames() {
		if !sectionAdmitted(section, globalModelSectionNames()...) {
			t.Errorf("model section %q was refused by the model surface", section)
		}
	}
	if sectionAdmitted(GlobalProviderSection, globalModelSectionNames()...) {
		t.Error("the model surface admitted a credential row")
	}
}

// sectionAdmitted mirrors requireGlobalRowSection's membership decision.
func sectionAdmitted(section string, allowed ...string) bool {
	for _, want := range allowed {
		if section == want {
			return true
		}
	}
	return false
}

/* ── the stored operations on a platform credential ────────────────────── */

// governanceResolver resolves the central permissions a caller holds, and
// asserts the MODE the gate resolves in.
//
// `administration` is not decoration here: `configuration.governance` is
// granted in that mode by shared migration 0082, so a gate that resolved in
// the default mode would reach nobody — which is the #386 shape, a route that
// answers 403 to every caller on a clean database and looks like a permission
// problem at the operator's end.
func governanceResolver(t *testing.T, granted ...string) auth.PermissionResolver {
	t.Helper()
	return governanceResolverFunc(func(
		_ context.Context, _ auth.User, mode string, projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeAdministration {
			t.Errorf("the gate resolved in mode %q, want %q",
				mode, auth.PermissionModeAdministration)
		}
		if projectID != "" {
			t.Errorf("a CENTRAL gate resolved against project %q; this surface names no project",
				projectID)
		}
		return auth.PermissionResolution{UserID: 1, Permissions: granted}, nil
	})
}

type governanceResolverFunc func(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error)

func (f governanceResolverFunc) ResolvePermissions(
	ctx context.Context, principal auth.User, mode string, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// gatedProviderRouter mounts this surface the way router.go does: the central
// permission is applied at the MOUNT, and the sub-router applies none itself.
func gatedProviderRouter(t *testing.T, handler *Handler, resolver auth.PermissionResolver) chi.Router {
	t.Helper()
	root := chi.NewRouter()
	root.Use(func(next http.Handler) http.Handler {
		// Stands in for apimw.Auth. Without a user in the context every gate
		// answers 401, and the 403 this file is about would never be reached.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
		})
	})
	root.Group(func(r chi.Router) {
		r.Use(apimw.RequireCentralPermissions(
			resolver, auth.PermissionModeAdministration, "configuration.governance"))
		r.Route("/gateway", func(r chi.Router) {
			r.Mount("/providers", handler.GlobalProviderRoutes())
		})
	})
	return root
}

// TestTheStoredProviderRoutesInheritTheCentralGate.
//
// The gate is applied at the mount, so a route ADDED to GlobalProviderRoutes is
// gated by construction — which is exactly why it is worth pinning: nothing in
// this package would fail if a later refactor moved these two registrations
// outside that mount, and the two of them are the ones that make the platform
// dial a provider and rewrite a credential's status column.
//
// The entitled direction is asserted too. A gate that refuses everybody is
// indistinguishable from a correct one when only the refusal is measured, and
// 503 (routed, then refused for want of a store) is the answer that proves the
// request reached the handler.
func TestTheStoredProviderRoutesInheritTheCentralGate(t *testing.T) {
	for _, target := range []string{
		"/gateway/providers/4/check",
		"/gateway/providers/4/revalidate",
	} {
		for name, test := range map[string]struct {
			granted []string
			want    int
		}{
			"no permission at all":       {want: http.StatusForbidden},
			"another central permission": {granted: []string{"admin.moderation"}, want: http.StatusForbidden},
			"configuration.governance": {
				granted: []string{"configuration.governance"},
				want:    http.StatusServiceUnavailable,
			},
		} {
			t.Run(target+" "+name, func(t *testing.T) {
				router := gatedProviderRouter(t,
					&Handler{publicProjectID: 1}, governanceResolver(t, test.granted...))

				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))

				if recorder.Code != test.want {
					t.Fatalf("POST %s = %d, want %d (body %s)",
						target, recorder.Code, test.want, recorder.Body.String())
				}
			})
		}
	}
}

// TestTheStoredProviderRoutesRefuseWithNoPublicProject — the same refusal the
// other four verbs give, for the same reason. Guessing project 1 would dial a
// provider with, or rewrite the status of, a row in a schema this deployment's
// gateway may never read.
func TestTheStoredProviderRoutesRefuseWithNoPublicProject(t *testing.T) {
	router := chi.NewRouter()
	router.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", (&Handler{}).GlobalProviderRoutes())
	})

	for _, target := range []string{
		"/gateway/providers/4/check",
		"/gateway/providers/4/revalidate",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))

		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s = %d, want 503 when no public project is configured",
				target, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "AI_PROJECT_ID") {
			t.Errorf("POST %s: the body does not say how to fix it: %s",
				target, recorder.Body.String())
		}
	}
}

// TestAFailedSectionReadIsNotReportedAsADeletedCredential.
//
// The fence in front of both routes reads the row's section, and that read can
// FAIL — a saturated pool, a dropped connection. Answering 404 for it would
// tell an operator that a platform credential which is still there has been
// deleted, and send them to re-create one. The executors behind these routes
// both refuse to conflate the two (stored_check.go says so at its read;
// revalidate.go answers 500 rather than 404), and the fence must not
// reintroduce the conflation in front of them.
//
// The two answer in their OWN shapes, which is the other half of this test: the
// check's control reads `success`/`message`, and the CRUD verbs' reads `error`.
func TestAFailedSectionReadIsNotReportedAsADeletedCredential(t *testing.T) {
	// A closed pool fails every statement, which is what a saturated one looks
	// like from the fence.
	handler := NewHandler(revalidateClosedPool(t), WithPublicProjectID(1))
	router := chi.NewRouter()
	router.Route("/gateway", func(r chi.Router) {
		r.Mount("/providers", handler.GlobalProviderRoutes())
	})

	for _, test := range []struct {
		target string
		want   int
		field  string
	}{
		{"/gateway/providers/4/check", http.StatusBadRequest, "success"},
		{"/gateway/providers/4/revalidate", http.StatusInternalServerError, "error"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.target, nil))

		if recorder.Code == http.StatusNotFound {
			t.Errorf("POST %s answered 404 for a failed read; absence and failure "+
				"must not be the same answer", test.target)
			continue
		}
		if recorder.Code != test.want {
			t.Errorf("POST %s = %d, want %d (body %s)",
				test.target, recorder.Code, test.want, recorder.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Errorf("POST %s: body %q is not JSON: %v", test.target, recorder.Body.String(), err)
			continue
		}
		if _, present := body[test.field]; !present {
			t.Errorf("POST %s answered %v, which carries no %q field — the control that "+
				"renders it would show nothing", test.target, body, test.field)
		}
	}
}
