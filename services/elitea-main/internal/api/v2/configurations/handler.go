package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

const (
	defaultConfigurationListLimit = 20
	maxConfigurationListLimit     = 200
	// The three filter bounds the reviewed service applies
	// (application/configurations/crud.go, MaxCurrentConfigurationFilterValues,
	// MaxCurrentConfigurationFilterLength and MaxCurrentConfigurationQueryLength).
	maxConfigurationFilterValues = 64
	maxConfigurationFilterLength = 128
	maxConfigurationQueryLength  = 1024
	maxConfigurationModelRows    = 1000
	maxConfigurationRequestBytes = 1 << 20
)

type Handler struct {
	pool               *pgxpool.Pool
	permissionResolver auth.PermissionResolver
	// catalog is the same pinned, embedded registry snapshot that
	// CurrentAvailableRoute serves. It is a static, global, credential-free
	// artifact — no pool, no vault, no feature flag — so this router serves
	// it unconditionally rather than behind ELITEA_CONFIGURATIONS_ENABLED
	// (#131: that flag gates the *production* router, which this compatibility
	// router is not, so no environment ever reached the real catalogue).
	catalog *configurationapp.CurrentAvailableCatalog
	// connectionChecker performs the real, minimal provider round trip
	// CheckConnection/BatchCheckConnections need (#319, check_connection.go).
	// nil means "not configured" — the handlers then report an honest
	// "not available" failure rather than fabricating success.
	connectionChecker ConnectionChecker
	// providerAdmission decides the status_ok column for a written provider
	// row (#457, provider_admission.go). nil keeps the column at its default.
	providerAdmission ProviderAdmission
	// storedResolver expands the references and redeems the hidden secrets of
	// an ALREADY SAVED row, so a stored credential can be checked without the
	// client resending its api_key (stored_check.go). nil makes both stored
	// checks report an honest "not available" failure; it never falls back to
	// checking the stored {{secret.NAME}} reference as if it were the key.
	storedResolver StoredConfigurationResolver
	// secretSealer stores a schema-declared password value in the project
	// vault (secret_sealing.go). nil REFUSES such a write; it never falls back
	// to a plaintext row.
	secretSealer SecretSealer
	// publicProjectID names the project whose shared configurations the list
	// response's `shared` block serves. Zero means "not configured", and the
	// block is then empty — see sharedConfigurationSchema.
	publicProjectID int
	// toolkitChecker probes a TOOLKIT credential (github, gitlab, bitbucket,
	// jira, confluence) with one authenticated metadata GET — the half of #319
	// the gateway cannot serve, because the gateway speaks to LLM providers.
	// See toolkit_check.go. NewHandler always supplies one, so a toolkit type
	// this build carries a probe for is never refused for want of composition;
	// WithToolkitConnectionChecker replaces it in a test.
	toolkitChecker ToolkitConnectionChecker
}

type Option func(*Handler)

// WithPermissionResolver supplies the resolver EVERY project-scoped route in
// Routes() is gated on. Without it those routes answer 403 — see require below,
// which is fail-closed by construction, so a Handler built without a resolver
// exposes nothing rather than everything.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(handler *Handler) {
		handler.permissionResolver = resolver
	}
}

// WithConnectionChecker wires the real provider-connection checker (#319).
// Without this option CheckConnection/BatchCheckConnections report an honest
// "not available" failure for every checkable type — never a fabricated
// success.
func WithConnectionChecker(checker ConnectionChecker) Option {
	return func(handler *Handler) {
		handler.connectionChecker = checker
	}
}

// WithPublicProjectID names the PUBLIC project — the project whose shared
// configurations every other project may use. The list response's `shared`
// block reads that project's schema and no other.
//
// Without this option the block is empty. It must not fall back to the
// caller's own schema: that answer looks like a public credential list and is
// the caller's own rows.
//
// The value is ELITEA_AI_PROJECT_ID. This used to say "the composition root
// parses it only inside the ELITEA_CONFIGURATIONS_ENABLED branch. So this option
// has no caller yet, and the block stays empty in a default install" — which was
// true, and meant a shipped deployment showed no project the platform
// credentials it was entitled to use.
//
// router.go now applies it on the always-on path, from
// `apimw.PublicProjectID()`, which is the same resolution the Project middleware
// uses. That helper also learned to read ELITEA_AI_PROJECT_ID: this service read
// `AI_PROJECT_ID` while the gateway read `ELITEA_AI_PROJECT_ID`, so a deployment
// that set the documented variable — as deploy/docker-compose.standalone-full.yml
// does for BOTH services, with a CI gate asserting the two agree — was handing
// this service a variable it did not read.
func WithPublicProjectID(projectID int) Option {
	return func(handler *Handler) {
		if projectID > 0 {
			handler.publicProjectID = projectID
		}
	}
}

func NewHandler(pool *pgxpool.Pool, opts ...Option) *Handler {
	handler := &Handler{pool: pool}
	// A malformed embedded snapshot must not stop the process: every other
	// route in this handler is independent of the catalogue. Available alone
	// reports the failure, as an explicit "catalog is unavailable" error
	// rather than as a silently degraded list.
	if catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog(); err == nil {
		handler.catalog = catalog
	} else {
		slog.Error("failed to load pinned configuration catalog", "err", err)
	}
	for _, opt := range opts {
		opt(handler)
	}
	if handler.toolkitChecker == nil {
		// Built here rather than at the composition root because it needs no
		// dependency the root owns — an allowlist read from the environment and
		// an HTTP client. A nil checker would read as "this type cannot be
		// checked", which is a different claim from the one an unconfigured
		// allowlist makes (toolkit_check.go's own doc says so).
		handler.toolkitChecker = NewToolkitConnectionCheckerFromEnv()
	}
	return handler
}

// Routes is the broad current-main compatibility surface. It is the surface
// EVERY shipped deployment serves for this plugin, not a prototype: #243 made
// newProductionRouter the only build path, and router.go mounts this router at
// /api/v2/configurations there.
//
// The comment that stood here said "Production composition uses ProductionRoutes
// or the typed current handlers instead". Both halves were wrong, and the second
// half was the dangerous one:
//
//   - ProductionRoutes had no caller outside this package's tests. It is deleted
//     with this change rather than left as a second, differently-authorized
//     registration of paths this router already owns.
//   - The typed current handlers (read.go, mutation.go, models.go, types.go,
//     available_route.go) replace only the MODE-LESS twins, and only when
//     composed. Reads need ELITEA_CONFIGURATIONS_ENABLED; the writes need
//     ELITEA_CONFIGURATIONS_MUTATION_ENABLED, which deploy/README.md records as
//     off in BOTH profiles. So POST, PUT and DELETE on a project's
//     configurations always land here, and every `{mode}` twin always lands
//     here, whatever is composed.
//
// So this router carried no gate at all over the per-project CREDENTIAL store
// (#496). GET /configurations/configurations/{mode}/{projectID} answered any
// authenticated caller for any project id with the whole `configuration` table
// of that project, `data` included. DELETE removed any project's row.
//
// The `data` column also held the provider api_key VERBATIM: Create and Update
// marshalled the request body straight into it, with no store_secrets step, so
// the row kept the literal key. A gate alone does not repair that, because
// migrations/shared/0072 grants the read to the project VIEWER role as well.
// Create and Update now seal every schema-declared password field into the
// project vault and store the `{{secret.NAME}}` reference the reference
// implementation writes (secret_sealing.go). A deployment with no vault
// REFUSES such a write; it never falls back to a plaintext row.
//
// EVERY GATE BELOW IS THE ONE THE REVIEWED COPY OF THE SAME PATH ALREADY USES,
// and the five strings are the ones the reference declares
// (legacy/plugins/configurations/api/v2/{configurations,configuration}.py). They
// are granted in DEFAULT mode by migrations/shared/0072, so no new migration is
// needed and no route here answers 403 to every caller on a clean database —
// the shape of #354, #359 and #402.
//
// THE `{mode}` TWINS ARE GATED IN THE DEFAULT MODE, WHATEVER THE SEGMENT SAYS.
// The reference resolves an `administration`-mode URL against the caller's
// CENTRAL roles, which would let an operator who is a member of no project read
// every project's credentials. This router does not reproduce that: the mode
// segment is decoration here (one handler serves both, unlike /secrets, whose
// two modes address two different stores), no client in the workspace calls the
// mode-ful form at all — apps/elitea-web, apps/elitea-ui, elitea-sdk and qa/ all
// send the mode-LESS URL — and adding a cross-tenant read while closing a
// cross-tenant read would be the wrong direction. An unknown mode segment stays
// what it is today rather than becoming a 404: the gate does not read it, so no
// mode value can change the answer.
//
// `/available/` is the ONE route with no gate, and it names no project: it
// serves the pinned, credential-free registry snapshot. NewCurrentAvailableRoute
// authenticates and does not authorize the same catalogue for the same reason.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	list := h.require(CurrentConfigurationListPermission)
	details := h.require(CurrentConfigurationGetPermission)
	create := h.require(CurrentConfigurationCreatePermission)
	update := h.require(CurrentConfigurationUpdatePermission)
	remove := h.require(CurrentConfigurationDeletePermission)

	r.Get("/available/", h.Available)
	r.With(list).Get("/configurations/{projectID}", h.List)
	r.With(list).Get("/configurations/{mode}/{projectID}", h.List)
	r.With(create).Post("/configurations/{projectID}", h.Create)
	r.With(create).Post("/configurations/{mode}/{projectID}", h.Create)
	r.With(details).Get("/configuration/{projectID}/{configID}", h.Get)
	r.With(details).Get("/configuration/{mode}/{projectID}/{configID}", h.Get)
	r.With(update).Put("/configuration/{projectID}/{configID}", h.Update)
	r.With(update).Put("/configuration/{mode}/{projectID}/{configID}", h.Update)
	r.With(remove).Delete("/configuration/{projectID}/{configID}", h.Delete)
	r.With(remove).Delete("/configuration/{mode}/{projectID}/{configID}", h.Delete)
	// The connection checks take the CREATE string. The reference declares no
	// permission on either route, so this is a proposal, and it is the narrowest
	// one that costs no caller a control: the button they serve sits on the
	// credential create form and on the edit form, and the legacy matrix gives
	// `create` and `update` to exactly the same default-mode roles (admin and
	// editor), so naming one of the two withholds nothing from the other's
	// holders. It has to be a WRITE-tier string rather than `list`: the handler
	// makes the platform dial a caller-supplied api_base, attributed to the
	// {projectID} in the path through the signed identity header, and a viewer
	// who may not save a credential has no use for a pre-save probe.
	//
	// THE TOOLKIT FORMS CALL THIS ROUTE TOO, and that was checked rather than
	// assumed. apps/elitea-web features/toolkits and features/agents both post
	// to /configurations/check_connection/{projectID}/{configType} for toolkit
	// credential types (github, jira, sharepoint), so a caller who may create a
	// TOOLKIT and not a CREDENTIAL would lose that button. The legacy matrix
	// gives `models.applications.tools.create` and
	// `configurations.configuration.create` to the SAME default-mode roles —
	// admin and editor — so the two sets are identical and no role loses a
	// control here.
	r.With(create).Post("/check_connection/{projectID}/{configType}", h.CheckConnection)
	r.With(create).Post("/check_connection/{mode}/{projectID}/{configType}", h.CheckConnection)
	r.With(create).Post("/check_connections/{projectID}", h.BatchCheckConnections)
	r.With(create).Post("/check_connections/{mode}/{projectID}", h.BatchCheckConnections)
	// The STORED checks and the revalidation take the UPDATE string, not the
	// create one, and the difference is deliberate. The two routes above test
	// a payload the caller is holding — a pre-save probe on the create form.
	// These three address a row that already exists: they read it, resolve its
	// secrets server-side, and (for revalidate) write its status column. That
	// is the edit form's control, and `configurations.configuration.update` is
	// the string the reference declares for every other write to an existing
	// row (PUT /configuration/{projectID}/{configID} above). The two strings
	// are granted to the SAME default-mode roles by migrations/shared/0072 —
	// admin and editor — so no role loses a control either way, and naming the
	// narrower-in-intent one keeps a viewer, who cannot edit the credential,
	// from making the platform dial the provider with it.
	r.With(update).Post("/check_stored_connection/{projectID}/{configID}", h.CheckStoredConnection)
	r.With(update).Post("/check_stored_connection/{mode}/{projectID}/{configID}", h.CheckStoredConnection)
	r.With(update).Post("/check_stored_connections/{projectID}", h.BatchCheckStoredConnections)
	r.With(update).Post("/check_stored_connections/{mode}/{projectID}", h.BatchCheckStoredConnections)
	// Revalidation re-runs ADMISSION, which is not a provider contact — see
	// the head of revalidate.go for why the two are separate routes.
	r.With(update).Post("/revalidate/{projectID}/{configID}", h.RevalidateConfiguration)
	r.With(update).Post("/revalidate/{mode}/{projectID}/{configID}", h.RevalidateConfiguration)
	// models.go and model_default.go gate the reviewed copies of these two on
	// exactly these strings, over the same paths.
	r.With(list).Get("/models/{projectID}", h.ListModels)
	r.With(list).Get("/models/{mode}/{projectID}", h.ListModels)
	r.With(update).Post("/models/{projectID}", h.SetDefaultModel)
	r.With(update).Post("/models/{mode}/{projectID}", h.SetDefaultModel)
	// types.go gates the reviewed copy on the list string, and states why:
	// "listing stored type names is inventory access, not public schema
	// discovery".
	r.With(list).Get("/types/{projectID}", h.ListTypes)
	r.With(list).Get("/types/{mode}/{projectID}", h.ListTypes)
	// TTSVoices answers 501 for every project (see ttsVoicesUnavailable). It is
	// still gated, and on the list string, because a gate must match what the
	// route is for and not what its body does today: the reference reads the
	// project's tts configuration row, which is the same inventory access every
	// other read here takes. When a voice listing exists upstream to give this
	// route a real body, the gate is already the right one.
	r.With(list).Get("/tts_voices/{projectID}", h.TTSVoices)
	r.With(list).Get("/tts_voices/{mode}/{projectID}", h.TTSVoices)
	return r
}

// require gates one route on the named legacy permission, resolved in DEFAULT
// mode against the `{projectID}` path segment.
//
// It fails closed twice over, and both matter here.
//
//  1. RequireResolvedPermissionsForProject answers 403 when the resolver is nil,
//     so a Handler built without WithPermissionResolver serves nothing.
//  2. legacyrbac.PostgresResolver parses the project id with parsePositiveID in
//     the default mode, so a project id that is not a positive integer is
//     refused BEFORE any handler runs.
//
// The handlers below no longer depend on either gate for their SQL. Each one
// builds its tenant schema with tenantSchema, which refuses a project id that
// is not a plain decimal number and quotes the name with SQL rules. The
// previous code interpolated the id with %q, which is Go string quoting: it
// writes an embedded quote as \" where PostgreSQL wants it doubled, so a
// crafted id left the identifier. It failed only because the backslash landed
// inside the name. That was an accident, not a defence (issue #543).
func (h *Handler) require(permission string) func(http.Handler) http.Handler {
	return middleware.RequireResolvedPermissions(
		h.permissionResolver,
		auth.PermissionModeDefault,
		permission,
	)
}

// Available serves the pinned registry snapshot — the same entries, with the
// same `config_schema`, that CurrentAvailableRoute serves. It replaces a
// hardcoded eight-row list of `{type, display_name, section}` that carried no
// schema at all, which the credential type picker cannot render a form from
// (#131). Section filtering follows Flask request.args.getlist semantics, as
// on the production route.
func (h *Handler) Available(w http.ResponseWriter, r *http.Request) {
	entries, err := h.catalog.CompleteEntries(r.URL.Query()["section"]...)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, configurationapp.ErrCurrentAvailableCatalogPartial) {
			status = http.StatusServiceUnavailable
		}
		writeCurrentConfigurationError(w, status, "configuration catalog is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, newCurrentAvailableConfigurationTypesDTO(entries))
}

// Configuration is the compatibility write/read projection.
//
// `project_id` is a NUMBER, and that is not a free choice: the reviewed read
// route serves the same field from CurrentConfigurationDTO, whose ProjectID is
// an int32, so it emits `"project_id": 2`. This struct emitted the string
// `"2"` for the same column. A client that reads both routes — the
// AI-Configuration screen reads the list from one and a single row from the
// other — then compared a number with a string, and every card reported "No
// edit permissions" because the two never matched. `int` (not `int32`) keeps
// the field consistent with `ID` and `AuthorID` in this same struct; the wire
// type is what has to agree, and a Go `int` and an `int32` both marshal to a
// JSON number. dto_test.go pins the agreement.
type Configuration struct {
	ID         int            `json:"id"`
	UUID       string         `json:"uuid,omitempty"`
	ProjectID  int            `json:"project_id"`
	Label      string         `json:"label,omitempty"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Section    string         `json:"section"`
	Data       map[string]any `json:"data,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Shared     bool           `json:"shared"`
	StatusOK   bool           `json:"status_ok"`
	StatusLogs string         `json:"status_logs,omitempty"`
	Source     string         `json:"source"`
	AuthorID   *int           `json:"author_id,omitempty"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at,omitempty"`
}

// MarshalJSON serves the stored title under BOTH `name` and `elitea_title`.
//
// `Name` holds the `elitea_title` COLUMN, and `elitea_title` is the name every
// reader in the product resolves a credential by. A toolkit does not store the
// secret, it stores `{"github_configuration": {"elitea_title": …}}`; the model
// and toolkit forms build their credential picker from `row.elitea_title`
// (apps/elitea-web/src/pages/credentials/CredentialFormFields.tsx); and the
// credential editor seeds its stable lookup key from the same field
// (useCredentialFormController.ts's `useFormSeeding`).
//
// This projection emitted the column as `name` alone, so against this handler
// every one of those readers read an empty title. Two consequences, both
// silent: the picker offered NO credential to link — the list of options is
// built by filtering out the empty ones — and an edit-save re-derived the
// stable key from the display label, so renaming a credential rewrote the key
// that every toolkit referencing it holds.
//
// `name` stays. The two read routes are a documented union (v2.yaml's
// ConfigurationRow), other clients already read `name`, and an extra key is
// additive for all of them.
//
// A marshaller rather than a struct field: the column is scanned straight into
// `Name` at five call sites (list, get, create, update, revalidate), and a
// sixth added later would otherwise ship the same empty field again.
func (c Configuration) MarshalJSON() ([]byte, error) {
	// `projection` drops this method, so the call below cannot recurse.
	type projection Configuration
	return json.Marshal(struct {
		projection
		EliteaTitle string `json:"elitea_title"`
	}{projection(c), c.Name})
}

type ListResponse struct {
	Items  []Configuration `json:"items"`
	Total  int             `json:"total"`
	Offset int             `json:"offset"`
	Limit  int             `json:"limit"`
	Shared SharedSection   `json:"shared"`
}

type SharedSection struct {
	Items  []Configuration `json:"items"`
	Total  int             `json:"total"`
	Offset int             `json:"offset"`
	Limit  int             `json:"limit"`
}

// configurationListQuery is one parsed GET /configurations/{projectID}
// request.
//
// The handler read `limit`, `offset` and `section` only. The shipped clients
// also send `type`, `query`, `sort_by`, `sort_order`, `include_shared`,
// `shared_offset` and `shared_limit`. Every dropped parameter changed the
// answer. The SharePoint credential control asks for `type=sharepoint`. It
// received the newest rows of EVERY type instead. So in a project with more
// than one page of configurations, the credential it needs is not in the
// response. The control then renders as if no credential exists.
//
// The names, the defaults and the clamps are the ones the reviewed service
// applies (application/configurations/crud.go,
// normalizeCurrentConfigurationListRequest).
type configurationListQuery struct {
	sections      []string
	types         []string
	search        string
	offset        int
	limit         int
	includeShared bool
	sharedOffset  int
	sharedLimit   int
	sortBy        string
	sortOrder     string
}

func parseConfigurationListQuery(values url.Values) configurationListQuery {
	return configurationListQuery{
		sections:      values["section"],
		types:         values["type"],
		search:        values.Get("query"),
		offset:        configurationListOffset(values.Get("offset")),
		limit:         configurationListLimit(values.Get("limit")),
		includeShared: strings.EqualFold(values.Get("include_shared"), "true"),
		sharedOffset:  configurationListOffset(values.Get("shared_offset")),
		sharedLimit:   configurationListLimit(values.Get("shared_limit")),
		sortBy:        configurationListSortBy(values.Get("sort_by")),
		sortOrder:     configurationListSortOrder(values.Get("sort_order")),
	}
}

// configurationListLimit applies the default and the clamp to one page size.
// A value that does not parse is the same as an absent one, which is what
// Flask's `request.args.get(..., type=int)` does in the reference.
func configurationListLimit(raw string) int {
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultConfigurationListLimit
	}
	if limit > maxConfigurationListLimit {
		return maxConfigurationListLimit
	}
	return limit
}

func configurationListOffset(raw string) int {
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 {
		return 0
	}
	return offset
}

// configurationListQueryInBounds reports whether the filters stay inside the
// bounds of the reviewed service.
//
// A list request carries two repeated parameters and one search string. The
// type list goes into `type = ANY($n)`, and the search string goes into a
// leading-wildcard ILIKE over the tenant table. Both are unbounded work for
// the database, so one request can hold thousands of values.
//
// The reviewed service refuses such a request
// (application/configurations/crud.go, normalizeCurrentConfigurationListRequest).
// This route refuses it with the same status and the same message.
func configurationListQueryInBounds(request configurationListQuery) bool {
	if len(request.search) > maxConfigurationQueryLength {
		return false
	}
	return configurationFilterInBounds(request.sections) && configurationFilterInBounds(request.types)
}

// configurationFilterInBounds checks the length of one repeated parameter, and
// the length of each of its values.
func configurationFilterInBounds(values []string) bool {
	if len(values) > maxConfigurationFilterValues {
		return false
	}
	for _, value := range values {
		if len(value) > maxConfigurationFilterLength {
			return false
		}
	}
	return true
}

// configurationSortColumns is the sort whitelist, and the map value reports
// whether the column holds NULL.
//
// The sort column is INTERPOLATED into the ORDER BY, because a placeholder
// there sorts by a constant. So a value outside this set must never reach SQL.
// The set is the one internal/db/queries/configurations.sql accepts, and the
// NULLS LAST flags are that query's.
var configurationSortColumns = map[string]bool{
	"id":           false,
	"uuid":         false,
	"project_id":   false,
	"label":        true,
	"elitea_title": false,
	"type":         false,
	"section":      false,
	"data":         false,
	"meta":         false,
	"shared":       false,
	"status_ok":    false,
	"status_logs":  true,
	"source":       false,
	"author_id":    true,
	"created_at":   false,
	"updated_at":   true,
}

// configurationListSortBy resolves `sort_by` against the whitelist. An
// unrecognised value falls back to created_at. It is not a 400: the reference
// ignores an unknown sort key, and a 400 would break a client that sends one.
func configurationListSortBy(raw string) string {
	if _, ok := configurationSortColumns[raw]; ok {
		return raw
	}
	return "created_at"
}

func configurationListSortOrder(raw string) string {
	if strings.EqualFold(raw, "asc") {
		return "asc"
	}
	return "desc"
}

// configurationOrderBy renders the ORDER BY for a whitelisted sort pair.
// `id ASC` is the tiebreaker the reviewed query carries, so a page boundary
// stays stable when two rows share the sort value.
func configurationOrderBy(sortBy, sortOrder string) string {
	direction := "DESC"
	if sortOrder == "asc" {
		direction = "ASC"
	}
	nulls := ""
	if configurationSortColumns[sortBy] {
		nulls = " NULLS LAST"
	}
	return fmt.Sprintf("ORDER BY %s %s%s, id ASC", sortBy, direction, nulls)
}

// configurationRowFilter builds the bound WHERE clause of one list request.
// `placeholder` is the first free placeholder index. Every value is BOUND, so
// no caller string reaches the statement text.
func configurationRowFilter(request configurationListQuery, placeholder int) (string, []any) {
	clause, args := configurationSectionFilter(request.sections, placeholder)
	placeholder += len(args)
	if len(request.types) > 0 {
		clause += " AND type = ANY($" + strconv.Itoa(placeholder) + ")"
		args = append(args, request.types)
		placeholder++
	}
	if request.search != "" {
		clause += " AND label ILIKE ('%' || $" + strconv.Itoa(placeholder) + " || '%')"
		args = append(args, request.search)
	}
	return clause, args
}

// The two sentinels separate a row this handler could not READ from a row it
// read and could not DECODE. Both are 500s, and they keep the two messages the
// handler already answered.
var (
	errConfigurationRowUnreadable = errors.New("configuration row is unreadable")
	errConfigurationRowInvalid    = errors.New("stored configuration is invalid")
)

// scanConfigurationRows reads one result set into the wire shape.
//
// The list and the shared list ran two copies of this loop. rows.Err() was
// checked in neither, so a result set that failed part way through was served
// as a short page with HTTP 200.
func scanConfigurationRows(rows pgx.Rows) ([]Configuration, error) {
	defer rows.Close()
	items := make([]Configuration, 0)
	for rows.Next() {
		var c Configuration
		var data, meta []byte
		var createdAt, updatedAt *time.Time
		if err := rows.Scan(
			&c.ID, &c.UUID, &c.ProjectID, &c.Label, &c.Name, &c.Type, &c.Section,
			&data, &meta, &c.Shared, &c.StatusOK, &c.StatusLogs, &c.Source, &c.AuthorID,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("%w: %w", errConfigurationRowUnreadable, err)
		}
		if err := json.Unmarshal(data, &c.Data); err != nil {
			return nil, fmt.Errorf("%w: %w", errConfigurationRowInvalid, err)
		}
		if err := json.Unmarshal(meta, &c.Meta); err != nil {
			return nil, fmt.Errorf("%w: %w", errConfigurationRowInvalid, err)
		}
		if createdAt != nil {
			c.CreatedAt = createdAt.Format(time.RFC3339)
		}
		if updatedAt != nil {
			c.UpdatedAt = updatedAt.Format(time.RFC3339)
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errConfigurationRowUnreadable, err)
	}
	return items, nil
}

// configurationSchemaMissing reports whether an error means the tenant schema,
// or its table, does not exist yet.
//
// This is the ONLY error a list may answer with an empty page. Every other
// error is a failure: a saturated pool, a lost connection, a statement
// timeout, a lock error. An empty page reports such a failure as a project
// that holds no credentials. The user then creates a credential that already exists, and
// the operator receives no signal at all.
func configurationSchemaMissing(err error) bool {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return false
	}
	// 3F000 invalid_schema_name, 42P01 undefined_table.
	return postgresError.Code == "3F000" || postgresError.Code == "42P01"
}

// writeConfigurationQueryFailure logs one failed statement and answers 500.
//
// A cancelled request writes nothing and logs nothing: the caller has gone, so
// the write lands on a dead connection and the log line is noise.
func writeConfigurationQueryFailure(
	ctx context.Context,
	w http.ResponseWriter,
	message string,
	projectID string,
	err error,
) {
	if ctx.Err() != nil {
		return
	}
	slog.ErrorContext(ctx, message, "project_id", projectID, "err", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "list failed")
}

// writeConfigurationRowFailure answers a row this handler could not read or
// could not decode. It keeps the two messages apart, and it logs the cause,
// which the two scan loops discarded.
func writeConfigurationRowFailure(ctx context.Context, w http.ResponseWriter, projectID string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.ErrorContext(ctx, "configuration row read failed", "project_id", projectID, "err", err)
	if errors.Is(err, errConfigurationRowInvalid) {
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid stored configuration")
		return
	}
	apierr.WriteStatus(w, http.StatusInternalServerError, "list failed")
}

// sharedConfigurationSchema names the schema the `shared` block reads, and
// reports whether the block is served at all.
//
// It is the PUBLIC project's schema, never the caller's. The block used to read
// `WHERE shared = true` in the CALLER's own schema. The main page used to read
// `WHERE shared = false` in the same schema. So a credential left the
// AI-Configuration page the moment the user shared it. The public credentials
// the block exists for were unreachable from every project. The
// reviewed service reads request.PublicProjectID for this page, and omits it
// when the caller IS the public project (application/configurations/crud.go).
//
// Without WithPublicProjectID the block stays empty. An empty block is the
// honest answer: the caller's own schema is a wrong answer, not a fallback.
func (h *Handler) sharedConfigurationSchema(projectID string, request configurationListQuery) (string, bool) {
	if !request.includeShared || h.publicProjectID <= 0 {
		return "", false
	}
	callerID, err := strconv.Atoi(projectID)
	if err != nil || callerID == h.publicProjectID {
		return "", false
	}
	shared, err := tenantschema.QuoteInt(int64(h.publicProjectID))
	if err != nil {
		return "", false
	}
	return shared, true
}

// configurationSelectColumns is the row shape both list statements read. It is
// a constant, never caller data.
const configurationSelectColumns = `id, COALESCE(uuid::text, ''), project_id, COALESCE(label, ''), elitea_title, type, section,
			data, meta, shared, status_ok, COALESCE(status_logs, ''), source, author_id,
			created_at, updated_at`

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return
	}
	projectID := chi.URLParam(r, "projectID")
	// The client always sends ?section= (it fires one request per section),
	// and this handler ignored it: every section received the whole table, so
	// one credential rendered under all seven headings — LLM, Embedding, TTS
	// and the rest alike (#131, measured: 7 copies of a single row).
	request := parseConfigurationListQuery(r.URL.Query())
	// An oversized filter never reaches the database. The message is the one
	// the reviewed route gives for the same refusal (read.go).
	if !configurationListQueryInBounds(request) {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid configuration request")
		return
	}
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	filter, filterArgs := configurationRowFilter(request, 1)

	// The page carries every row the project OWNS, shared or not. The old
	// `shared = false` predicate hid a shared credential from its own project.
	var total int
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.configuration WHERE TRUE%s`, schema, filter)
	if err := h.pool.QueryRow(ctx, countQ, filterArgs...).Scan(&total); err != nil {
		if configurationSchemaMissing(err) {
			writeJSON(w, http.StatusOK, emptyConfigurationList(request))
			return
		}
		writeConfigurationQueryFailure(ctx, w, "configuration count failed", projectID, err)
		return
	}

	listQ := fmt.Sprintf(`
		SELECT %s
		FROM %s.configuration
		WHERE TRUE%s
		%s
		LIMIT $%d OFFSET $%d
	`, configurationSelectColumns, schema, filter, configurationOrderBy(request.sortBy, request.sortOrder),
		len(filterArgs)+1, len(filterArgs)+2)

	// The count above proved the schema exists, so no error here is a missing
	// schema. Every one of them is a failure, and the empty page this branch
	// used to answer also discarded the `total` the previous statement read.
	rows, err := h.pool.Query(ctx, listQ, append(append([]any{}, filterArgs...), request.limit, request.offset)...)
	if err != nil {
		writeConfigurationQueryFailure(ctx, w, "configuration list failed", projectID, err)
		return
	}
	items, err := scanConfigurationRows(rows)
	if err != nil {
		writeConfigurationRowFailure(ctx, w, projectID, err)
		return
	}

	response := ListResponse{
		Items:  items,
		Total:  total,
		Offset: request.offset,
		Limit:  request.limit,
		Shared: emptySharedConfigurationSection(request),
	}
	sharedSchema, ok := h.sharedConfigurationSchema(projectID, request)
	if !ok {
		writeJSON(w, http.StatusOK, response)
		return
	}
	if !h.appendSharedConfigurations(ctx, w, sharedSchema, projectID, request, &response) {
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// appendSharedConfigurations fills the `shared` block from the public project.
// It reports whether the caller may still write the response.
//
// The block carries the type and section filters, as the reviewed service does,
// and honours shared_offset and shared_limit. Both were hardcoded to 0 and 20.
// The label search is NOT applied here: the reviewed service passes no
// LabelQuery to the shared page.
func (h *Handler) appendSharedConfigurations(
	ctx context.Context,
	w http.ResponseWriter,
	sharedSchema string,
	projectID string,
	request configurationListQuery,
	response *ListResponse,
) bool {
	sharedFilter, sharedArgs := configurationRowFilter(
		configurationListQuery{sections: request.sections, types: request.types}, 1)

	var sharedTotal int
	sharedCountQ := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.configuration WHERE shared = true%s`, sharedSchema, sharedFilter)
	if err := h.pool.QueryRow(ctx, sharedCountQ, sharedArgs...).Scan(&sharedTotal); err != nil {
		// This is the FIRST statement against the public project's schema, so
		// a missing schema is answerable: the public project holds nothing to
		// share. Any other error is a failure of the whole list.
		if configurationSchemaMissing(err) {
			return true
		}
		writeConfigurationQueryFailure(ctx, w, "shared configuration count failed", projectID, err)
		return false
	}

	sharedQ := fmt.Sprintf(`
		SELECT %s
		FROM %s.configuration
		WHERE shared = true%s
		%s
		LIMIT $%d OFFSET $%d
	`, configurationSelectColumns, sharedSchema, sharedFilter,
		configurationOrderBy(request.sortBy, request.sortOrder),
		len(sharedArgs)+1, len(sharedArgs)+2)

	// An error here used to leave an empty item list beside a non-zero
	// sharedTotal, and said nothing.
	sharedRows, err := h.pool.Query(
		ctx, sharedQ, append(append([]any{}, sharedArgs...), request.sharedLimit, request.sharedOffset)...)
	if err != nil {
		writeConfigurationQueryFailure(ctx, w, "shared configuration list failed", projectID, err)
		return false
	}
	sharedItems, err := scanConfigurationRows(sharedRows)
	if err != nil {
		writeConfigurationRowFailure(ctx, w, projectID, err)
		return false
	}
	response.Shared.Items = sharedItems
	response.Shared.Total = sharedTotal
	return true
}

// emptySharedConfigurationSection echoes the REQUESTED shared page. The
// response used to report `offset: 0, limit: 20` whatever the caller asked for,
// so a client could not page the block at all.
func emptySharedConfigurationSection(request configurationListQuery) SharedSection {
	return SharedSection{
		Items:  []Configuration{},
		Total:  0,
		Offset: request.sharedOffset,
		Limit:  request.sharedLimit,
	}
}

func emptyConfigurationList(request configurationListQuery) ListResponse {
	return ListResponse{
		Items:  []Configuration{},
		Total:  0,
		Offset: request.offset,
		Limit:  request.limit,
		Shared: emptySharedConfigurationSection(request),
	}
}

// configurationTitleConflict reports whether a write failed on the UNIQUE
// constraint over elitea_title.
//
// `elitea_title VARCHAR NOT NULL UNIQUE` (migrations/001_initial.sql) gives the
// constraint the name `configuration_elitea_title_key`. The match is POSITIVE
// on that column name.
//
// A negative match on the uuid constraint reads every other 23505 as a
// duplicate title. The table also holds `id SERIAL PRIMARY KEY`. A sequence
// behind the maximum id raises 23505 on `configuration_pkey`, and the user
// then reads "Configuration already exists" for a server fault.
func configurationTitleConflict(err error) bool {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" {
		return false
	}
	return strings.Contains(postgresError.ConstraintName, "elitea_title")
}

// writeConfigurationCreateFailure answers one failed INSERT.
//
// Every error, including the UNIQUE violation on elitea_title, answered 500
// `{"error":"create failed"}`. A user who saves a second credential with a name
// the project already holds makes a plain client mistake. The platform reported
// it as a server fault. The platform also logged nothing. The reviewed twin answers
// 400 "Configuration already exists" on field elitea_title (mutation.go), and
// this route now emits the same body.
func writeConfigurationCreateFailure(ctx context.Context, w http.ResponseWriter, projectID string, err error) {
	if ctx.Err() != nil {
		return
	}
	if configurationTitleConflict(err) {
		writeCurrentConfigurationMutationError(
			w, http.StatusBadRequest, "Configuration already exists", "elitea_title")
		return
	}
	if errors.Is(err, errConfigurationSecretStoreUnavailable) {
		slog.ErrorContext(ctx, "configuration secret sealing failed", "project_id", projectID, "err", err)
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration secret store is not available")
		return
	}
	slog.ErrorContext(ctx, "configuration create failed", "project_id", projectID, "err", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "create failed")
}

// writeConfigurationUpdateFailure answers one failed UPDATE.
//
// Three causes had one answer: 404 "configuration not found". A rename onto a
// title the project already holds reported a missing row, and a database
// failure reported a missing row as well. Only pgx.ErrNoRows is a 404 now.
func writeConfigurationUpdateFailure(
	ctx context.Context,
	w http.ResponseWriter,
	projectID string,
	configID string,
	err error,
) {
	if ctx.Err() != nil {
		return
	}
	if configurationTitleConflict(err) {
		writeCurrentConfigurationMutationError(
			w, http.StatusBadRequest, "Configuration already exists", "elitea_title")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}
	if errors.Is(err, errConfigurationSecretStoreUnavailable) {
		slog.ErrorContext(ctx, "configuration secret sealing failed",
			"project_id", projectID, "configuration_id", configID, "err", err)
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration secret store is not available")
		return
	}
	slog.ErrorContext(ctx, "configuration update failed",
		"project_id", projectID, "configuration_id", configID, "err", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	q := fmt.Sprintf(`
		SELECT id, COALESCE(uuid::text, ''), project_id, COALESCE(label, ''), elitea_title, type, section,
			data, meta, shared, status_ok, COALESCE(status_logs, ''), source, author_id,
			created_at, updated_at
		FROM %s.configuration WHERE %s = $1
	`, schema, configurationIDColumn(configID))

	var c Configuration
	var data, meta []byte
	var createdAt, updatedAt *time.Time
	err := h.pool.QueryRow(ctx, q, configID).Scan(
		&c.ID, &c.UUID, &c.ProjectID, &c.Label, &c.Name, &c.Type, &c.Section,
		&data, &meta, &c.Shared, &c.StatusOK, &c.StatusLogs, &c.Source, &c.AuthorID,
		&createdAt, &updatedAt,
	)
	if err != nil {
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}
	if err := json.Unmarshal(data, &c.Data); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid stored configuration")
		return
	}
	if err := json.Unmarshal(meta, &c.Meta); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid stored configuration")
		return
	}
	if createdAt != nil {
		c.CreatedAt = createdAt.Format(time.RFC3339)
	}
	if updatedAt != nil {
		c.UpdatedAt = updatedAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var body map[string]any
	if !decodeBoundedJSON(w, r, &body) {
		return
	}

	// The type's own schema decides which fields a create must carry
	// (required_fields.go). It runs FIRST, on the body as sent: a row with no
	// label and no data used to be stored with a 201 and could then be
	// explained by nobody — the catalogue reader treats an unlabelled model row
	// as an error, and a credential with no data names no endpoint.
	//
	// An unknown type carries no schema and keeps the behaviour it had. A row
	// filed under a section that is not the type's own is checked as a ROW and
	// not as an instance of the type — see required_fields.go for the
	// compatibility shape that rule exists for.
	if entry, known := h.catalog.EntryByType(strVal(body, "type")); known {
		depth := requiredFieldDepthRow
		if requested := strVal(body, "section"); requested == "" || requested == entry.Section {
			depth = requiredFieldDepthData
		}
		if missing := missingRequiredConfigurationFields(entry.ConfigSchema, body, depth); len(missing) > 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, requiredConfigurationFieldsMessage(missing))
			return
		}
	}

	dataMap, _ := body["data"].(map[string]any)
	if dataMap == nil {
		dataMap = map[string]any{}
	}
	if err := validateNotSelfReferential(dataMap, selfLLMOrigins()); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, err.Error())
		return
	}

	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return
	}
	configType := strVal(body, "type")
	// The api_key the caller sends must never reach the row. It goes to the
	// project vault, and the row keeps the {{secret.NAME}} reference.
	sealedData, secretMutations, failure := h.sealConfigurationSecrets(ctx, configType, dataMap)
	if failure != nil {
		failure.write(w)
		return
	}

	dataBytes, err := json.Marshal(sealedData)
	if err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid configuration data")
		return
	}
	// `meta` follows the same rule the partial UPDATE applies: an absent key
	// and a present JSON null both store `{}`.
	//
	// It used to be `json.Marshal(body["meta"])`, and the UI never sends the
	// key. json.Marshal(nil) is the four bytes `null`, so every configuration
	// created through this route stored `meta = 'null'::jsonb` — a value the
	// column's own default (`'{}'`) exists to prevent, and one the typed
	// reader refuses ("decode current configuration metadata: JSON object is
	// required"). The create answered 201 and the list of that section then
	// answered 500 for every member of the project, permanently: one row made
	// the whole credentials screen unreachable.
	metaBytes, metaReason := createdMetadataColumn(body)
	if metaReason != "" {
		apierr.WriteStatus(w, http.StatusBadRequest, metaReason)
		return
	}
	shared, _ := body["shared"].(bool)

	q := fmt.Sprintf(`
		INSERT INTO %s.configuration (project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, author_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false, 'user', $9)
		RETURNING id, uuid::text, created_at
	`, schema)

	pID, err := strconv.Atoi(projectID)
	if err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project")
		return
	}
	var authorID any
	var snapshotAuthorID *int
	if user, ok := auth.UserFromContext(ctx); ok {
		// author_id is an INTEGER column, so an id above math.MaxInt32 names
		// no row. The sibling mutation service applies the same bound
		// (application/configurations/mutation.go). Without the bound,
		// `int(owningUserID)` truncates on a 32-bit build. The row is then
		// stored against a different person's id.
		if owningUserID, safe := user.OwningUserID(); safe && owningUserID > 0 && owningUserID <= math.MaxInt32 {
			authorID = owningUserID
			owner := int(owningUserID)
			snapshotAuthorID = &owner
		}
	}
	title := firstStrVal(body, "elitea_title", "name")
	// The display label, read once and used TWICE — stored in the INSERT below
	// and echoed on the created row.
	//
	// It used to be read for the INSERT alone. The field is `omitempty`, so the
	// stored value simply vanished from the 201 while both read routes served
	// it, and a client that rendered its list from the create's own answer
	// showed a credential with no label until something else reloaded the row.
	// This is the same class as the `elitea_title`/`name` gap one field over in
	// the same struct.
	label := strVal(body, "label")
	section := h.sectionFor(configType, strVal(body, "section"))

	var id int
	var uuid string
	var createdAt time.Time
	err = h.withConfigurationSecretTx(ctx, pID, secretMutations, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q,
			pID,
			label,
			title,
			configType,
			section,
			dataBytes,
			metaBytes,
			shared,
			authorID,
		).Scan(&id, &uuid, &createdAt)
	})
	if err != nil {
		writeConfigurationCreateFailure(ctx, w, projectID, err)
		return
	}

	c := Configuration{
		ID:        id,
		UUID:      uuid,
		ProjectID: pID,
		Label:     label,
		Name:      title,
		Type:      configType,
		Section:   section,
		Shared:    shared,
		Source:    "user",
		CreatedAt: createdAt.Format(time.RFC3339),
	}
	if err := json.Unmarshal(dataBytes, &c.Data); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid configuration data")
		return
	}
	if err := json.Unmarshal(metaBytes, &c.Meta); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid configuration metadata")
		return
	}
	// The INSERT above stores the pending status explicitly. A provider row that
	// resolves must reach status_ok = true here, in this request, because the
	// LLM gateway admits only status_ok = true and no other component in a
	// shipped stack writes the column (#457).
	if snapshot, ok := configurationAdmissionSnapshot(
		id, uuid, pID, title, configType, section, c.Data, snapshotAuthorID,
	); ok {
		c.StatusOK = h.admitConfiguration(ctx, schema, c.StatusOK, snapshot)
	}

	writeJSON(w, http.StatusCreated, c)
}

// sectionFor resolves the `section` column for a configuration. The UI never
// sends one — it posts {elitea_title, label, data, shared, type} — so the
// column was written empty and the row belonged to none of the sections the
// AI-Configuration page queries (#131). The registry entry for the type is
// the authority (open_ai → ai_credentials), matching what the current
// mutation service does (application/configurations/mutation.go).
// An explicit body value still wins, and an unknown type still stores "".
func (h *Handler) sectionFor(configType, requested string) string {
	if requested != "" {
		return requested
	}
	entry, ok := h.catalog.EntryByType(configType)
	if !ok {
		return ""
	}
	return entry.Section
}

// Update applies a PARTIAL change. Only the fields the body carries are
// written, which is the contract this compatibility route exists to preserve:
// the reference implementation dumps the parsed body with `exclude_unset=True`
// and assigns the surviving keys one by one
// (legacy/plugins/configurations/utils.py).
//
// Before this change the statement assigned `data`, `meta` and `shared`
// unconditionally, from a body decoded into map[string]any. An absent `data`
// key therefore became `{}`, an absent `meta` became JSON null, and an absent
// `shared` became false. A caller that sent `{"shared":true}` — the documented
// way to share a configuration — erased the row's provider credential. Nothing
// keeps a copy of that column, so the value was unrecoverable. The Update then
// re-ran provider admission over the emptied row and wrote status_ok = false,
// which withdrew the credential from the LLM gateway as well. The four string
// columns were already guarded with COALESCE, so the three columns that hold
// the payload were the only ones that behaved this way.
//
// A present key that carries the wrong JSON type is now a 400 rather than a
// silent default. `"data": "oops"` used to become `{}` and `"shared": 1` used
// to become false, so a malformed body wiped the row just as an absent key did.
//
// Provider admission stays unconditional. Its snapshot comes from the RETURNING
// row — the row as actually stored. So it is correct under a partial update. A
// change of `type`, `section` or `elitea_title` alone still needs a fresh
// decision (#457).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var body map[string]any
	if !decodeBoundedJSON(w, r, &body) {
		return
	}
	// The self-referential guard answers before the transaction opens. It
	// refuses a client mistake, so it must not depend on the database.
	if failure := validateUpdatedConfigurationData(body); failure != nil {
		failure.write(w)
		return
	}
	pID, convErr := strconv.Atoi(projectID)
	if convErr != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return
	}

	c, failure, err := h.applyConfigurationUpdate(ctx, body, pID, schema, configID)
	if failure != nil {
		failure.write(w)
		return
	}
	if err != nil {
		writeConfigurationUpdateFailure(ctx, w, projectID, configID, err)
		return
	}
	// An update carries new data, so the previous decision no longer describes
	// the row. A row that stops resolving must drop back to status_ok = false:
	// withdrawing the row from every reader is exactly this write (#457).
	// The path project identifier is the one the schema was built from, so it
	// is the project whose row was just written.
	if snapshot, ok := configurationAdmissionSnapshot(
		c.ID, c.UUID, pID, c.Name, c.Type, c.Section, c.Data, c.AuthorID,
	); ok {
		c.StatusOK = h.admitConfiguration(ctx, schema, c.StatusOK, snapshot)
	}
	writeJSON(w, http.StatusOK, c)
}

// applyConfigurationUpdate runs the whole write in ONE transaction: it reads
// the stored type, seals the submitted secrets, applies the partial update and
// stores the sealed values.
//
// The stored type is read inside the transaction because the body may omit
// `type`. The type names the schema that says which field is a password, so a
// stale answer can store a credential in clear text.
func (h *Handler) applyConfigurationUpdate(
	ctx context.Context,
	body map[string]any,
	projectID int,
	schema string,
	configID string,
) (Configuration, *configurationWriteFailure, error) {
	var c Configuration
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return c, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	configType, err := h.updatedConfigurationType(ctx, tx, body, schema, configID)
	if err != nil {
		return c, nil, err
	}
	// The required-field rule, for the `data` object this body carries. It runs
	// on the resolved type — which the body may have omitted — and before any
	// secret is sealed, so a refused update writes nothing and mints no vault
	// entry. See refuseIncompleteUpdatedModelData for why it is the model rows
	// that are held to it.
	if failure := h.refuseIncompleteUpdatedModelData(body, configType); failure != nil {
		return c, failure, nil
	}
	secretMutations, failure := h.sealConfigurationBodyData(ctx, body, configType)
	if failure != nil {
		return c, failure, nil
	}

	q, args, reason := h.buildConfigurationUpdate(body, schema, configID)
	if reason != "" {
		return c, &configurationWriteFailure{status: http.StatusBadRequest, message: reason}, nil
	}
	failure, err = h.scanUpdatedConfiguration(ctx, tx, q, args, &c)
	if failure != nil || err != nil {
		return c, failure, err
	}
	if err = h.sealTransactionSecrets(ctx, tx, projectID, secretMutations); err != nil {
		return c, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return c, nil, err
	}
	return c, nil, nil
}

// scanUpdatedConfiguration runs the partial UPDATE and decodes the row it
// returns.
//
// The statement error travels through the second result, so pgx.ErrNoRows
// still answers 404 and a title conflict still answers 400. A row this handler
// cannot decode is the first result, because that is not a statement failure.
func (h *Handler) scanUpdatedConfiguration(
	ctx context.Context,
	tx pgx.Tx,
	query string,
	args []any,
	c *Configuration,
) (*configurationWriteFailure, error) {
	var data, meta []byte
	var createdAt, updatedAt *time.Time
	if err := tx.QueryRow(ctx, query, args...).Scan(
		&c.ID, &c.UUID, &c.ProjectID, &c.Label, &c.Name, &c.Type, &c.Section,
		&data, &meta, &c.Shared, &c.StatusOK, &c.StatusLogs, &c.Source, &c.AuthorID,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &c.Data); err != nil {
		return invalidStoredConfiguration(), nil
	}
	if err := json.Unmarshal(meta, &c.Meta); err != nil {
		return invalidStoredConfiguration(), nil
	}
	if createdAt != nil {
		c.CreatedAt = createdAt.Format(time.RFC3339)
	}
	if updatedAt != nil {
		c.UpdatedAt = updatedAt.Format(time.RFC3339)
	}
	return nil, nil
}

// buildConfigurationUpdate returns the partial UPDATE for one request body, its
// bound arguments, and an empty reason. A non-empty reason names a present
// field that could not be decoded, and the caller answers 400 with it.
//
// The four string columns keep their COALESCE guard, so an omitted name still
// keeps the stored one. `data`, `meta` and `shared` are assigned ONLY when the
// body carries the key.
func (h *Handler) buildConfigurationUpdate(
	body map[string]any,
	schema string,
	configID string,
) (query string, args []any, reason string) {
	updatedType := strVal(body, "type")
	assignments := []string{
		"label = COALESCE($1, label)",
		"elitea_title = COALESCE($2, elitea_title)",
		"type = COALESCE($3, type)",
		"section = COALESCE($4, section)",
	}
	args = []any{
		nullableStrVal(strVal(body, "label")),
		nullableStrVal(firstStrVal(body, "elitea_title", "name")),
		nullableStrVal(updatedType),
		nullableStrVal(h.sectionFor(updatedType, strVal(body, "section"))),
	}

	for _, column := range []string{"data", "meta"} {
		encoded, present, objectReason := updatedObjectColumn(body, column)
		if objectReason != "" {
			return "", nil, objectReason
		}
		if !present {
			continue
		}
		args = append(args, encoded)
		assignments = append(assignments, fmt.Sprintf("%s = $%d", column, len(args)))
	}

	if raw, present := body["shared"]; present {
		shared, isBool := raw.(bool)
		if !isBool {
			return "", nil, "invalid configuration sharing flag"
		}
		args = append(args, shared)
		assignments = append(assignments, fmt.Sprintf("shared = $%d", len(args)))
	}

	assignments = append(assignments, "updated_at = now()")
	args = append(args, configID)

	query = fmt.Sprintf(`
		UPDATE %s.configuration SET
			%s
		WHERE %s = $%d
		RETURNING id, COALESCE(uuid::text, ''), project_id, COALESCE(label, ''), elitea_title, type, section,
			data, meta, shared, status_ok, COALESCE(status_logs, ''), source, author_id, created_at, updated_at
	`, schema, strings.Join(assignments, ",\n\t\t\t"), configurationIDColumn(configID), len(args))
	return query, args, ""
}

// validateUpdatedConfigurationData runs the self-referential guard over a
// present `data` object.
//
// The guard runs before the transaction opens. Its refusal is a client
// mistake, so it must not depend on the database.
func validateUpdatedConfigurationData(body map[string]any) *configurationWriteFailure {
	object, isObject := body["data"].(map[string]any)
	if !isObject {
		return nil
	}
	if err := validateNotSelfReferential(object, selfLLMOrigins()); err != nil {
		return &configurationWriteFailure{status: http.StatusBadRequest, message: err.Error()}
	}
	return nil
}

// createdMetadataColumn encodes the `meta` column of a CREATE, applying the
// rule the partial UPDATE already applies: an absent key and a present JSON
// null both store `{}`.
//
// The column holds a dictionary in the reference model and in every reader, so
// a null there is off-contract and no reader accepts it.
func createdMetadataColumn(body map[string]any) (encoded []byte, reason string) {
	encoded, _, reason = updatedObjectColumn(map[string]any{"meta": body["meta"]}, "meta")
	return encoded, reason
}

// updatedObjectColumn encodes one jsonb column of a partial UPDATE.
//
// Presence is read with the two-value map lookup, because absence and an empty
// object are the same value after a type assertion. A present JSON null becomes
// an empty object: the column holds a dictionary in the reference model and in
// every reader, so a null there is off-contract.
//
// column is interpolated into the statement. Its caller passes a literal, never
// caller data.
func updatedObjectColumn(body map[string]any, column string) (encoded []byte, present bool, reason string) {
	raw, present := body[column]
	if !present {
		return nil, false, ""
	}
	invalid := "invalid configuration data"
	if column == "meta" {
		invalid = "invalid configuration metadata"
	}
	object, isObject := raw.(map[string]any)
	if !isObject && raw != nil {
		return nil, false, invalid
	}
	if object == nil {
		object = map[string]any{}
	}
	if column == "data" {
		if err := validateNotSelfReferential(object, selfLLMOrigins()); err != nil {
			return nil, false, err.Error()
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, false, invalid
	}
	return encoded, true, ""
}

// Delete removes one configuration row.
//
// A failed statement and an absent row answered the SAME 404. So a delete that
// did not happen was reported as a row that was already gone. The row stayed,
// nothing was logged, and a client that treats DELETE-404 as idempotent
// success stopped retrying. Only RowsAffected() == 0 is a 404 now. Every
// sibling in this service already splits the two (scheduling/schedules.go,
// projects/groups.go, admin/projects.go).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return
	}
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	q := fmt.Sprintf(`DELETE FROM %s.configuration WHERE %s = $1`, schema, configurationIDColumn(configID))
	ct, err := h.pool.Exec(ctx, q, configID)
	if err != nil {
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "configuration delete failed",
				"project_id", projectID, "configuration_id", configID, "err", err)
			apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	if ct.RowsAffected() == 0 {
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CheckConnection and BatchCheckConnections are implemented in
// check_connection.go (#319) — they used to be unconditional stubs here that
// reported success for every payload without ever contacting the provider.

// Model is the compatibility projection of the model list.
//
// `project_id` is a NUMBER here for the same reason it is one on
// Configuration above. The reviewed model list serves
// CurrentModelCatalogItem, whose ProjectID is an int32
// (internal/application/configurations/models.go), so that route emits
// `"project_id": 2`. This struct emitted the string `"2"` for the same
// column. ELITEA_CONFIGURATIONS_ENABLED selects which of the two routes
// answers, so one deployment gave a client a number and another gave it a
// string, and a client that compares the value with `===` breaks on exactly
// one of them. `int` (not `int32`) keeps the field consistent with `ID` and
// `ConfigID` in this same struct; a Go `int` and an `int32` both marshal to a
// JSON number. dto_test.go pins the agreement.
type Model struct {
	ID         int            `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	ProjectID  int            `json:"project_id"`
	Section    string         `json:"section"`
	IsDefault  bool           `json:"is_default"`
	ConfigID   int            `json:"config_id"`
	ConfigName string         `json:"config_name"`
	Data       map[string]any `json:"data,omitempty"`
}

type TypeDescriptor struct {
	Type        string        `json:"type"`
	DisplayName string        `json:"display_name"`
	Section     string        `json:"section"`
	Fields      []interface{} `json:"fields"`
}

func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []Model{}, "total": 0})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	modelTypes := []string{"llm_model", "embedding_model", "asr_model", "tts_model", "image_generation_model"}

	q := fmt.Sprintf(`
		SELECT id, COALESCE(elitea_title, ''), type, section, data, project_id
		FROM %s.configuration
		WHERE type = ANY($1)
		ORDER BY id
		LIMIT %d
	`, schema, maxConfigurationModelRows)

	// A project whose schema is not created yet holds no models. Any OTHER
	// error is a failure, and an empty list reports it as a project with no
	// models — the same swallow the list page carried.
	rows, err := h.pool.Query(ctx, q, modelTypes)
	if err != nil {
		if configurationSchemaMissing(err) {
			writeJSON(w, http.StatusOK, map[string]any{"items": []Model{}, "total": 0})
			return
		}
		writeConfigurationQueryFailure(ctx, w, "configuration model list failed", projectID, err)
		return
	}
	defer rows.Close()

	items := make([]Model, 0)
	for rows.Next() {
		var m Model
		var dataBytes []byte
		var dbProjectID int
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.Section, &dataBytes, &dbProjectID); err != nil {
			continue
		}
		m.ConfigID = m.ID
		m.ConfigName = m.Name
		m.ProjectID = dbProjectID
		m.IsDefault = false
		if dataBytes != nil {
			if err := json.Unmarshal(dataBytes, &m.Data); err != nil {
				apierr.WriteStatus(w, http.StatusInternalServerError, "invalid stored model")
				return
			}
		}
		items = append(items, m)
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// modelDefaultUnavailable is what POST /configurations/models/{projectID}
// answers when no writer is composed, and why.
//
// This route used to decode the body and answer 200 with an empty item list.
// It wrote nothing — not the configuration row, not the project vault. An
// administrator opened Settings > AI Configuration, chose a model, and got a
// success. The default stayed what it was.
//
// The real writer is NewCurrentModelDefaultRoute (model_default.go). The
// production router registers it only when the composition root supplies it,
// and the composition root builds it only under ELITEA_CONFIGURATIONS_ENABLED.
// The shipped chart sets that variable to "false", so a default install serves
// this handler and nothing else. A refusal is the honest answer there, and it
// names the variable that turns the capability on.
//
// This follows the TTSVoices decision (#466): a write route that reports
// success and stores nothing is worse than one that refuses, because the caller
// cannot tell the two apart.
//
// Both registrations stay. When the variable is on, the production router
// registers the writer's static path on the root router. Chi prefers that path
// over this mount. So only an unconfigured install reaches this body.
const modelDefaultUnavailable = "the default model cannot be set in this deployment. The route that writes a " +
	"project's default model is composed only when ELITEA_CONFIGURATIONS_ENABLED is true. This handler refuses " +
	"rather than reporting a success it did not perform."

// SetDefaultModel refuses. See modelDefaultUnavailable.
//
// The body is still decoded and bounded. So a malformed or oversized request is
// still a 400. It also still cannot be used to read the whole stream.
func (h *Handler) SetDefaultModel(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if !decodeBoundedJSON(w, r, &body) {
		return
	}
	apierr.WriteStatus(w, http.StatusServiceUnavailable, modelDefaultUnavailable)
}

func (h *Handler) ListTypes(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusOK, []TypeDescriptor{})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	displayNames := map[string]string{
		"llm_model":              "LLM Model",
		"embedding_model":        "Embedding Model",
		"asr_model":              "ASR Model",
		"tts_model":              "TTS Model",
		"image_generation_model": "Image Generation Model",
	}
	sectionMap := map[string]string{
		"llm_model":              "llm",
		"embedding_model":        "embedding",
		"asr_model":              "asr",
		"tts_model":              "tts",
		"image_generation_model": "image_generation",
	}

	q := fmt.Sprintf(`SELECT DISTINCT type, section FROM %s.configuration ORDER BY type`, schema)
	// Same rule as ListModels: only a missing schema is an empty list.
	rows, err := h.pool.Query(ctx, q)
	if err != nil {
		if configurationSchemaMissing(err) {
			writeJSON(w, http.StatusOK, []TypeDescriptor{})
			return
		}
		writeConfigurationQueryFailure(ctx, w, "configuration type list failed", projectID, err)
		return
	}
	defer rows.Close()

	descriptors := make([]TypeDescriptor, 0)
	for rows.Next() {
		var typeName, section string
		if err := rows.Scan(&typeName, &section); err != nil {
			continue
		}
		displayName := displayNames[typeName]
		if displayName == "" {
			displayName = typeName
		}
		if section == "" {
			section = sectionMap[typeName]
		}
		descriptors = append(descriptors, TypeDescriptor{
			Type:        typeName,
			DisplayName: displayName,
			Section:     section,
			Fields:      []interface{}{},
		})
	}

	writeJSON(w, http.StatusOK, descriptors)
}

// TTSVoices serves GET /configurations/tts_voices/{projectID} — the voices a
// TTS model can be asked for (issue 323).
//
// # What this replaces
//
// It answered 501 for every project, and the reason was true when it was
// written: nothing anywhere could ask a provider which voices it has, and no
// code path ever wrote the `meta.voices` cache the reference falls back on, so
// both of the reference's two sources were empty by construction. Answering 200
// with an empty list would have been worse than a refusal, because a caller
// cannot tell an empty cache from a route that does no work.
//
// Both sources exist now. The gateway serves the listing
// (elitea-llm-gateway internal/llmproxy/listprovidervoices.go) and this handler
// fills the cache from its answer, so an empty list here is a REAL answer about
// a provider and no longer a statement about this platform.
//
// # The resolution order is the reference's
//
// legacy/plugins/configurations/api/v2/tts_voices.py's `_resolve_voices`:
//
//  1. `refresh=true` asks the provider first and takes what it says.
//  2. Otherwise `meta.voices` on the project's tts configuration row.
//  3. Otherwise the provider, as an automatic fallback, and the answer is
//     cached on the row.
//
// # Why the provider TYPE is followed through the credential link
//
// A `tts_model` row does not name a provider; it links one, through
// `data.ai_credentials`, and the credential row's `type` is the dialect that
// decides the voice set. The reference resolves the same link
// (`expand_configuration` then `ai_credentials.configuration_type`). Only the
// TYPE is read here — no secret is redeemed, because the listing is a property
// of the dialect and not of the key.
//
// # Failure never becomes an empty list
//
// A gateway that cannot be reached is reported as a failure, not as a project
// with no voices: the second reads as a correct answer and would leave an
// operator looking for a provider problem that is not there.
func (h *Handler) TTSVoices(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	modelName := strings.TrimSpace(r.URL.Query().Get("model_name"))
	refresh := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("refresh")), "true")

	// No model, no voices. The reference logs and answers an empty list; a 400
	// would break the picker's own first render, which asks before a model is
	// chosen.
	if modelName == "" || h.pool == nil {
		writeTTSVoices(w, modelName, nil)
		return
	}

	row, found, err := h.ttsConfigurationRow(r.Context(), schema, modelName)
	if err != nil {
		writeConfigurationQueryFailure(r.Context(), w, "tts voice lookup failed", projectID, err)
		return
	}
	if !found {
		// A model name the project does not have is not an error: the picker
		// asks with whatever model is selected, and a stale selection is a
		// normal state.
		writeTTSVoices(w, modelName, nil)
		return
	}

	if !refresh && len(row.voices) > 0 {
		writeTTSVoices(w, modelName, row.voices)
		return
	}

	lister := h.providerVoiceLister()
	if lister == nil {
		// No gateway composed. Serve the cache if there is one — it is what
		// this deployment knows — and say nothing false about the provider.
		writeTTSVoices(w, modelName, row.voices)
		return
	}

	listing, err := lister.ListProviderVoices(r.Context(), row.providerType, modelName)
	if err != nil {
		slog.ErrorContext(r.Context(), "tts voice listing failed", "project_id", projectID, "err", err)
		if len(row.voices) > 0 {
			// A cached list is better than nothing while the gateway is down,
			// and it is what the operator saw a moment ago.
			writeTTSVoices(w, modelName, row.voices)
			return
		}
		apierr.WriteStatus(w, http.StatusBadGateway, "the voice listing is unavailable")
		return
	}

	if len(listing.Voices) == 0 {
		// The provider publishes no catalogue this build knows. The caller
		// keeps its own default voice; nothing is cached, because there is
		// nothing to cache and a stored empty list would look like a hit.
		writeTTSVoices(w, modelName, row.voices)
		return
	}

	h.cacheTTSVoices(r.Context(), schema, row.id, listing.Voices)
	writeTTSVoices(w, modelName, listing.Voices)
}

// ttsConfigurationRow is the project's tts row for one model name, plus the two
// things the listing needs from it.
type ttsConfigurationRow struct {
	id           int
	providerType string
	voices       []ProviderVoice
}

// ttsConfigurationRow reads the row and resolves the provider dialect.
func (h *Handler) ttsConfigurationRow(
	ctx context.Context, schema, modelName string,
) (ttsConfigurationRow, bool, error) {
	var (
		row       ttsConfigurationRow
		dataBytes []byte
		metaBytes []byte
	)
	query := fmt.Sprintf(`
		SELECT id, type, data, meta
		FROM %s.configuration
		WHERE elitea_title = $1 AND section = 'tts'
		ORDER BY id
		LIMIT 1
	`, schema)
	err := h.pool.QueryRow(ctx, query, modelName).Scan(&row.id, &row.providerType, &dataBytes, &metaBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || configurationSchemaMissing(err) {
			return ttsConfigurationRow{}, false, nil
		}
		return ttsConfigurationRow{}, false, err
	}

	var data map[string]any
	if len(dataBytes) > 0 {
		_ = json.Unmarshal(dataBytes, &data)
	}
	var meta map[string]any
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &meta)
	}
	row.voices = cachedVoicesFrom(meta)

	// A generic `tts_model` row names no dialect; the linked credential does.
	if strings.EqualFold(row.providerType, "tts_model") {
		if linked := h.credentialTypeFor(ctx, schema, credentialTitleOf(data)); linked != "" {
			row.providerType = linked
		}
	}
	return row, true, nil
}

// credentialTypeFor reads the TYPE of a linked ai_credentials row. No secret is
// read: the dialect decides the voice set, the key does not.
func (h *Handler) credentialTypeFor(ctx context.Context, schema, title string) string {
	if title == "" {
		return ""
	}
	var credentialType string
	query := fmt.Sprintf(`
		SELECT type FROM %s.configuration
		WHERE elitea_title = $1 AND section = 'ai_credentials'
		ORDER BY id
		LIMIT 1
	`, schema)
	if err := h.pool.QueryRow(ctx, query, title).Scan(&credentialType); err != nil {
		return ""
	}
	return credentialType
}

// cachedVoicesFrom reads `meta.voices`, dropping anything that is not a usable
// voice. A malformed cache is treated as no cache rather than as an error: the
// provider is asked again and the row is rewritten.
func cachedVoicesFrom(meta map[string]any) []ProviderVoice {
	raw, ok := meta["voices"].([]any)
	if !ok {
		return nil
	}
	voices := make([]ProviderVoice, 0, len(raw))
	for _, entry := range raw {
		item, isObject := entry.(map[string]any)
		if !isObject {
			continue
		}
		id, _ := item["id"].(string)
		name, _ := item["name"].(string)
		voices = append(voices, ProviderVoice{ID: id, Name: name})
	}
	return boundProviderVoices(voices)
}

// cacheTTSVoices writes the answer onto `meta.voices` — the cache the reference
// reads and that nothing in this platform used to fill.
//
// The write MERGES into the existing meta object rather than replacing it, so
// per-type bookkeeping another path stored there survives. A failure is logged
// and swallowed: the caller already has the voices, and refusing the read
// because the cache could not be written would turn a working listing into an
// error.
func (h *Handler) cacheTTSVoices(ctx context.Context, schema string, id int, voices []ProviderVoice) {
	encoded, err := json.Marshal(voices)
	if err != nil {
		return
	}
	query := fmt.Sprintf(`
		UPDATE %s.configuration
		SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('voices', $1::jsonb)
		WHERE id = $2
	`, schema)
	if _, err := h.pool.Exec(ctx, query, string(encoded), id); err != nil {
		slog.WarnContext(ctx, "tts voice cache write failed", "configuration_id", id, "err", err)
	}
}

// writeTTSVoices answers in the reference's exact shape: the voice array and
// the model name it was resolved for, always both, never null.
func writeTTSVoices(w http.ResponseWriter, modelName string, voices []ProviderVoice) {
	if voices == nil {
		voices = []ProviderVoice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": voices, "model_name": modelName})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode configurations response", "err", err)
	}
}

func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigurationRequestBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			apierr.WriteStatus(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return false
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}
