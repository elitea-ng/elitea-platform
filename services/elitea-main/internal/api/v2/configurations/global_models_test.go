package configurations

// The platform model surface's guards.
//
// Two of these pin things the GATEWAY is deliberately lenient about, and that
// leniency is why the check has to live here:
//
//   - a model row whose (section, type) pair does not match is invisible to
//     every dispatch path while looking complete in the admin list;
//   - a model whose credential link does not resolve is still ADVERTISED, with
//     its provider guessed from a prefix in the model name, and says so only in
//     a log line on whichever pod loaded it.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rewriteModel runs one body through the model rewrite and returns what would
// have been sent onward.
func rewriteModel(t *testing.T, handler *Handler, method, body string, requireType bool) (
	map[string]any, *httptest.ResponseRecorder, bool,
) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := providerRequest(method, "/", body)
	rewritten, ok := handler.rewriteGlobalModelBody(recorder, request, requireType)
	if !ok {
		return nil, recorder, false
	}
	raw, err := io.ReadAll(rewritten.Body)
	if err != nil {
		t.Fatalf("read rewritten body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	return decoded, recorder, true
}

// TestTheSectionIsDerivedFromTheType.
//
// The gateway matches a model row on the (section, type) PAIR. A row whose
// section does not match its type is invisible to every dispatch path while
// looking complete in the admin list — so the section is derived, and a caller
// that sent a wrong one has it overwritten rather than honoured.
func TestTheSectionIsDerivedFromTheType(t *testing.T) {
	handler := &Handler{publicProjectID: 1}

	for modelType, wantSection := range map[string]string{
		"llm_model":              "llm",
		"embedding_model":        "embedding",
		"image_generation_model": "image_generation",
		"asr_model":              "asr",
		"tts_model":              "tts",
	} {
		body, recorder, ok := rewriteModel(t, handler, http.MethodPost,
			`{"elitea_title":"x","type":"`+modelType+`"}`, true)
		if !ok {
			t.Errorf("type %q was refused: %s", modelType, recorder.Body.String())
			continue
		}
		if body["section"] != wantSection {
			t.Errorf("type %q derived section %v, want %q", modelType, body["section"], wantSection)
		}
	}

	// A section the caller supplied is overwritten, not honoured.
	body, recorder, ok := rewriteModel(t, handler, http.MethodPost,
		`{"elitea_title":"x","type":"llm_model","section":"embedding"}`, true)
	if !ok {
		t.Fatalf("refused: %s", recorder.Body.String())
	}
	if body["section"] != "llm" {
		t.Errorf("section = %v, want the derived 'llm' rather than the caller's", body["section"])
	}
}

// TestOnlyADispatchableModelTypeIsAdmitted — the same argument the provider
// surface's type check makes: without it this route writes any row into a
// schema every tenant reads.
func TestOnlyADispatchableModelTypeIsAdmitted(t *testing.T) {
	handler := &Handler{publicProjectID: 1}
	for _, modelType := range []string{"open_ai", "github", "realtime_model", "project_context", ""} {
		body := `{"elitea_title":"x","type":"` + modelType + `"}`
		if modelType == "" {
			body = `{"elitea_title":"x"}`
		}
		if _, recorder, ok := rewriteModel(t, handler, http.MethodPost, body, true); ok {
			t.Errorf("type %q was admitted as a platform model", modelType)
		} else if recorder.Code != http.StatusBadRequest {
			t.Errorf("type %q: status = %d, want 400", modelType, recorder.Code)
		}
	}
}

// TestNoRealtimeModelTypeExists is a deliberate absence, restated here because
// it looks like an omission. /llm/v1/realtime dispatches an `asr` model; there
// is no `realtime` section for a row to live in, so admitting the type would
// publish a model no dispatch path could ever find.
func TestNoRealtimeModelTypeExists(t *testing.T) {
	if _, present := globalModelSections["realtime_model"]; present {
		t.Error("a realtime model type is admitted; no configuration section holds one")
	}
}

// TestAPlatformModelIsAlwaysShared.
func TestAPlatformModelIsAlwaysShared(t *testing.T) {
	handler := &Handler{publicProjectID: 1}
	body, recorder, ok := rewriteModel(t, handler, http.MethodPost,
		`{"elitea_title":"x","type":"llm_model"}`, true)
	if !ok {
		t.Fatalf("refused: %s", recorder.Body.String())
	}
	if body["shared"] != true {
		t.Errorf("shared = %v, want true", body["shared"])
	}

	if _, recorder, ok := rewriteModel(t, handler, http.MethodPost,
		`{"elitea_title":"x","type":"llm_model","shared":false}`, true); ok {
		t.Error("shared:false was silently overridden rather than refused")
	} else if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

// TestAnUpdateMayOmitTheModelType — the delegated Update is partial, so
// requiring a type would make renaming a model impossible without restating it.
// The section is then left alone too: deriving one from an absent type would
// write the wrong pair.
func TestAnUpdateMayOmitTheModelType(t *testing.T) {
	handler := &Handler{publicProjectID: 1}
	body, recorder, ok := rewriteModel(t, handler, http.MethodPut,
		`{"elitea_title":"renamed"}`, false)
	if !ok {
		t.Fatalf("a partial update was refused: %s", recorder.Body.String())
	}
	if _, present := body["section"]; present {
		t.Errorf("section = %v was written for an update that named no type", body["section"])
	}
}

// TestACredentialLinkIsReadInBothSpellings — `alita_title` is the pre-debranding
// name of the same field, and the gateway's own reader accepts both. A database
// that has not run the rename still holds the old one, and reading only the new
// name would report every such model as naming no credential.
func TestACredentialLinkIsReadInBothSpellings(t *testing.T) {
	for _, field := range []string{"elitea_title", "alita_title"} {
		data := map[string]any{"ai_credentials": map[string]any{field: "platform-openai"}}
		if got := credentialTitleOf(data); got != "platform-openai" {
			t.Errorf("%s: credentialTitleOf = %q, want the linked title", field, got)
		}
	}

	// An empty link object names nothing — the gateway's `names()` agrees.
	if got := credentialTitleOf(map[string]any{"ai_credentials": map[string]any{}}); got != "" {
		t.Errorf("an empty link resolved to %q", got)
	}
	if got := credentialTitleOf(nil); got != "" {
		t.Errorf("a nil data object resolved to %q", got)
	}
}

// TestAModelNamingNoCredentialIsReportedUnresolved.
//
// It used to report as RESOLVING, on the argument that the gateway falls back
// to reading the provider out of a prefix in the model name. That argument
// described a row nothing here can write: `ai_credentials` is required on all
// five model types, so provider admission refuses such a row and stores
// `status_ok = false`, and every reader — the catalogue, the tier defaults and
// the gateway itself — selects on `status_ok = true`. The row is listed by this
// surface and served by nobody, which is exactly the state this flag exists to
// show.
//
// The flag is reported without consulting the credential list, because the
// absence of a link is a fact about the row and needs no list to establish.
func TestAModelNamingNoCredentialIsReportedUnresolved(t *testing.T) {
	item, err := scanGlobalModel(modelRowScan(`{"name":"gpt-4o"}`), []string{"platform-openai"}, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if item.CredentialResolves {
		t.Error("a model naming no credential at all was reported as resolving")
	}
	if item.ModelName != "gpt-4o" {
		t.Errorf("model_name = %q, want the wire name", item.ModelName)
	}

	unverified, err := scanGlobalModel(modelRowScan(`{"name":"gpt-4o"}`), nil, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if unverified.CredentialResolves {
		t.Error("an unread credential list turned a row with NO link into a resolving one")
	}
}

// TestTheTierFlagsAreReportedForTheEditForm.
//
// `low_tier` and `high_tier` decide whether a project's AI configuration offers
// the model as a tier default. The edit dialog rewrites `data` whole, so a form
// that could not read them back would clear the flag of every model it saved —
// the same shape as the credential link this file's other tests pin.
func TestTheTierFlagsAreReportedForTheEditForm(t *testing.T) {
	row := `{"name":"gpt-4o","ai_credentials":{"elitea_title":"platform-openai"},` +
		`"high_tier":true,"low_tier":false}`
	item, err := scanGlobalModel(modelRowScan(row), []string{"platform-openai"}, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !item.HighTier || item.LowTier {
		t.Errorf("high_tier = %v, low_tier = %v, want the flags the row carries", item.HighTier, item.LowTier)
	}

	// A row written before the flags existed carries neither key, and the
	// registry defaults both to false.
	plain, err := scanGlobalModel(
		modelRowScan(`{"name":"gpt-4o","ai_credentials":{"elitea_title":"platform-openai"}}`),
		[]string{"platform-openai"}, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if plain.HighTier || plain.LowTier {
		t.Errorf("a row carrying no flags reported high=%v low=%v", plain.HighTier, plain.LowTier)
	}
}

// TestAModelNamingAMissingCredentialIsReportedUnresolved.
//
// The gateway ADVERTISES such a model and guesses its provider from the model
// name, saying so only in a log line. This flag is the only place the state is
// visible to an operator.
func TestAModelNamingAMissingCredentialIsReportedUnresolved(t *testing.T) {
	row := `{"name":"gpt-4o","ai_credentials":{"elitea_title":"deleted-openai"}}`
	item, err := scanGlobalModel(modelRowScan(row), []string{"platform-openai"}, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if item.CredentialResolves {
		t.Error("a model naming a credential the platform does not publish was reported as resolving")
	}
	if item.CredentialName != "deleted-openai" {
		t.Errorf("credential_name = %q, want the name that failed", item.CredentialName)
	}
}

// TestACorruptModelDataColumnDoesNotBreakTheListing — one bad row must not make
// the platform's model screen unreachable.
func TestACorruptModelDataColumnDoesNotBreakTheListing(t *testing.T) {
	item, err := scanGlobalModel(modelRowScan(`not json`), nil, true)
	if err != nil {
		t.Fatalf("scan refused a corrupt data column: %v", err)
	}
	if item.ModelName != "" || item.CredentialName != "" {
		t.Errorf("item = %+v, want an empty report rather than invented fields", item)
	}
}

// TestTheModelPairsMatchThePinnedCatalogue.
//
// globalModelSections is a COPY of the gateway's addressableModelSections; the
// two services share no importable package. The pinned registry snapshot is the
// closest shared artefact, and it declares the same pairs, so a drift in either
// direction fails here rather than as a 404 `model_not_found` for a model this
// surface published.
func TestTheModelPairsMatchThePinnedCatalogue(t *testing.T) {
	handler := NewHandler(nil)
	if handler.catalog == nil {
		t.Fatal("the pinned catalogue did not load")
	}

	for modelType, wantSection := range globalModelSections {
		entry, ok := handler.catalog.EntryByType(modelType)
		if !ok {
			t.Errorf("model type %q is admitted here and absent from the catalogue, so its rows "+
				"would be stored with an empty section", modelType)
			continue
		}
		if entry.Section != wantSection {
			t.Errorf("model type %q: catalogue section %q, this file says %q",
				modelType, entry.Section, wantSection)
		}
	}

	// And the other direction: a model section the catalogue describes and this
	// file omits is one an operator cannot publish.
	for _, section := range []string{"llm", "embedding", "image_generation", "asr", "tts"} {
		found := false
		for _, got := range globalModelSections {
			if got == section {
				found = true
			}
		}
		if !found {
			t.Errorf("catalogue section %q has no admitted model type here", section)
		}
	}
}

// modelRowScan returns a scan func standing in for one model row.
func modelRowScan(data string) func(...any) error {
	return modelRowScanUpdated(data, true)
}

// modelRowScanUpdated is the same, with the row's `updated_at` under the
// caller's control. `updated: false` is a freshly created model — Create
// writes `created_at` only, so its `updated_at` is NULL.
//
// The timestamps go through `scanFakeTimestamp` (global_providers_test.go),
// which reproduces pgx's refusal to put a NULL into a `*time.Time`. The
// destination type is what is being pinned.
func modelRowScanUpdated(data string, updated bool) func(...any) error {
	return func(targets ...any) error {
		if len(targets) != 10 {
			return errors.New("column count mismatch")
		}
		*(targets[0].(*int)) = 1
		*(targets[1].(*string)) = "uuid-1"
		*(targets[2].(*string)) = "gpt-4o"
		*(targets[3].(*string)) = "llm_model"
		*(targets[4].(*string)) = "llm"
		*(targets[5].(*[]byte)) = []byte(data)
		*(targets[6].(*bool)) = true
		*(targets[7].(*string)) = ""
		created := fakeRowTimestamp
		if err := scanFakeTimestamp(8, "created_at", targets[8], &created); err != nil {
			return err
		}
		var updatedAt *time.Time
		if updated {
			updatedAt = &created
		}
		return scanFakeTimestamp(9, "updated_at", targets[9], updatedAt)
	}
}

// TestANeverUpdatedModelDoesNotBreakTheWholeListing — the model surface's half
// of the F1 regression pinned in global_providers_test.go.
//
// The same shape, the same consequence: `updated_at` is NULL on a freshly
// created model, this scanner asked pgx for a `time.Time`, and the resulting
// `cannot scan NULL into *time.Time` is raised per row — so publishing ONE
// platform model made GET /admin/gateway/platform_models answer 500 for every
// row and every operator. Both scanners are asserted because fixing one and
// reading the other as "the same code, so it must be fine" is how the second
// half of a paired defect survives.
func TestANeverUpdatedModelDoesNotBreakTheWholeListing(t *testing.T) {
	item, err := scanGlobalModel(
		modelRowScanUpdated(`{"name":"gpt-4o"}`, false), []string{"platform-openai"}, true)
	if err != nil {
		t.Fatalf("a freshly created model was refused by the listing: %v", err)
	}
	if item.Name != "gpt-4o" {
		t.Errorf("item = %+v, want the row reported rather than skipped", item)
	}
	if item.CreatedAt != fakeRowTimestamp.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want the row's creation time", item.CreatedAt)
	}
	if item.UpdatedAt != "" {
		t.Errorf("updated_at = %q, want empty for a row that was never updated", item.UpdatedAt)
	}
}

// TestTheRefusalNamesTheAdmittedModelTypes — the operator who hits it is
// looking at a type this platform does not dispatch, and a bare "invalid" would
// not say which ones it does.
func TestTheRefusalNamesTheAdmittedModelTypes(t *testing.T) {
	handler := &Handler{publicProjectID: 1}
	_, recorder, ok := rewriteModel(t, handler, http.MethodPost,
		`{"elitea_title":"x","type":"realtime_model"}`, true)
	if ok {
		t.Fatal("realtime_model was admitted")
	}
	if !strings.Contains(recorder.Body.String(), "llm_model") {
		t.Errorf("the refusal does not name what IS admitted: %s", recorder.Body.String())
	}
}

// TestAnUnverifiedCredentialListDoesNotMarkEveryModelBroken.
//
// When the credential read fails the list is nil, so `containsString` is false
// for every linked model at once. Reported as-is, the panel raises "these models
// name a provider this platform does not publish" naming EVERY model on the
// platform — a failure of the check rendered as a finding about the data, which
// is the conflation the per-section errors exist to prevent. The listing already
// carries `credential_error` to say what actually happened.
func TestAnUnverifiedCredentialListDoesNotMarkEveryModelBroken(t *testing.T) {
	row := `{"name":"gpt-4o","ai_credentials":{"elitea_title":"platform-openai"}}`

	unverified, err := scanGlobalModel(modelRowScan(row), nil, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !unverified.CredentialResolves {
		t.Error("a model was reported as unresolved because the credential list could not be read")
	}

	// The check still bites when the list WAS read.
	verified, err := scanGlobalModel(modelRowScan(row), nil, true)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if verified.CredentialResolves {
		t.Error("a model naming a credential absent from a READ list was reported as resolving")
	}
}
