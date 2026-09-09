package configurations

// Global LLM models — `/api/v2/admin/gateway/platform_models`.
//
// ## What a platform model is
//
// The other half of what `global_providers.go` publishes. A platform CREDENTIAL
// lets a project authenticate to a provider; it dispatches nothing on its own.
// What a caller addresses is a MODEL: a row in the public project's schema, in
// one of the five sections the gateway can dispatch (`llm`, `embedding`,
// `image_generation`, `asr`, `tts`), with `shared = true` and a link naming the
// credential it uses.
//
// The gateway has read that scope since issue #316, in `ModelResolver.List`:
// the caller's own rows, then the public project's shared rows. The precedence
// there is worth restating because it decides what this surface may safely
// author — a PUBLIC model row is resolved against PUBLIC credentials only, and
// deliberately: "a published model must not resolve differently for each
// caller". So a platform model may only name a platform credential, and this
// file enforces exactly that.
//
// ## Why a broken link is refused HERE rather than left to the gateway
//
// The gateway does not fail a model whose credential link does not resolve. It
// logs a warning and falls back to reading the provider out of a PREFIX in the
// model name (`applyCredentialLink`), which is the pre-#451 behaviour and is
// kept so a row written straight into the database by a seed keeps loading.
//
// That fallback is right for the gateway and wrong as the only check. A
// platform model naming a credential that does not exist is advertised to every
// project on the deployment, resolves its provider by guessing at a string, and
// says so only in a log line on whichever pod happened to load it. The operator
// authoring it sees a success. So the link is validated at WRITE time, against
// the public project's shared credentials, and a name that does not resolve is
// refused with the names that would have.
//
// ## A model with NO link is refused as well
//
// It reads as the lenient case and is the worse one. The registry declares
// `ai_credentials` REQUIRED on all five model types, so provider admission
// refuses such a row and stores `status_ok = false` — and every reader, the
// gateway included, selects on `status_ok = true`. The row is therefore
// written, listed by this surface alone, and served to nobody, which is the
// state an operator has no way to read back from the screen that wrote it.
// This surface refuses the body instead, on the create and on the update that
// would clear the link.
//
// ## Everything else is the provider surface's argument, unchanged
//
// Same delegation to this package's own Create/Update/Delete — so the same
// author resolution, the same partial-update semantics, and the same provider
// admission that decides `status_ok`, which the gateway requires on a model row
// exactly as it does on a credential (`modelsSQL`: `WHERE c.status_ok = true`).
// Same pinning to the public project, same forced `shared`, same restriction of
// `type` to a set this platform can place — see global_providers.go for why
// each of those is load-bearing.
//
// The one addition is that `section` is DERIVED rather than accepted. A model
// row is matched by the gateway on the (section, type) PAIR, so a row whose
// section does not match its type is invisible to every dispatch path while
// looking complete in the admin list.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"

	"github.com/jackc/pgx/v5"
)

// globalModelSections are the (type → section) pairs the gateway dispatches.
//
// It MIRRORS `addressableModelSections` in
// services/elitea-llm-gateway/internal/llmproxy/models.go. A pair missing there
// makes the gateway answer 404 `model_not_found` for a model this surface
// published; a pair missing here makes the model unpublishable. The gateway's
// list is the authority and this is a copy, because the two services share no
// importable package — TestGlobalModelPairsMatchTheCatalogue checks this copy
// against the pinned registry snapshot, which is the closest shared artefact
// there is.
var globalModelSections = map[string]string{
	"llm_model":              "llm",
	"embedding_model":        "embedding",
	"image_generation_model": "image_generation",
	"asr_model":              "asr",
	"tts_model":              "tts",
}

// globalModelTypes is the same set, ordered, for the listing and the refusals.
func globalModelTypes() []string {
	types := make([]string, 0, len(globalModelSections))
	for modelType := range globalModelSections {
		types = append(types, modelType)
	}
	sort.Strings(types)
	return types
}

// GlobalModel is one platform model as the admin listing reports it.
type GlobalModel struct {
	ID   int    `json:"id"`
	UUID string `json:"uuid"`
	// Name is `elitea_title` — the id a caller addresses the model by. The
	// gateway exposes this when it is set and falls back to the wire name.
	Name string `json:"elitea_title"`
	Type string `json:"type"`
	// Section is reported although it is derived, so an operator auditing a
	// deployment can see the pair the gateway matches on.
	Section string `json:"section"`
	// StatusOK is what the gateway requires. A model with false here is stored,
	// listed and never dispatched.
	StatusOK   bool   `json:"status_ok"`
	StatusLogs string `json:"status_logs"`
	// ModelName is `data.name`, the provider's own model string.
	ModelName string `json:"model_name"`
	// CredentialName is the platform credential this model uses, by title.
	// Empty means the row names none, which this surface no longer writes.
	CredentialName string `json:"credential_name"`
	// CredentialResolves is false when the row names no platform credential at
	// all, and when the one it names is not among the platform's shared
	// credentials. Neither row is dispatched, and this is the only place either
	// state is visible to an operator.
	CredentialResolves bool `json:"credential_resolves"`
	// LowTier and HighTier are the two `data` flags a project's AI
	// configuration filters its tier defaults on. They are reported because the
	// edit dialog rewrites `data` whole: a form that could not read them back
	// would clear the flag of every model it saved.
	LowTier  bool `json:"low_tier"`
	HighTier bool `json:"high_tier"`
	// ShareScope and SharedWith are the row's GRANT: which projects this
	// platform model is offered to. `shared = true` still marks the row as a
	// platform row — every reader keys on it — and the scope narrows it to
	// every project (`all`), to none (`none`), or to the ids in SharedWith
	// (`projects`).
	//
	// A row written before the grant existed carries neither field and was
	// offered to every project, so it is REPORTED as `all` rather than as an
	// empty string: the panel renders this value, and a blank there would read
	// as "this model is granted to nobody" for every model on the deployment.
	ShareScope string `json:"share_scope"`
	// SharedWith is empty for every scope but `projects`, and it is a list
	// rather than an omitted field so the edit dialog can clear it.
	SharedWith []int `json:"shared_with"`
	// Data is the row's stored `data` object, ENTIRE.
	//
	// The fields above name what this listing interprets. They are not what the
	// row holds. An `llm_model` also declares `context_window`,
	// `max_output_tokens`, `openai_compatible`, `supports_reasoning` and
	// `supports_vision`, and the update replaces the `data` column whole — so an
	// edit dialog that could only read back the interpreted fields rebuilt
	// `data` from them and DROPPED every other one. Renaming a model, or ticking
	// a tier, silently reset its context window and its capabilities.
	//
	// Naming each new field on this struct would fix the five that exist today
	// and lose the next one the registry adds, because the failure is the
	// listing being a projection at all. So the object is reported as stored and
	// the dialog merges its own fields over it.
	//
	// Disclosure: this query reads the five MODEL sections and cannot reach the
	// `ai_credentials` section, so no provider row is in scope; no model type in
	// the pinned registry snapshot declares a secret field; and a model's link
	// to its provider is a TITLE, with the secret held in the provider row.
	// There is nothing here the interpreted fields were protecting.
	Data      map[string]any `json:"data"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// listGlobalModelsSQL reads the public project's shared model rows.
//
// The section/type pairs are bound as parallel arrays rather than concatenated,
// so nothing about which rows are read is built from a string.
const listGlobalModelsSQL = `
	SELECT c.id, COALESCE(c.uuid::text, ''), COALESCE(c.elitea_title, ''), c.type, c.section,
	       c.data, c.status_ok, COALESCE(c.status_logs, ''), c.created_at, c.updated_at
	  FROM %s.configuration AS c
	  JOIN unnest($1::text[], $2::text[]) AS s(section, type)
	    ON c.section = s.section AND c.type = s.type
	 WHERE c.shared = true
	 ORDER BY c.section, c.elitea_title, c.id`

// GlobalModelRoutes is the admin surface for platform-wide models. Mount it
// behind the central gate; it applies none itself.
func (h *Handler) GlobalModelRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.ListGlobalModels)
	r.Post("/", h.CreateGlobalModel)
	r.Put("/{configID}", h.UpdateGlobalModel)
	r.Delete("/{configID}", h.DeleteGlobalModel)
	return r
}

// globalModelSectionArgs returns the two parallel arrays listGlobalModelsSQL
// binds, in a stable order.
func globalModelSectionArgs() (sections, types []string) {
	types = globalModelTypes()
	sections = make([]string, len(types))
	for index, modelType := range types {
		sections[index] = globalModelSections[modelType]
	}
	return sections, types
}

// ListGlobalModels serves GET /admin/gateway/platform_models.
func (h *Handler) ListGlobalModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.pinPublicProject(w, r); !ok {
		return
	}
	ctx := r.Context()
	schema := pgx.Identifier{fmt.Sprintf("p_%d", h.publicProjectID)}.Sanitize()

	// The credential names are read FIRST, so every row can be reported with
	// whether its link resolves. Reading them per row would be one query per
	// model for a fact that is the same for all of them.
	credentials, credErr := h.globalCredentialTitles(ctx, schema)

	sections, types := globalModelSectionArgs()
	rows, err := h.pool.Query(ctx, fmt.Sprintf(listGlobalModelsSQL, schema), sections, types)
	if err != nil {
		writeConfigurationQueryFailure(ctx, w, "list global models failed", schema, err)
		return
	}
	defer rows.Close()

	items := make([]GlobalModel, 0)
	for rows.Next() {
		item, scanErr := scanGlobalModel(rows.Scan, credentials, credErr == nil)
		if scanErr != nil {
			writeConfigurationRowFailure(ctx, w, schema, scanErr)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		writeConfigurationQueryFailure(ctx, w, "list global models failed", schema, rows.Err())
		return
	}

	body := map[string]any{
		"items":             items,
		"total":             len(items),
		"public_project_id": h.publicProjectID,
		"model_types":       globalModelTypes(),
		// The credentials a model may name, so the form offers them instead of
		// asking an operator to retype a title exactly.
		"credential_names": credentials,
	}
	if credErr != nil {
		// Stated rather than swallowed. Without it an empty credential list
		// renders as "this platform has published no providers", which is the
		// reading that sends an operator to create a duplicate — and every
		// model's `credential_resolves` would read false for the same reason.
		body["credential_error"] = credErr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// globalCredentialTitles reads the titles of the platform's shared credentials.
//
// Titles, not ids: a model row names its credential by `elitea_title`
// (`modelCredentialRef.title()`), so the title is what has to match.
func (h *Handler) globalCredentialTitles(ctx context.Context, schema string) ([]string, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT COALESCE(elitea_title, '')
		  FROM %s.configuration
		 WHERE section = '`+GlobalProviderSection+`' AND shared = true
		 ORDER BY elitea_title`, schema))
	if err != nil {
		return nil, fmt.Errorf("read platform credentials: %w", err)
	}
	defer rows.Close()

	titles := make([]string, 0)
	for rows.Next() {
		var title string
		if scanErr := rows.Scan(&title); scanErr != nil {
			return nil, fmt.Errorf("read platform credentials: %w", scanErr)
		}
		if title != "" {
			titles = append(titles, title)
		}
	}
	return titles, rows.Err()
}

// scanGlobalModel turns one row into its report.
//
// `verified` is false when the credential list could not be read. Every link
// then reports as RESOLVING rather than as broken: an unread list makes
// `containsString` false for every model at once, and the panel would raise
// "these models name a provider this platform does not publish" naming every
// model on the platform — a failure of the check rendered as a finding about
// the data, which is the exact conflation the per-section errors exist to
// prevent. The listing already carries `credential_error` to say what happened.
func scanGlobalModel(
	scan func(...any) error, credentials []string, verified bool,
) (GlobalModel, error) {
	var item GlobalModel
	var data []byte
	// Pointers, for the reason scanGlobalProvider's header gives: `updated_at`
	// is nullable and a freshly created model leaves it NULL, so scanning it
	// as a `time.Time` made one new model answer 500 for the whole listing.
	var createdAt, updatedAt *time.Time
	if err := scan(&item.ID, &item.UUID, &item.Name, &item.Type, &item.Section,
		&data, &item.StatusOK, &item.StatusLogs, &createdAt, &updatedAt); err != nil {
		return GlobalModel{}, err
	}

	var decoded map[string]any
	// A row whose `data` will not decode is reported as a model with no wire
	// name and no link rather than failing the whole listing — one corrupt row
	// must not make the platform's model screen unreachable.
	_ = json.Unmarshal(data, &decoded)

	item.ModelName = globalProviderString(decoded, "name")
	item.CredentialName = credentialTitleOf(decoded)
	item.LowTier = globalModelFlag(decoded, "low_tier")
	item.HighTier = globalModelFlag(decoded, "high_tier")
	grant := configurationapp.ReadModelGrant(decoded)
	item.ShareScope = string(grant.Scope)
	item.SharedWith = make([]int, 0, len(grant.Projects))
	for _, projectID := range grant.Projects {
		item.SharedWith = append(item.SharedWith, int(projectID))
	}
	// An EMPTY object, never a null, when the column would not decode. The
	// dialog merges its edited fields over this one, and `null` there would make
	// the merge itself the thing that fails — a corrupt row would stop being
	// editable instead of being reported as a model with no settings.
	item.Data = decoded
	if item.Data == nil {
		item.Data = map[string]any{}
	}
	// A row that names NO credential is reported as unresolved, and the absence
	// of the link is the whole reason. `ai_credentials` is required on all five
	// model types, so admission refuses such a row and every reader selects it
	// out on `status_ok`. Reporting it as resolving described the gateway's
	// prefix fallback, which no row this surface can write ever reaches.
	//
	// `verified` guards only the LOOKUP. When the credential list could not be
	// read, a named link is given the benefit of the doubt — see this
	// function's header — but an absent link is a fact about the row itself and
	// needs no list to establish.
	item.CredentialResolves = item.CredentialName != "" &&
		(!verified || containsString(credentials, item.CredentialName))
	if createdAt != nil {
		item.CreatedAt = createdAt.Format(time.RFC3339)
	}
	if updatedAt != nil {
		item.UpdatedAt = updatedAt.Format(time.RFC3339)
	}
	return item, nil
}

// credentialTitleOf reads a model row's `data.ai_credentials` link title.
//
// Both spellings are accepted. `alita_title` is the pre-debranding name of the
// same field, renamed in place by a legacy admin task, so a database that has
// not run that task still holds the old one — the gateway's own
// `modelCredentialRef.title()` accepts both and this must agree with it.
func credentialTitleOf(data map[string]any) string {
	if data == nil {
		return ""
	}
	link, ok := data["ai_credentials"].(map[string]any)
	if !ok {
		return ""
	}
	if title := globalProviderString(link, "elitea_title"); title != "" {
		return title
	}
	return globalProviderString(link, "alita_title")
}

// globalModelFlag reads one boolean `data` flag, defaulting to false.
//
// A row written before the flag existed carries no key, and the registry
// declares both flags `default: false`, so an absent key and `false` are the
// same answer.
func globalModelFlag(data map[string]any, key string) bool {
	if data == nil {
		return false
	}
	value, _ := data[key].(bool)
	return value
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// CreateGlobalModel serves POST /admin/gateway/platform_models.
func (h *Handler) CreateGlobalModel(w http.ResponseWriter, r *http.Request) {
	request, ok := h.pinPublicProject(w, r)
	if !ok {
		return
	}
	rewritten, ok := h.rewriteGlobalModelBody(w, request, true)
	if !ok {
		return
	}
	h.Create(w, rewritten)
}

// UpdateGlobalModel serves PUT /admin/gateway/platform_models/{configID}.
func (h *Handler) UpdateGlobalModel(w http.ResponseWriter, r *http.Request) {
	request, ok := h.pinPublicProject(w, r)
	if !ok {
		return
	}
	if !h.requireGlobalRowSection(w, request, globalModelSectionNames()...) {
		return
	}
	rewritten, ok := h.rewriteGlobalModelBody(w, request, false)
	if !ok {
		return
	}
	h.Update(w, rewritten)
}

// DeleteGlobalModel serves DELETE /admin/gateway/platform_models/{configID}.
func (h *Handler) DeleteGlobalModel(w http.ResponseWriter, r *http.Request) {
	request, ok := h.pinPublicProject(w, r)
	if !ok {
		return
	}
	// Scoped to the MODEL sections — see requireGlobalRowSection. Without it
	// this route deleted any row in the public project by id, a credential
	// included.
	if !h.requireGlobalRowSection(w, request, globalModelSectionNames()...) {
		return
	}
	h.Delete(w, request)
}

// globalModelSectionNames are the sections a platform model can live in.
func globalModelSectionNames() []string {
	sections := make([]string, 0, len(globalModelSections))
	for _, section := range globalModelSections {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	return sections
}

// rewriteGlobalModelBody forces `shared`, derives `section` and validates the
// credential link.
//
// It shares the buffering, the size bound, the shared-flag rule and the label
// completion with the provider surface — see rewriteGlobalProviderBody for why
// each is done before the delegated handler reads a byte.
func (h *Handler) rewriteGlobalModelBody(
	w http.ResponseWriter, r *http.Request, creating bool,
) (*http.Request, bool) {
	body, ok := decodeGlobalBody(w, r)
	if !ok {
		return nil, false
	}
	if !admitGlobalShared(w, body) {
		return nil, false
	}

	modelType, ok := admitGlobalModelType(w, body, creating)
	if !ok {
		return nil, false
	}
	// SECTION IS DERIVED, never accepted. The gateway matches a model row on
	// the (section, type) pair, so a row whose section does not match its type
	// is invisible to every dispatch path while looking complete here. An
	// explicit section in the body is overwritten rather than refused: there is
	// exactly one correct value and the caller has no reason to send another.
	if modelType != "" {
		body["section"] = globalModelSections[modelType]
	}

	if !h.admitGlobalModelCredential(w, r, body) {
		return nil, false
	}
	if !admitGlobalModelGrant(w, body) {
		return nil, false
	}
	// The model dialog has no label field either — see completeGlobalRowLabel
	// in global_providers.go, which both surfaces share along with the body
	// bound and the shared-flag rule.
	if creating {
		completeGlobalRowLabel(body)
	}
	return encodeGlobalBody(w, r, body)
}

// admitGlobalModelType checks the model type and reports it.
func admitGlobalModelType(
	w http.ResponseWriter, body map[string]any, required bool,
) (string, bool) {
	raw, present := body["type"]
	if !present {
		if required {
			apierr.WriteStatus(w, http.StatusBadRequest, "a model type is required")
			return "", false
		}
		return "", true
	}
	modelType, isString := raw.(string)
	if !isString {
		apierr.WriteStatus(w, http.StatusBadRequest, "a model type is required")
		return "", false
	}
	if _, known := globalModelSections[modelType]; !known {
		apierr.WriteStatus(w, http.StatusBadRequest,
			"not a model type the gateway can dispatch: "+strings.Join(globalModelTypes(), ", "))
		return "", false
	}
	return modelType, true
}

// admitGlobalModelCredential refuses a `data` object that names no published
// platform credential — whether it names the wrong one or names none.
//
// The gateway would ACCEPT a wrong name — it logs a warning and resolves the
// provider from a prefix in the model name — so this is the only place that
// mistake is caught while the operator is still looking at it. See the file
// header for why the gateway's leniency is right there and insufficient here.
//
// A model naming NO credential is refused for a different reason: the registry
// declares `ai_credentials` required on all five model types, so such a row
// fails admission, stores `status_ok = false` and is served by nobody. The
// refusal here names the providers the operator can pick instead; the
// schema-driven check in Create would otherwise answer with the field path
// alone, and an update that dropped the link would be caught only by the
// generic model-data rule in required_fields.go.
//
// It keys on the PRESENCE of `data`, so a partial update that only renames a
// model touches no link and is not asked about one.
func (h *Handler) admitGlobalModelCredential(
	w http.ResponseWriter, r *http.Request, body map[string]any,
) bool {
	raw, present := body["data"]
	if !present {
		return true
	}
	data, _ := raw.(map[string]any)
	title := credentialTitleOf(data)
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return false
	}

	schema := pgx.Identifier{fmt.Sprintf("p_%d", h.publicProjectID)}.Sanitize()
	credentials, err := h.globalCredentialTitles(r.Context(), schema)
	if err != nil {
		// NOT permissive. Admitting the link because the check could not run
		// would publish, to every project, a model whose provider is guessed
		// from a string — which is the state this validation exists to prevent.
		apierr.WriteStatus(w, http.StatusServiceUnavailable,
			"the platform credentials could not be read, so this model's credential could not be verified")
		return false
	}
	if title == "" {
		apierr.WriteStatus(w, http.StatusBadRequest,
			"a platform model must name the platform provider it uses. A model with no "+
				"ai_credentials link is refused admission, so it would be listed here and served "+
				"to nobody. Published providers: "+strings.Join(credentials, ", "))
		return false
	}
	if !containsString(credentials, title) {
		apierr.WriteStatus(w, http.StatusBadRequest,
			"no platform provider is named "+title+". A platform model may only use a platform "+
				"provider, because a published model must resolve the same way for every project. "+
				"Published providers: "+strings.Join(credentials, ", "))
		return false
	}
	return true
}

// admitGlobalModelGrant refuses a grant this platform cannot act on.
//
// The READ side is deliberately lenient — an absent or malformed scope is read
// as "every project", because that is what every row written before the field
// existed meant and a stricter read would withdraw them all (see
// application/configurations/model_grant.go). That leniency is only safe while
// the WRITE side refuses what it cannot store, which is here: a scope this
// platform does not know, and a `projects` grant naming no project, would both
// be stored and then read back as "all" — the opposite of what the operator
// chose, reported to them as a success.
//
// Like the credential check it keys on the PRESENCE of `data`, so a partial
// update that touches neither field is not asked about them. `data` is
// replaced whole, though, so a body that carries the object and omits the
// scope is a row with no scope — which is "all", and is the same answer the
// row had before this feature. That is why an omitted scope is admitted rather
// than required.
func admitGlobalModelGrant(w http.ResponseWriter, body map[string]any) bool {
	raw, present := body["data"]
	if !present {
		return true
	}
	data, _ := raw.(map[string]any)
	if data == nil {
		return true
	}
	scopeValue, hasScope := data[configurationapp.ModelShareScopeField]
	if !hasScope {
		return true
	}
	scope, isString := scopeValue.(string)
	if !isString || !configurationapp.IsSupportedModelShareScope(
		configurationapp.ModelShareScope(scope)) {
		apierr.WriteStatus(w, http.StatusBadRequest,
			"a platform model is available to one of: "+strings.Join(globalModelShareScopes(), ", ")+
				". An unknown value is read back as \"all\", so it would grant the model to "+
				"every project while the screen said otherwise.")
		return false
	}
	if configurationapp.ModelShareScope(scope) != configurationapp.ModelShareScopeProjects {
		// The list is CLEARED, never left behind. A row that had been granted
		// to three projects and is now granted to none would otherwise keep
		// naming them, and the next edit that switched back to `projects` would
		// silently restore a grant nobody re-chose.
		data[configurationapp.ModelSharedWithField] = []any{}
		return true
	}
	granted := configurationapp.ReadModelGrant(data)
	if len(granted.Projects) == 0 {
		apierr.WriteStatus(w, http.StatusBadRequest,
			"a model available to selected projects must name at least one project id in "+
				configurationapp.ModelSharedWithField+
				". A list that names none grants the model to nobody, which is the \"none\" choice.")
		return false
	}
	// Rewritten as the ids that were READ, so what is stored is what the rule
	// above admitted. A body that spelled an id as a string, or repeated one,
	// is stored once and as a number — the shape every reader expects.
	normalized := make([]any, 0, len(granted.Projects))
	seen := make(map[int32]struct{}, len(granted.Projects))
	for _, projectID := range granted.Projects {
		if _, duplicate := seen[projectID]; duplicate {
			continue
		}
		seen[projectID] = struct{}{}
		normalized = append(normalized, projectID)
	}
	data[configurationapp.ModelSharedWithField] = normalized
	return true
}

// globalModelShareScopes are the grant scopes a platform model may carry, in a
// stable order for the refusal above.
func globalModelShareScopes() []string {
	return []string{
		string(configurationapp.ModelShareScopeAll),
		string(configurationapp.ModelShareScopeNone),
		string(configurationapp.ModelShareScopeProjects),
	}
}
