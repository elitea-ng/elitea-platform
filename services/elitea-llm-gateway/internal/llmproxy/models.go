package llmproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// DefaultModelsCacheTTL is the per-project models-list cache lifetime. The
// synthetic /llm/v1/models surface resolves the calling project's configured
// models from p_{projectID}.configuration at request time behind a short cache
// so the hot path never makes a management RPC (design §4.2: "local cache
// (TTL 60 s, Postgres fallback)"). It replaces the legacy _map_model_name
// 3-step prefix probe entirely.
const DefaultModelsCacheTTL = 60 * time.Second

// modelObjectType / modelsListType are the OpenAI /v1/models object markers.
// The synthesised response is byte-shape-compatible with the legacy LiteLLM
// /v1/models so no SDK changes are required (spec §3, design §3.4).
const (
	modelObjectType = "model"
	modelsListType  = "list"
	// modelsOwnedBy is the owner tag stamped on every synthesised model. The
	// gateway is the model owner from the caller's perspective (the real
	// provider is never leaked); a fixed value keeps the response deterministic.
	modelsOwnedBy = "elitea"
)

// modelObject is one entry in the OpenAI /v1/models response. Created is a
// fixed 0 rather than a wall-clock timestamp: the models set is synthesised
// from static per-project config, has no meaningful creation time, and a fixed
// value keeps the response deterministic (the parity gate BFF.3 compares the id
// set, order-insensitive).
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// providerModel is the row's data.name — the model name the PROVIDER
	// accepts. ID is elitea_title, a user-authored label, and the two are
	// independent by construction, so the inference path must translate ID to
	// providerModel before it dispatches (issue #317).
	//
	// The field is deliberately UNEXPORTED: encoding/json skips it, so the
	// provider's wire name can never leak into a caller-facing /llm/v1/models
	// response (the gateway is the model owner from the caller's view — see
	// modelsOwnedBy).
	providerModel string

	// credentialProvider is the bifrost provider the row's linked credential
	// serves, derived from that credential's configuration type (issue #451).
	// It is empty when the row links to no credential, or when the link names a
	// credential the caller's scopes do not hold, or when the credential type is
	// one this gateway cannot serve.
	//
	// It is the ONLY provider source for a bare model name. Measured on the
	// staging dump of 2026-07-09: 48 of 48 chat rows and 8 of 8 embedding rows
	// hold a bare data.name, so the model-string prefix names a provider for
	// none of them.
	credentialProvider schemas.ModelProvider
	// credentialProject and credentialID identify the linked credential row.
	// They travel to the account package on the request context so it returns
	// that one credential instead of every credential of the provider.
	credentialProject string
	credentialID      string
	// credentialTitle is the link's elitea_title. It identifies the credential
	// when credentialID is empty, and it names the row in a log line.
	credentialTitle string
	// credentialModelOwnerAccess records that this credential was resolved
	// through a model published from the operator's public project. It lets the
	// account use that model's owner credential without publishing the
	// credential as a generally reusable provider.
	credentialModelOwnerAccess bool
}

// linkedCredential returns the account-side selector for this model's linked
// credential, and whether the row links to one.
func (mo modelObject) linkedCredential() (account.LinkedCredential, bool) {
	if mo.credentialID == "" && mo.credentialTitle == "" {
		return account.LinkedCredential{}, false
	}
	return account.LinkedCredential{
		ProjectID:        mo.credentialProject,
		ConfigID:         mo.credentialID,
		Title:            mo.credentialTitle,
		ModelOwnerAccess: mo.credentialModelOwnerAccess,
	}, true
}

// modelsList is the OpenAI /v1/models list envelope.
type modelsList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// modelRowQuerier is the minimal pgx surface the resolver needs: a multi-row
// Query. It is satisfied by *pgxpool.Pool (via ModelPoolQuerier) and by test
// fakes, mirroring the account/cost package DB seams so the resolver is
// unit-testable without a live database.
type modelRowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (modelRows, error)
}

// modelRows mirrors the subset of pgx.Rows the resolver consumes.
type modelRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// modelConfigData is the subset of a model configuration row's JSONB `data` the
// resolver reads. name is the underlying model wire name; it backs the exposed
// id only when elitea_title is empty. ai_credentials is the row's link to the
// ONE credential the model uses (issue #451).
type modelConfigData struct {
	Name          string              `json:"name"`
	AICredentials *modelCredentialRef `json:"ai_credentials"`
}

// modelCredentialRef is a model row's link to a credential row. TWO shapes
// exist, and a correct reader accepts both:
//
//  1. The STORED shape, `{"elitea_title": "...", "private": true|false}`. This
//     is what the platform persists. Measured on the staging dump of
//     2026-07-09: all 58 model rows carry this shape and none carries any
//     other. `alita_title` is the pre-debranding spelling of the same field;
//     legacy/plugins/configurations/methods/admin_tasks.py renames it in place,
//     so a database that has not run that task still holds the old name.
//
//  2. The EXPANDED shape, which adds `configuration_type`,
//     `configuration_uuid` and `configuration_project_id`. The legacy platform
//     built it in memory (legacy/plugins/configurations/utils.py,
//     expand_configuration) and never wrote it back, and the deleted LiteLLM
//     mapper read that in-memory form. elitea-main writes the same three keys
//     when it freezes a configuration reference
//     (application/configurations/toolkit_settings.go), so a row CAN hold it.
//
// Shape 2 answers the provider question by itself. Shape 1 does not: it names
// the credential by title only, so the credential's own row must be read to
// learn its type. credentialRefs does that read.
type modelCredentialRef struct {
	EliteaTitle string `json:"elitea_title"`
	AlitaTitle  string `json:"alita_title"`
	// Private selects the caller's personal-project credential when true. When
	// false, the current platform resolves the credential in the model owner's
	// project. False is also the legacy default for an expanded shape that did
	// not persist this field.
	Private bool `json:"private"`
	// ConfigurationType is the credential row's `type` column, present only in
	// the expanded shape.
	ConfigurationType string `json:"configuration_type"`
	// ConfigurationUUID is the credential row's uuid, present only in the
	// expanded shape.
	ConfigurationUUID string `json:"configuration_uuid"`
	// ConfigurationProjectID is the credential owner's project id, present only
	// in the expanded shape. json.Number accepts it whether it was written as a
	// number or as a string.
	ConfigurationProjectID json.Number `json:"configuration_project_id"`
}

// title returns the credential's elitea_title, accepting the pre-debranding
// spelling.
func (r *modelCredentialRef) title() string {
	if r == nil {
		return ""
	}
	if r.EliteaTitle != "" {
		return r.EliteaTitle
	}
	return r.AlitaTitle
}

// names reports whether the link names a credential at all. A row can carry an
// `ai_credentials` key whose object is empty; that names nothing.
func (r *modelCredentialRef) names() bool {
	return r != nil && (r.title() != "" || r.ConfigurationUUID != "")
}

// credentialRef is one credential row as the model resolver needs it: enough to
// name the provider and to pin the credential. It carries no secret material —
// the account package reads the secret, per request, from the vault.
type credentialRef struct {
	// configID matches the account package's own credential id, which is the
	// row's uuid when it has one and its numeric id otherwise. The two SELECT
	// expressions MUST stay identical or the pin will never match.
	configID string
	// typ is the row's `type` column, e.g. "azure_open_ai". Together with
	// apiBase, it selects the provider through account.ProviderForCredential.
	typ string
	// apiBase is the non-secret endpoint selector. It distinguishes native
	// OpenAI credentials from the same platform type pointed at an
	// OpenAI-compatible endpoint.
	apiBase string
	// ownerProjectID is the project whose schema holds the row.
	ownerProjectID string
}

// modelCredentialLookups preserves the current platform's two reference
// scopes. projectLocal is used when data.ai_credentials.private is false;
// personal is used when it is true. modelOwnerAccess applies only to a
// project-local match from a published public-project model.
type modelCredentialLookups struct {
	projectLocal     []map[string]credentialRef
	personal         []map[string]credentialRef
	modelOwnerAccess bool
	// grantedTo is the CALLER's project id, and it is set for the public-scope
	// pass alone. A catalogue row is skipped unless its grant names this
	// project (model_grant.go). Empty means "do not filter", which is what an
	// own-project read is: a project's own rows are its own.
	//
	// It sits on this struct rather than beside it because it is the same kind
	// of fact — whose scopes this read is being made for — and adding a
	// positional parameter to queryScope for it would make the two boolean
	// arguments three.
	grantedTo string
}

// modelSection is one (section, type) pair in p_{projectID}.configuration that
// holds a model a /llm route can address.
type modelSection struct {
	section string
	typ     string
}

// addressableModelSections enumerates the configuration rows that describe a
// model the gateway can dispatch to. elitea-main writes the two columns as a
// pair (internal/api/v2/configurations/handler.go) and its own reads verify
// both before they trust a row, so both are matched here too.
//
// This is the WHOLE model surface, not the chat surface. mapModel gates EVERY
// dialect against this set, so a section that is absent here makes the gateway
// answer 404 `model_not_found` for every model it holds — even though the
// project configured the model and the credential resolves. That is what
// happened to POST /llm/v1/embeddings while the resolver read the `llm` section
// alone: an `embedding`/`embedding_model` row was invisible, so the embedding
// hop of the index plane could never dispatch.
//
// The order is precedence: a model id that two sections both carry resolves to
// the first section in this list (see modelsSQL's ORDER BY).
//
// Keep this in step with the routes in internal/api/router.go. `vectorstorage`
// holds no model at all, so it is absent. ADD THE PAIR HERE when you add a
// route that dispatches one.
//
// `asr` and `tts` joined the list with the audio routes (issue #323). While
// they were absent the gateway had no way to dispatch a voice model even after
// the routes existed: mapModel gates EVERY dialect against this one set, so
// every /llm/v1/audio/* call would have answered 404 `model_not_found` for a
// model the project had configured and whose credential resolved. That is the
// same defect the `embedding` pair fixed one paragraph above.
//
// THE REALTIME ROUTE ADDS NO PAIR, and that is a finding rather than an
// omission. /llm/v1/realtime dispatches a speech-to-text model, which is the
// `asr` pair already on this list. elitea-main writes exactly five model types —
// llm_model, embedding_model, asr_model, tts_model, image_generation_model
// (internal/api/v2/configurations/handler.go) — and no `realtime` section
// exists for a project to put a row in. Declaring one here would admit a pair
// no row can ever carry, so every realtime call would answer 404 for a model
// nobody could configure. Do NOT put a realtime model in `llm` either: those
// rows ARE the chat catalogue the web model picker reads.
var addressableModelSections = []modelSection{
	{section: "llm", typ: "llm_model"},                           // /chat/completions, /completions, /responses, /messages
	{section: "embedding", typ: "embedding_model"},               // /embeddings
	{section: "image_generation", typ: "image_generation_model"}, // /images/generations, /images/edits, /images/variations
	{section: "asr", typ: "asr_model"},                           // /audio/transcriptions, /audio/translations, /realtime
	{section: "tts", typ: "tts_model"},                           // /audio/speech
}

// modelSectionArgs returns the two parallel arrays modelsSQL binds, in
// addressableModelSections order. Passing the pairs as bind parameters keeps
// the statement text fixed: nothing about which sections are read is built by
// string concatenation.
func modelSectionArgs() (sections, types []string) {
	sections = make([]string, len(addressableModelSections))
	types = make([]string, len(addressableModelSections))
	for i, s := range addressableModelSections {
		sections[i], types[i] = s.section, s.typ
	}
	return sections, types
}

// ModelResolver synthesises the per-project /llm/v1/models set from
// p_{projectID}.configuration, reading every pair in addressableModelSections.
// It caches the resolved list per project for a short TTL and, on a query
// failure, serves a stale cached list if one exists so a transient database blip
// does not empty a project's model surface. It is safe for concurrent use and
// NEVER routes through bifrost/core (design §4.2, §3.4).
type ModelResolver struct {
	db  modelRowQuerier
	ttl time.Duration
	now func() time.Time
	// publicProjectID is the platform's shared project (issue #316). Empty
	// disables the shared scope. Operator configuration, never request data.
	publicProjectID string
	logger          *slog.Logger

	mu    sync.RWMutex
	cache map[string]modelsCacheEntry
}

type modelsCacheEntry struct {
	models  []modelObject
	expires time.Time
}

// ModelResolverConfig configures a ModelResolver.
type ModelResolverConfig struct {
	// DB is the Postgres handle (*pgxpool.Pool via ModelPoolQuerier in
	// production). When nil the resolver returns an empty model set for every
	// project — a gateway booted without a database exposes no synthetic models
	// rather than erroring the /v1/models surface.
	DB modelRowQuerier
	// CacheTTL overrides the per-project cache lifetime. <= 0 uses
	// DefaultModelsCacheTTL.
	CacheTTL time.Duration
	// Now overrides the clock (tests). nil uses time.Now.
	Now func() time.Time
	// Logger is used for resolution warnings; nil uses slog.Default().
	Logger *slog.Logger
	// PublicProjectID is the platform's shared ("public") project id as a
	// decimal string (ELITEA_AI_PROJECT_ID). When set, List also returns that
	// project's `shared = true` models, so /llm/v1/models agrees with the model
	// picker the caller just used (issue #316). Empty disables the shared scope.
	//
	// This MUST be operator configuration. A request-supplied value would let a
	// caller name any project as "public" and enumerate its models.
	PublicProjectID string
}

// NewModelResolver builds a ModelResolver from cfg.
func NewModelResolver(cfg ModelResolverConfig) *ModelResolver {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultModelsCacheTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ModelResolver{
		db:              cfg.DB,
		ttl:             ttl,
		now:             now,
		publicProjectID: cfg.PublicProjectID,
		logger:          logger,
		cache:           make(map[string]modelsCacheEntry),
	}
}

// modelsSQL reads the caller-visible model ids for a project's configured
// models. elitea_title is the alias the caller uses in the request `model`
// field (it is what ChatConfig surfaces as the model "name"); data carries the
// underlying wire name as a fallback id.
//
// The admitted rows are the (section, type) pairs of addressableModelSections,
// bound as two parallel arrays and joined WITH ORDINALITY. The ordinality is
// load-bearing rather than decorative: it orders the result by the declared
// section order first and by row id second. The chat models therefore keep the
// position they held when the `llm` section was the only one read, and a model
// id that two sections both carry resolves to the same row on every call.
//
// %q is the schema name. %s is the scope predicate: empty for the caller's own
// project, and sharedModelPredicate for the public project, whose rows are
// visible to another project ONLY when the platform published them. `shared` is
// selected so the public-scope result can be re-verified in Go.
const modelsSQL = `SELECT COALESCE(c.elitea_title, ''), c.data, c.shared
	FROM %q.configuration AS c
	JOIN unnest($1::text[], $2::text[]) WITH ORDINALITY AS s(section, type, ord)
	  ON c.section = s.section AND c.type = s.type
	WHERE c.status_ok = true%s
	ORDER BY s.ord, c.id`

// sharedModelPredicate restricts a cross-project read to published rows. It is
// the only thing that makes reading a second schema safe, so it is a constant
// and is never built from a caller-supplied value.
const sharedModelPredicate = " AND c.shared = true"

// credentialSection is the configuration section that holds provider
// credentials. It appears in credentialRefsSQL, and the test doubles for
// modelRowQuerier match on it to tell the two statements apart.
const credentialSection = "section = 'ai_credentials'"

// credentialRefsSQL reads the id, type, title and non-secret api_base of every
// credential row in one project scope. It never reads api_key or any vault
// value: secret resolution stays in the account package and happens per
// request.
//
// The id expression MUST match credentialsSQL in the account package
// (COALESCE(uuid::text, id::text)). The model resolver pins a credential by the
// id it reads here, and the account package matches on the id it reads there;
// two different expressions would make every pin miss.
//
// %q is the schema name. %s is the scope predicate, exactly as in modelsSQL.
const credentialRefsSQL = `SELECT COALESCE(c.uuid::text, c.id::text), COALESCE(c.type, ''), COALESCE(c.elitea_title, ''), COALESCE(c.data->>'api_base', '')
	FROM %q.configuration AS c
	WHERE c.` + credentialSection + ` AND c.status_ok = true%s
	ORDER BY c.id`

// credentialRefs reads one project scope's credential rows, keyed by
// elitea_title. sharedOnly restricts a cross-project read to published rows.
//
// A credential with no title is skipped: the stored link names a credential by
// title, so an untitled row can never be the target of one.
//
// The map is built once per model-list refresh, not once per request: the
// resolved model list is cached for CacheTTL and carries the answer with it.
func (m *ModelResolver) credentialRefs(
	ctx context.Context,
	scopeProjectID string,
	sharedOnly bool,
) (map[string]credentialRef, error) {
	predicate := ""
	if sharedOnly {
		predicate = sharedModelPredicate
	}
	q := fmt.Sprintf(credentialRefsSQL, "p_"+scopeProjectID, predicate)
	rows, err := m.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query credentials for project %s: %w", scopeProjectID, err)
	}
	defer rows.Close()

	refs := make(map[string]credentialRef)
	for rows.Next() {
		var id, typ, title, apiBase string
		if err := rows.Scan(&id, &typ, &title, &apiBase); err != nil {
			return nil, fmt.Errorf("scan credential row: %w", err)
		}
		if title == "" {
			continue
		}
		// elitea_title is unique within a schema, so the first row for a title
		// is the only row for it. Keeping the first also matches the ORDER BY.
		if _, dup := refs[title]; dup {
			continue
		}
		refs[title] = credentialRef{
			configID: id, typ: typ, apiBase: apiBase, ownerProjectID: scopeProjectID,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate credential rows: %w", err)
	}
	return refs, nil
}

// List returns the synthesised model set for projectID. It serves a fresh
// cached list when one exists, else queries Postgres and caches the result. On
// a query failure it serves a stale cached list if present (logging a warning)
// so a transient database blip does not empty the surface; with no cache and a
// failing/absent database it returns an empty (non-nil) slice. An empty
// projectID yields an empty set (no project ⇒ no models).
func (m *ModelResolver) List(ctx context.Context, projectID string) []modelObject {
	models, _ := m.list(ctx, projectID)
	return models
}

// list is List plus the "did I actually read this project's model set?" answer.
// known is false when the set is UNKNOWN — an empty projectID, no database, or
// a query failure with nothing cached — and true when the returned set is the
// project's real (possibly stale, possibly empty) model set.
//
// A query failure WITH something cached is NOT unknown: the stale list is
// served and reported as known. The three conditions above are therefore the
// whole of the unknown case.
//
// The two cases must stay distinct because the inference path acts on them
// differently. mapModel owns that policy and is the only place that states it;
// do not restate the outcome here, because it has changed once already. List
// collapses both cases to an empty slice, which is correct for the
// /llm/v1/models surface.
func (m *ModelResolver) list(ctx context.Context, projectID string) (models []modelObject, known bool) {
	if projectID == "" {
		return []modelObject{}, false
	}
	if m.db == nil {
		return []modelObject{}, false
	}

	m.mu.RLock()
	ent, cached := m.cache[projectID]
	fresh := cached && m.now().Before(ent.expires)
	m.mu.RUnlock()
	if fresh {
		return ent.models, true
	}

	models, err := m.query(ctx, projectID)
	if err != nil {
		// Serve a stale cached list on a transient failure rather than emptying
		// the project's model surface. Nothing cached ⇒ empty set.
		if cached {
			m.logger.WarnContext(ctx, "models: query failed; serving stale cached list",
				"project_id", projectID, "err", err, "stale_count", len(ent.models))
			return ent.models, true
		}
		m.logger.WarnContext(ctx, "models: query failed and no cache; returning empty set",
			"project_id", projectID, "err", err)
		return []modelObject{}, false
	}

	m.mu.Lock()
	m.cache[projectID] = modelsCacheEntry{models: models, expires: m.now().Add(m.ttl)}
	m.mu.Unlock()
	return models, true
}

// Get returns the single synthesised model with the given id for projectID and
// whether it was found. It reuses List (and therefore the cache), so a
// single-model lookup never hits the database when the list is already cached.
func (m *ModelResolver) Get(ctx context.Context, projectID, id string) (modelObject, bool) {
	for _, mo := range m.List(ctx, projectID) {
		if mo.ID == id {
			return mo, true
		}
	}
	return modelObject{}, false
}

// query reads and decodes the project's model rows into a deduplicated,
// order-preserving model set.
//
// It reads two scopes (issue #316): the caller's own project, then the public
// project's `shared = true` rows. The own-project scope is read FIRST and a
// duplicate id keeps its first occurrence, so where both scopes expose the same
// model id the project's own row wins — the precedence the legacy
// _map_model_name resolver had. The second scope is skipped when it is unset or
// when the caller IS the public project.
//
// A row with no usable id (empty elitea_title and empty data.name) is skipped.
//
// TENANT ISOLATION: publicProjectID is operator configuration, never request
// data, and the public-scope read always carries sharedModelPredicate.
func (m *ModelResolver) query(ctx context.Context, projectID string) ([]modelObject, error) {
	if m.db == nil {
		return []modelObject{}, nil
	}
	if err := validateNumericProjectID(projectID); err != nil {
		return nil, err
	}

	// Read the credential rows BEFORE the model rows. A model row names its
	// credential by title, so the model rows cannot be turned into dispatchable
	// models until the titles can be looked up (issue #451).
	ownCreds, err := m.credentialRefs(ctx, projectID, false)
	if err != nil {
		return nil, err
	}

	models := make([]modelObject, 0)
	seen := make(map[string]struct{})

	publicScope := m.publicProjectID != "" && m.publicProjectID != projectID
	if !publicScope {
		if err := m.queryScope(ctx, projectID, false, modelCredentialLookups{
			projectLocal: []map[string]credentialRef{ownCreds},
			personal:     []map[string]credentialRef{ownCreds},
		}, &models, seen); err != nil {
			return nil, err
		}
		return models, nil
	}

	if err := validateNumericProjectID(m.publicProjectID); err != nil {
		return nil, fmt.Errorf("public project id: %w", err)
	}
	publicCreds, err := m.credentialRefs(ctx, m.publicProjectID, true)
	if err != nil {
		return nil, err
	}
	// A published model is a capability in the current platform. Its
	// private:false link resolves against the model's owning public project,
	// where the credential does not need to be separately shared. This second
	// non-secret metadata read is used only for published model rows below; an
	// ordinary caller-owned model still sees publicCreds and its shared=true
	// predicate only.
	publicModelOwnerCreds, err := m.credentialRefs(ctx, m.publicProjectID, false)
	if err != nil {
		return nil, err
	}

	// The caller's own model row may link to a credential the platform
	// published. Its own project is searched first, so a same-titled credential
	// of its own wins — the precedence the rest of this file already keeps.
	if err := m.queryScope(ctx, projectID, false, modelCredentialLookups{
		projectLocal: []map[string]credentialRef{ownCreds, publicCreds},
		personal:     []map[string]credentialRef{ownCreds, publicCreds},
	}, &models, seen); err != nil {
		return nil, err
	}
	// A public model's private:false link uses the model owner's credential.
	// private:true retains the current platform's personal-project behavior and
	// falls back only to a separately published public credential.
	// THE GRANT IS APPLIED HERE, on the public scope alone.
	//
	// `shared = true` says the row is a platform row; the grant says which
	// projects it is offered to (model_grant.go). The caller's own rows are
	// never filtered — they are its own whatever their `data` says — and the
	// public scope is not read at all when the caller IS the public project,
	// so the catalogue keeps seeing its own withdrawn models.
	if err := m.queryScope(ctx, m.publicProjectID, true, modelCredentialLookups{
		projectLocal:     []map[string]credentialRef{publicModelOwnerCreds},
		personal:         []map[string]credentialRef{ownCreds, publicCreds},
		modelOwnerAccess: true,
		grantedTo:        projectID,
	}, &models, seen); err != nil {
		return nil, err
	}
	return models, nil
}

// queryScope reads one project's model rows, across every pair in
// addressableModelSections, and appends the new ids to models, skipping any id
// already in seen. sharedOnly adds the `shared = true` predicate and re-verifies
// every returned row against its own `shared` column.
//
// creds are the credential lookups to search for a row's ai_credentials link,
// in precedence order (issue #451).
func (m *ModelResolver) queryScope(
	ctx context.Context,
	scopeProjectID string,
	sharedOnly bool,
	creds modelCredentialLookups,
	models *[]modelObject,
	seen map[string]struct{},
) error {
	predicate := ""
	if sharedOnly {
		predicate = sharedModelPredicate
	}
	// scopeProjectID is either the signed, server-resolved caller project or the
	// operator-configured public project. Both are numeric-validated before they
	// reach here, so the fmt-built schema identifier is not an injection vector
	// — the same guard the account package applies (design §5.3).
	q := fmt.Sprintf(modelsSQL, "p_"+scopeProjectID, predicate)
	sections, types := modelSectionArgs()
	rows, err := m.db.Query(ctx, q, sections, types)
	if err != nil {
		return fmt.Errorf("query models for project %s: %w", scopeProjectID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			title     string
			dataBytes []byte
			shared    bool
		)
		if err := rows.Scan(&title, &dataBytes, &shared); err != nil {
			return fmt.Errorf("scan model row: %w", err)
		}
		// Defence in depth: a cross-project read must never yield an unpublished
		// row. Reaching here means the query lost its predicate, so fail the read
		// rather than advertise another project's private model.
		if sharedOnly && !shared {
			return fmt.Errorf("model row from project %s escaped the shared scope", scopeProjectID)
		}
		// A platform row the caller holds no grant for is not this caller's
		// model. Skipped rather than refused: the row is valid, it is simply
		// not offered here, and the same read serves every other caller.
		if !modelRowGrantsProject(dataBytes, creds.grantedTo) {
			continue
		}
		id, providerModel, link := modelNames(title, dataBytes)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		mo := modelObject{
			ID:            id,
			Object:        modelObjectType,
			Created:       0,
			OwnedBy:       modelsOwnedBy,
			providerModel: providerModel,
		}
		// A shared row carries providerModel exactly like an own row (issue
		// #317). The scope decides WHICH rows come back; it does not change what
		// a row carries. A shared model must map to the provider's name too,
		// else the budget gate prices the wrong model and the provider gets a
		// title it does not know.
		m.applyCredentialLink(ctx, &mo, link, scopeProjectID, creds)
		*models = append(*models, mo)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate model rows: %w", err)
	}
	return nil
}

// applyCredentialLink resolves a model row's ai_credentials link onto mo and
// records the provider and the credential the row named (issue #451).
//
// The order is: the expanded shape first, because it answers by itself; then
// the stored shape, by title, against each lookup in precedence order.
//
// It leaves mo untouched in three cases, and each one keeps the pre-#451
// behaviour of taking the provider from a prefix in the model string:
//
//   - The row links to no credential. The standalone seed writes such rows and
//     relies on the prefix, so this path must not change.
//   - The link names a credential the caller's scopes do not hold. Failing here
//     would refuse a model that the prefix path can still dispatch today.
//   - The credential type is one the gateway cannot serve.
//
// The last two are logged. They are not silent: the row is advertised but not
// improved, and an operator needs to see which link did not resolve.
func (m *ModelResolver) applyCredentialLink(
	ctx context.Context,
	mo *modelObject,
	link *modelCredentialRef,
	scopeProjectID string,
	creds modelCredentialLookups,
) {
	if !link.names() {
		return
	}

	credentialType := link.ConfigurationType
	configID := link.ConfigurationUUID
	ownerProject := link.ConfigurationProjectID.String()
	title := link.title()
	credentialScopes := creds.projectLocal
	modelOwnerAccess := creds.modelOwnerAccess
	if link.Private {
		credentialScopes = creds.personal
		modelOwnerAccess = false
	}
	ref, refFound := lookupCredentialRef(credentialScopes, title)

	if credentialType == "" {
		if !refFound {
			m.logger.WarnContext(ctx, "model links to a credential that is not in scope; keeping the model-name prefix",
				"project_id", scopeProjectID, "model", mo.ID, "credential_title", title)
			return
		}
		credentialType, configID, ownerProject = ref.typ, ref.configID, ref.ownerProjectID
	}

	apiBase := ""
	if refFound {
		apiBase = ref.apiBase
	}
	provider, ok := account.ProviderForCredential(credentialType, apiBase)
	if !ok {
		m.logger.WarnContext(ctx, "model links to a credential type this gateway cannot serve; keeping the model-name prefix",
			"project_id", scopeProjectID, "model", mo.ID, "credential_type", credentialType)
		return
	}

	mo.credentialProvider = provider
	mo.credentialProject = ownerProject
	mo.credentialID = configID
	mo.credentialTitle = title
	mo.credentialModelOwnerAccess = modelOwnerAccess && ownerProject == scopeProjectID
}

// lookupCredentialRef finds title in the first lookup that holds it.
func lookupCredentialRef(creds []map[string]credentialRef, title string) (credentialRef, bool) {
	if title == "" {
		return credentialRef{}, false
	}
	for _, scope := range creds {
		if ref, ok := scope[title]; ok {
			return ref, true
		}
	}
	return credentialRef{}, false
}

// modelNames resolves the names and the credential link of one model row:
//
//   - id is the caller-visible model id: the elitea_title alias when present,
//     else the underlying data.name. It is "" when neither is usable, and the
//     row is then skipped.
//   - providerModel is the name to send to the provider: data.name. It falls
//     back to id when data.name is absent, so a row that carries a title and no
//     wire name dispatches the title exactly as it did before issue #317.
//   - link is the row's data.ai_credentials object, or nil when it has none.
//
// A malformed data JSONB is treated as absent.
func modelNames(title string, dataBytes []byte) (id, providerModel string, link *modelCredentialRef) {
	var d modelConfigData
	if len(dataBytes) > 0 {
		if err := json.Unmarshal(dataBytes, &d); err != nil {
			d = modelConfigData{}
		}
	}
	id = title
	if id == "" {
		id = d.Name
	}
	if id == "" {
		return "", "", nil
	}
	providerModel = d.Name
	if providerModel == "" {
		providerModel = id
	}
	return id, providerModel, d.AICredentials
}

// validateNumericProjectID rejects a non-numeric projectID before it is
// interpolated into the schema name. The id is server-resolved, but this guards
// a malformed/hostile value from reaching the query (mirrors the account
// package's guard).
func validateNumericProjectID(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("empty project id")
	}
	for _, r := range projectID {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid project id %q: must be numeric", projectID)
		}
	}
	return nil
}
