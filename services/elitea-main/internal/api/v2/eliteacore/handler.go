package eliteacore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return hex.EncodeToString(b)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

type Handler struct {
	pool               *pgxpool.Pool
	permissionResolver auth.PermissionResolver
	httpClient         *http.Client
	store              storage.ObjectStore
	// The pre-built MCP server catalogue and the vault holding its client
	// secrets (mcp_prebuilt_resolution.go). Both nil unless
	// WithPrebuiltMCPCatalogue is applied, in which case resolution is a no-op.
	prebuiltMCP      *mcpregistry.PrebuiltStore
	prebuiltMCPVault PrebuiltSecretReader
	delegatedAuth    DelegatedAuthToolkitSettingsResolver
	dcrClients       MCPDCRClients
	// Outbound e-mail for project invitations (users_write.go); nil means
	// the invite result reports no delivery.
	mailer InviteMailer
	// costBudgets says whether LLM cost tracking exists on this deployment, so
	// PlatformSettings can publish `cost_budgets_enabled`. See
	// WithCostBudgets for what it is derived from.
	costBudgets bool
	// events backs the agent.version.published / agent.version.unpublished
	// producers (#876's second half). nil leaves Publish/Unpublish exactly
	// as before.
	events EventEmitter
}

// EventEmitter is the seam to internal/events.Publisher, declared locally so
// this package does not import internal/events and close a cycle.
// *events.Publisher satisfies this structurally.
type EventEmitter interface {
	Emit(ctx context.Context, projectID, eventType string, payload any)
}

// WithEvents wires the agent.version.published and agent.version.unpublished
// producers.
//
// Guarded by eventEmitterPresent (see conversations.Handler.WithEvents'
// identical comment in that package): a nil *events.Publisher boxed into
// this interface parameter is non-nil, and unguarded would panic Publish's
// or Unpublish's first Emit call.
func WithEvents(emitter EventEmitter) Option {
	return func(handler *Handler) {
		if eventEmitterPresent(emitter) {
			handler.events = emitter
		}
	}
}

func eventEmitterPresent(emitter EventEmitter) bool {
	if emitter == nil {
		return false
	}
	v := reflect.ValueOf(emitter)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return !v.IsNil()
	default:
		return true
	}
}

// WithCostBudgets declares that this deployment tracks LLM cost, which is what
// `cost_budgets_enabled` means to a client.
//
// The reference answered the same question with an RPC into the LiteLLM plugin
// (`litellm_budgets_mode`, true for "observe" and "enforce") and the Usage tab
// was hidden when it was false. This platform's equivalent is whether the LLM
// gateway is composed: the gateway is what bills a call, publishes the budget
// delta and populates gateway.llm_budget_accumulators, so with no gateway
// address configured every spend figure the Usage tab renders is structurally
// zero and every budget row is authored against nothing.
//
// It is deliberately NOT the stronger claim "enforcement is on". Enforcement
// additionally needs GATEWAY_NATS_URL, and the gateway reports that itself
// through `GET /governance/status`, proxied at
// `GET /api/v2/admin/gateway/status`. Folding that into this flag would make an
// unreachable gateway hide a Usage tab whose accumulator history is still
// perfectly readable from PostgreSQL — a network fault silently removing a
// screenful of real data. The admin Budgets page reads the status route
// directly and warns there instead.
func WithCostBudgets(enabled bool) Option {
	return func(handler *Handler) {
		handler.costBudgets = enabled
	}
}

// InviteMailer is the seam to internal/application/mailer.
//
// Configured takes a context because outbound e-mail is configured at runtime
// (gap G7): the answer comes from `centry.platform_config` laid over the
// environment, so it is a read rather than a boot-time fact.
type InviteMailer interface {
	Configured(ctx context.Context) bool
	SendInvitation(ctx context.Context, invitation appmailer.Invitation) error
}

// WithInviteMailer supplies the composer project invites send through.
func WithInviteMailer(m InviteMailer) Option {
	return func(handler *Handler) {
		if m != nil {
			handler.mailer = m
		}
	}
}

type Option func(*Handler)

func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(handler *Handler) {
		handler.permissionResolver = resolver
	}
}

// WithHTTPClient configures the client used by the MCP OAuth and DCR proxies.
// It is primarily useful when the service needs a custom trusted CA bundle.
func WithHTTPClient(client *http.Client) Option {
	return func(handler *Handler) {
		if client != nil {
			handler.httpClient = client
		}
	}
}

// DelegatedAuthToolkitSettingsResolver materializes one actor-visible
// toolkit through Main's configuration and vault graph. Plaintext exists only
// for the outbound OAuth request and must never be logged or returned.
type DelegatedAuthToolkitSettingsResolver interface {
	ResolveDelegatedAuthToolkitSettings(
		context.Context,
		int32,
		int32,
		int32,
	) (toolkitexecutionapp.DelegatedAuthToolkitSettings, bool, error)
}

// WithDelegatedAuthToolkitSettingsResolver supplies the claim-mode settings
// resolver used by the OAuth proxy. A nil resolver leaves stored-credential
// fallback unavailable; caller-supplied and DCR credentials still work.
func WithDelegatedAuthToolkitSettingsResolver(resolver DelegatedAuthToolkitSettingsResolver) Option {
	return func(handler *Handler) {
		if resolver != nil {
			handler.delegatedAuth = resolver
		}
	}
}

// WithObjectStore wires S20b's icon byte path (UploadIcon/DeleteIcon) onto
// the object store instead of ICONS_DATA_DIR. Left nil, UploadIcon reports a
// clear 500 for a real upload attempt (never for its "no file" no-op) rather
// than silently falling back to disk; DeleteIcon stays unconditionally 204
// either way, matching its pre-S20b best-effort semantics.
func WithObjectStore(store storage.ObjectStore) Option {
	return func(handler *Handler) {
		handler.store = store
	}
}

func NewHandler(pool *pgxpool.Pool, opts ...Option) *Handler {
	handler := &Handler{
		pool:       pool,
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(handler)
	}
	return handler
}

func (h *Handler) PlatformSettings(w http.ResponseWriter, r *http.Request) {
	defaults := map[string]any{
		"chat_enabled":         true,
		"applications_enabled": true,
		"skills_enabled":       true,
		"toolkits_enabled":     true,
		"datasources_enabled":  true,
		"pipelines_enabled":    true,
		"publishing_enabled":   true,
		"moderation_enabled":   false,
		"mcp_enabled":          true,
		"support_chat_enabled": false,
	}

	projectID := chi.URLParam(r, "projectID")
	if h.pool != nil && projectID != "" {
		ctx := r.Context()
		s, schemaOK := tenantSchema(w, projectID)
		if !schemaOK {
			return
		}
		q := fmt.Sprintf(`SELECT data FROM %s.configuration WHERE type = 'environment_settings' LIMIT 1`, s)
		var data []byte
		if err := h.pool.QueryRow(ctx, q).Scan(&data); err == nil && len(data) > 0 {
			var dbSettings map[string]any
			if json.Unmarshal(data, &dbSettings) == nil {
				for k, v := range dbSettings {
					defaults[k] = v
				}
			}
		}
	}

	// The platform-wide MCP switch is applied LAST, after the per-project
	// overlay, and that ordering is the whole point of it.
	//
	// A project's `environment_settings` row can carry `mcp_enabled`, so a
	// global switch applied before it would be a kill switch any project could
	// re-open for itself — which is not what "master switch … across the entire
	// application" means, and not what the operator who turned it off believes
	// they did. Applied last it is a floor: globally off is off everywhere.
	// While globally on, a project may still disable MCP for itself, which is
	// the pre-existing behaviour and is unaffected.
	//
	// `mcp_in_menu_enabled` is added rather than overlaid: it is a NEW key, the
	// second half of the pair the reference's own platform_settings endpoint
	// returned (legacy/plugins/elitea_core/api/v2/platform_settings.py:45-46)
	// and the one apps/elitea-web's four `useIsMcpVisible` hooks each document
	// as missing from the wire. The schema declares
	// `additionalProperties: true`, so this is an addition the contract already
	// permits.
	mcp := h.mcpFlags(r.Context())
	if !mcp.enabled {
		defaults["mcp_enabled"] = false
	}
	defaults["mcp_in_menu_enabled"] = mcp.inMenu

	// The Voice Features pair, for the same reason and by the same route.
	// `widgets/chat/ui/chat-button/VoiceButton.tsx` hardcoded both of these as
	// module constants — `true` and `false` — so the admin switch named after
	// the control had no relationship to it. The button IS mounted: `/chat` →
	// `ChatBox` → `buildChatBoxInputSlots()` → `<VoiceButton>`.
	voice := h.voiceFlags(r.Context())
	defaults["voice_features_enabled"] = voice.enabled
	defaults["voice_features_temporarily_disabled"] = voice.temporarilyDisabled

	// The two publishing guardrails, so the product UI can gate its Publish
	// controls on the same answer the publish handlers enforce.
	//
	// Both pairs, because the reference publishes both here
	// (legacy/plugins/elitea_core/api/v2/platform_settings.py) and because the
	// pair a client is missing is the pair whose button it renders enabled into
	// a 403. `is_skill_publish_blocked` is the SKILL switch and is independent
	// of the agent one: `internal/api/v2/skillpublish` enforces the skill
	// section, `Publish` here enforces the agent section.
	//
	// The whitelists are published rather than a resolved boolean because the
	// UI needs them for the project it is IN, and this endpoint is already
	// per-project — a client that has both can also explain to a user why the
	// button is off, which "blocked: true" cannot.
	//
	// Added rather than overlaid, like the MCP and voice pairs above and under
	// the same `additionalProperties: true` permission: a project's own
	// `environment_settings` row must not be able to unblock a platform freeze
	// for itself.
	agentGuard := h.publishGuardrail(r.Context())
	defaults["is_publish_blocked"] = agentGuard.blocked
	defaults["publish_whitelist_project_ids"] = projectIDList(agentGuard.whitelist)
	skillGuard := h.skillPublishGuardrail(r.Context())
	defaults["is_skill_publish_blocked"] = skillGuard.blocked
	defaults["skill_publish_whitelist_project_ids"] = projectIDList(skillGuard.whitelist)

	// The guardrails blocklist, for the product UI to mark a toolkit blocked.
	//
	// This is the counterpart of a decision taken on the server: the toolkit
	// INSTANCE list is deliberately not filtered, so an administrator can still
	// see and delete toolkits of a type they have blocked rather than having
	// them vanish with their stored settings and vault references. A client that
	// is shown those rows needs to know which of them are blocked, or it renders
	// a toolkit as usable that no agent will run.
	//
	// It is the canonical keys that are published, not the operator's raw
	// strings. The client compares with the same normalisation
	// (`canonToolkitKey` in apps/elitea-web) and sending the raw values would
	// make correctness depend on the client repeating a rule it can only get
	// wrong — the list is for comparison, never for display.
	//
	// Added rather than overlaid, for the reason the MCP pair above is: the
	// schema declares `additionalProperties: true`, so this is an addition the
	// contract already permits and no spec edit follows.
	defaults["blocked_toolkits"] = h.blockedToolkits(r.Context())

	// The two platform-wide announcements. Added rather than overlaid, like the
	// MCP and voice pairs above and by the same contract permission
	// (`additionalProperties: true`): a per-project `environment_settings` row
	// must not be able to raise or silence a PLATFORM banner, nor claim the
	// platform is in maintenance when it is not.
	//
	// These are the only two keys here that are objects rather than scalars.
	// They are objects because each is a message plus the rules for showing it,
	// and flattening them into `banner_enabled` / `banner_message` / … would
	// scatter one setting across five sibling keys that a client would have to
	// know to reassemble.
	banner, maintenance := h.announcements(r.Context())
	defaults["dedicated_banner"] = banner
	defaults["maintenance"] = h.maintenancePayload(r.Context(), maintenance)

	// The Analytics visibility switch, ported off the withheld `observability`
	// section (config_schemas.go's "Observability, Runtime and Admin Panel are
	// GONE too" note) onto the Features page. Added under
	// `additionalProperties: true`, like the pairs above: apps/elitea-web's
	// Settings page hides its Analytics tab when this is false. The routes
	// themselves are gated independently, in internal/api/router.go, so a
	// client that ignores this key still meets a 403 rather than an open
	// endpoint behind a hidden button.
	defaults["analytics_enabled"] = h.analyticsFlags(r.Context()).enabled

	// The id of the PUBLIC project, published so a client stops guessing it.
	//
	// The same project was configured in three places that could not check each
	// other: `ELITEA_AI_PROJECT_ID` here, the identically named variable in the
	// LLM gateway, and `VITE_PUBLIC_PROJECT_ID` baked into the SPA image by
	// apps/elitea-web/docker-entrypoint.sh. Nothing compared the SPA's copy with
	// the server's, and one page did not even read the SPA's copy —
	// `pages/settings/ServicePrompts.tsx` compared the selected project against
	// the literal '1' and gated its query on the result, so on a deployment
	// whose public project is not id 1 the Service Prompts cards rendered empty
	// with no request made and no error shown.
	//
	// This endpoint is the right carrier: it is already ungated, already polled
	// by the app shell, and per-project, so the answer costs no extra request.
	// The SPA now prefers this value and keeps its build-time copy only as the
	// fallback for a deployment too old to send this key.
	//
	// Added rather than overlaid, like every pair above and under the same
	// contract permission (`additionalProperties: true`): a project's own
	// `environment_settings` row must not be able to tell a client that some
	// OTHER project is the public one.
	//
	// A NUMBER, not a string. `p_{id}` schema names, `model_project_id` in
	// llm_settings and the `project_id` in every entity_meta are numeric, and
	// the one client-side comparison that matters is against a project id the
	// SPA already holds as a string — one conversion at the edge is safer than
	// two representations on the wire.
	defaults["public_project_id"] = publicproject.ID()
	// The cost-budget switch, which gates Settings → Usage exactly as the
	// reference's own platform_settings endpoint gated it
	// (legacy/plugins/elitea_core/api/v2/platform_settings.py
	// `_is_cost_budgets_enabled`, true for LiteLLM budget modes "observe" and
	// "enforce"). Without it the tab had no way to know the platform keeps no
	// cost data, and rendered a page of structural zeroes as if they were
	// measurements.
	//
	// Added rather than overlaid, under the same `additionalProperties: true`
	// permission as the pairs above: a project's own `environment_settings` row
	// must not be able to claim cost tracking this deployment does not do.
	//
	// This is NOT a claim that enforcement is on. See WithCostBudgets.
	defaults["cost_budgets_enabled"] = h.costBudgets

	writeJSON(w, http.StatusOK, defaults)
}

// ProjectInfo REFUSES. It is reachable only when ELITEA_PROJECT_INFO_ENABLED
// is off, which is the default.
//
// # WHY THERE IS NOTHING TO SERVE HERE
//
// It used to answer 200 with `{"name": …, "icon_meta": null}`, and every part
// of that was a claim it had not measured:
//
//   - `icon_meta` was the literal nil on BOTH branches. The project's icon
//     lives in the tenant `configuration` row (type='project_icon',
//     elitea_title='project_icon_{N}'), which this handler never read. A
//     project with an icon selected was told, with a 200, that it had none.
//   - `teammates_count` was omitted entirely, and the web client turns the
//     absent key into a rendered 0 via `?? 0`
//     (apps/elitea-web/src/features/settings/ui/project-context/ProjectParamsHeader.tsx).
//     Every project displayed "Teammates: 0" — a counted figure the server had
//     never counted.
//   - a failed query took the SAME 200, with an empty name. A dropped
//     connection, a missing table and a project that genuinely has no name all
//     arrived at the browser indistinguishable from each other, and from
//     success.
//
// The real handler is internal/api/v2/projectinfo, which counts distinct
// project members and reads the icon row.
//
// When ELITEA_PROJECT_INFO_ENABLED is on, cmd/elitea-main composes
// projectinfo.CurrentProjectInfoRoute and internal/api/production_router.go
// registers it at this exact path. chi resolves that explicitly registered
// path ahead of the nested r.Route registration this handler sits behind, so
// the real route wins and this function is never reached there.
//
// 501 with a machine-readable `code`, not 500, for the reason
// internal/api/v2/analytics/handler.go:64-78 sets out: an absent producer is
// the server's final answer, and it will be the final answer to the next
// identical request too. apps/elitea-web/src/app/providers/queryClient.ts
// classifies 501 as final (`isFinalClientAnswer`) and does not retry it, so
// this refusal costs one request per screen rather than two.
func (h *Handler) ProjectInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": "project info is not available on this deployment",
		"code":  "project_info_not_available",
		"detail": "teammates_count and icon_meta are served by the reviewed " +
			"project-info route, which this deployment has not enabled " +
			"(ELITEA_PROJECT_INFO_ENABLED)",
	})
}

// UpdateProjectInfo writes what the caller sent. It used to read only
// body["name"] and answer `{"ok":true}` — while the web client's only caller
// of this route PUTs `{icon_meta}` and nothing else
// (apps/elitea-web/src/entities/project/api/projectContextApi.ts,
// updateProjectInfo). Selecting a project icon therefore returned success,
// invalidated the query, refetched, and rendered no icon, forever, with two
// 200s in the network tab.
//
// The icon is now written to the row projectinfo READS BACK — the same tenant
// `configuration` row, matched on all three of project_id, type and
// elitea_title (internal/api/v2/projectinfo/handler.go:113-124). The `data`
// shape is the one the create-time normalizer produces for this type
// (internal/application/configurations/local_configurations_normalizer.go:212),
// i.e. `{"icon_meta": {"name": …, "url": …}}` with either field nullable, or
// `{"icon_meta": null}` to clear it. Nothing here invents a URL.
//
// A write that fails is reported as a failure. The previous version discarded
// every Exec error and still answered `{"ok":true}`, which is the same defect
// in the other direction: the caller was told its edit had been saved when the
// database had refused it.
func (h *Handler) UpdateProjectInfo(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	ctx := r.Context()

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional; ignore EOF/empty-body errors

	name, renaming := body["name"].(string)
	renaming = renaming && name != ""

	// An absent key means "leave the icon alone". An explicit null means "clear
	// it" — the two are different requests and must not collapse, which is why
	// this tests presence rather than nil-ness.
	rawIcon, iconRequested := body["icon_meta"]

	// The caller's own mistake is answered before any server-side precondition,
	// so a malformed body gets 400 rather than being blamed on the database.
	var iconMeta map[string]any
	if iconRequested {
		var err error
		if iconMeta, err = normalizeProjectIconMeta(rawIcon); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "icon_meta must be null or an object with string name/url",
				"code":  "invalid_icon_meta",
			})
			return
		}
	}

	// The pool is only a precondition for a request that actually asks for a
	// write. A PUT with neither field is a no-op, and answering it 500 would
	// invent a failure the same way the old code invented a success.
	if (renaming || iconRequested) && h.pool == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": projectInfoWriteFailed,
			"code":  "project_info_write_failed",
		})
		return
	}

	response := map[string]any{"ok": true}

	if renaming {
		if _, err := h.pool.Exec(
			ctx,
			`UPDATE centry.project SET name = $1 WHERE id = $2`,
			name, projectID,
		); err != nil {
			slog.ErrorContext(ctx, "update project info: rename failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": projectInfoWriteFailed,
				"code":  "project_info_write_failed",
			})
			return
		}
		response["name"] = name
	}

	if !iconRequested {
		writeJSON(w, http.StatusOK, response)
		return
	}
	if err := h.writeProjectIcon(ctx, w, projectID, iconMeta); err != nil {
		return // writeProjectIcon has already answered.
	}
	response["icon_meta"] = iconMeta
	writeJSON(w, http.StatusOK, response)
}

const projectInfoWriteFailed = "failed to save project info"

// normalizeProjectIconMeta mirrors normalizeCurrentLocalProjectIcon in
// internal/application/configurations — the create-time normalizer for
// type='project_icon' — rather than importing it, because that function is
// unexported and takes/returns the whole configuration `data` object. Keeping
// the shape here identical to that one is what makes a row written by this
// route indistinguishable from a row written through the configurations API.
//
// A nil return means an explicit null: clear the icon.
func normalizeProjectIconMeta(raw any) (map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	icon, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("icon_meta must be an object or null")
	}
	normalized := make(map[string]any, 2)
	for _, field := range [...]string{"name", "url"} {
		value, present := icon[field]
		if !present || value == nil {
			normalized[field] = nil
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("icon_meta.%s must be a string", field)
		}
		normalized[field] = text
	}
	return normalized, nil
}

// writeProjectIcon upserts the one tenant configuration row
// internal/api/v2/projectinfo reads. It answers the request itself on failure
// and returns a non-nil error to tell the caller it has done so.
//
// project_id, type and elitea_title are all set because the read matches on
// all three; a row missing any of them reads back as "no icon" and the write
// would be invisible again. `section` is 'project_settings' — the section the
// configurations catalogue records for this type
// (internal/application/configurations/current_available_snapshot.json).
//
// ON CONFLICT targets elitea_title alone: that is the column's own UNIQUE
// constraint (internal/infra/db/migrations/001_initial.sql). A partial
// conflict target (`... WHERE type = 'project_icon'`) needs a matching partial
// unique index, which this table does not have.
func (h *Handler) writeProjectIcon(
	ctx context.Context,
	w http.ResponseWriter,
	projectID string,
	iconMeta map[string]any,
) error {
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return errors.New("invalid project id")
	}
	numericProjectID, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || numericProjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return errors.New("invalid project id")
	}

	// json.Marshal of a map[string]any built above cannot fail.
	data, _ := json.Marshal(map[string]any{"icon_meta": iconMeta})
	title := fmt.Sprintf("project_icon_%d", numericProjectID)

	query := fmt.Sprintf(`
		INSERT INTO %s.configuration
			(project_id, label, elitea_title, type, section, data, status_ok, created_at)
		VALUES ($1, 'Project Icon', $2, 'project_icon', 'project_settings', $3, true, NOW())
		ON CONFLICT (elitea_title) DO UPDATE
			SET data = EXCLUDED.data, updated_at = NOW()`, schema)
	if _, err := h.pool.Exec(ctx, query, numericProjectID, title, data); err != nil {
		slog.ErrorContext(ctx, "update project info: icon write failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": projectInfoWriteFailed,
			"code":  "project_info_write_failed",
		})
		return err
	}
	return nil
}

func (h *Handler) SearchOptions(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	q := fmt.Sprintf(`SELECT name FROM %s.tags ORDER BY name`, s)
	rows, err := h.pool.Query(ctx, q)

	tags := make([]string, 0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil {
				continue
			}
			tags = append(tags, name)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "collections": []any{}})
}

// usersCountQuery and usersPageQuery list the members of one project, plus the
// central platform administrators.
//
// THE MODE PREDICATE IS LOAD-BEARING. A role name alone does not identify a
// platform administrator. auth_core__role is UNIQUE (name, mode), so a legacy
// database carries a `super_admin` role in the `default` and `developer` modes
// as well. Only the `administration` mode grants central access. Without
// `r.mode = 'administration'` every holder of a same-named role joined every
// project's member list as a phantom member.
//
// They are package constants rather than locals for a second reason:
// scripts/validate_contract_static.sh reads only the first 80 lines of the
// Users function to find its writeJSON call. Inline, the two queries pushed
// that call to line 81 and the gate reported the response shape as unmeasured.
const usersCountQuery = `
			SELECT COUNT(*) FROM (
				SELECT au.id FROM auth_core__user au
				JOIN auth_core__project_user_role pur ON pur.user_id = au.id AND pur.project_id = $1
				WHERE au.email NOT LIKE '%@centry.user'
			UNION
				SELECT au.id FROM auth_core__user au
				JOIN auth_core__user_role ur ON ur.user_id = au.id
				JOIN auth_core__role r ON r.id = ur.role_id
				WHERE r.name = 'super_admin' AND r.mode = 'administration'
					AND au.email NOT LIKE '%@centry.user'
			) combined
		`

// usersPageQuery is usersCountQuery's page: project roles for a member, and the
// literal `super_admin` for a central administrator who is not one.
const usersPageQuery = `
			SELECT id, email, name, roles FROM (
				SELECT au.id, au.email, COALESCE(au.name, '') as name,
					COALESCE(array_agg(pr.name) FILTER (WHERE pr.name IS NOT NULL), ARRAY['viewer']) as roles
				FROM auth_core__user au
				JOIN auth_core__project_user_role pur ON pur.user_id = au.id AND pur.project_id = $1
				LEFT JOIN auth_core__project_role pr ON pr.id = pur.role_id
				WHERE au.email NOT LIKE '%@centry.user'
				GROUP BY au.id, au.email, au.name
			UNION
				SELECT au.id, au.email, COALESCE(au.name, '') as name,
					ARRAY['super_admin'] as roles
				FROM auth_core__user au
				JOIN auth_core__user_role ur ON ur.user_id = au.id
				JOIN auth_core__role r ON r.id = ur.role_id
				WHERE r.name = 'super_admin' AND r.mode = 'administration'
					AND au.email NOT LIKE '%@centry.user'
					AND au.id NOT IN (
						SELECT pur2.user_id FROM auth_core__project_user_role pur2 WHERE pur2.project_id = $1
					)
			) combined
			ORDER BY name, id
			LIMIT $2 OFFSET $3
		`

func (h *Handler) Users(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	ctx := r.Context()

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 20
	}

	items := make([]map[string]any, 0)
	total := 0

	if pidNum, err := strconv.Atoi(projectID); err == nil && pidNum > 0 {
		_ = h.pool.QueryRow(ctx, usersCountQuery, pidNum).Scan(&total) // failure leaves total=0, which is safe

		rows, err := h.pool.Query(ctx, usersPageQuery, pidNum, limit, offset)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int
				var email, name string
				var roles []string
				if rows.Scan(&id, &email, &name, &roles) != nil {
					continue
				}
				if roles == nil {
					roles = []string{"viewer"}
				}
				items = append(items, map[string]any{
					"id": fmt.Sprintf("%d", id), "email": email, "name": name, "roles": roles,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"rows": items, "total": total})
}

func (h *Handler) Roles(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	ctx := r.Context()

	items := make([]map[string]any, 0)

	if _, err := strconv.Atoi(projectID); err == nil {
		q := `SELECT id, name FROM auth_core__project_role WHERE project_id = $1 ORDER BY id`
		rows, err := h.pool.Query(ctx, q, projectID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int
				var name string
				if rows.Scan(&id, &name) != nil {
					continue
				}
				items = append(items, map[string]any{"id": fmt.Sprintf("%d", id), "name": name})
			}
		}
	}

	// If no project-specific roles, return global defaults
	if len(items) == 0 {
		items = []map[string]any{
			{"id": "1", "name": "admin"},
			{"id": "2", "name": "editor"},
			{"id": "3", "name": "viewer"},
		}
	}

	// UI roleList query expects a plain array (roles.map(...))
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) ChatConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	// Get configured LLM integrations as available models
	q := fmt.Sprintf(`
		SELECT elitea_title, type, data
		FROM %s.configuration
		WHERE section = 'llm' AND status_ok = true
		ORDER BY created_at`, s)

	rows, err := h.pool.Query(ctx, q)

	models := make([]map[string]any, 0)
	defaultModel := ""
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var title, cType string
			var data []byte
			if rows.Scan(&title, &cType, &data) != nil {
				continue
			}
			models = append(models, map[string]any{
				"name":        title,
				"config_type": cType,
			})
			if defaultModel == "" {
				defaultModel = title
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":        models,
		"default_model": defaultModel,
	})
}

func (h *Handler) Notifications(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	ctx := r.Context()

	user, ok := auth.UserFromContext(ctx)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"notifications": []any{}, "total": 0})
		return
	}

	rows, err := h.pool.Query(ctx, `
		SELECT id, uuid, is_seen, meta, event_type, created_at
		FROM centry.notifications
		WHERE project_id = $1 AND user_id = $2
		ORDER BY created_at DESC
		LIMIT 50`, projectID, user.ID)

	items := make([]map[string]any, 0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var uuid, eventType string
			var isSeen bool
			var meta []byte
			var createdAt interface{}
			if rows.Scan(&id, &uuid, &isSeen, &meta, &eventType, &createdAt) == nil {
				var metaObj any
				_ = json.Unmarshal(meta, &metaObj) // meta is a DB jsonb column; malformed means nil metaObj
				items = append(items, map[string]any{
					"id": fmt.Sprintf("%d", id), "uuid": uuid, "is_seen": isSeen,
					"meta": metaObj, "event_type": eventType, "created_at": createdAt,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": items, "total": len(items)})
}

func (h *Handler) Author(w http.ResponseWriter, r *http.Request) {
	authorID := chi.URLParam(r, "authorID")
	ctx := r.Context()

	var name, email, avatar, desc string
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(au.name, ''), COALESCE(au.email, ''), COALESCE(su.avatar, ''), COALESCE(su.description, '')
		FROM auth_core__user au
		LEFT JOIN centry.social_users su ON su.user_id = au.id
		WHERE au.id = $1
	`, authorID).Scan(&name, &email, &avatar, &desc)

	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	// Count applications owned by this author — only in projects they belong to
	var totalApps, totalPipelines, totalToolkits, totalCollections int
	schemaRows, _ := h.pool.Query(ctx,
		`SELECT DISTINCT 'p_' || project_id FROM auth_core__project_user_role WHERE user_id = $1`, authorID)
	if schemaRows != nil {
		var schemas []string
		for schemaRows.Next() {
			var s string
			if schemaRows.Scan(&s) != nil {
				continue
			}
			schemas = append(schemas, s)
		}
		schemaRows.Close()
		for _, name := range schemas {
			s := catalogueSchema(name)
			var cnt int
			// Each Scan failure leaves cnt=0, which is safe for counting
			// The author of an agent is the author of its VERSIONS.
			// `applications.owner_id` is the owning PROJECT (#533), so
			// `a.owner_id = $1` counted the agents of the project whose id
			// happens to equal this user id — a different set, and usually an
			// empty one.
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.applications a WHERE EXISTS (SELECT 1 FROM %s.application_versions av WHERE av.application_id = a.id AND av.author_id = $1) AND NOT EXISTS (SELECT 1 FROM %s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline')`, s, s, s), authorID).Scan(&cnt)
			totalApps += cnt
			cnt = 0
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.applications a WHERE EXISTS (SELECT 1 FROM %s.application_versions av WHERE av.application_id = a.id AND av.author_id = $1) AND EXISTS (SELECT 1 FROM %s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline')`, s, s, s), authorID).Scan(&cnt)
			totalPipelines += cnt
			cnt = 0
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.elitea_tools WHERE author_id = $1`, s), authorID).Scan(&cnt)
			totalToolkits += cnt
			cnt = 0
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.prompt_collections WHERE author_id = $1`, s), authorID).Scan(&cnt)
			totalCollections += cnt
		}
	}

	aid, _ := strconv.Atoi(authorID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": aid, "name": name, "email": email,
		"avatar": avatar, "title": "", "description": desc,
		"total_conversations": 0, "public_conversations": 0,
		"public_applications": 0, "total_applications": totalApps,
		"public_pipelines": 0, "total_pipelines": totalPipelines,
		"total_toolkits": totalToolkits, "public_collections": 0,
		"total_collections": totalCollections, "rewards": 0,
	})
}

// publishActorID names the user a published version is attributed to, and
// reports whether there is one to name.
//
// The acting principal first, the draft's own author as the fallback — the
// same rule the skill publish path applies (internal/api/v2/skillpublish's
// actingUserID). A token principal that carries no owner resolves to no
// principal at all, and then the author is the honest answer: somebody
// published this version, and the only identity the request carries is the one
// stored on the row.
//
// The `false` return matters. A version whose author column holds nothing
// leaves the key OUT of the meta bag, so the dashboard reports `null` — "this
// platform does not know who published it" — rather than user 0, which reads
// as a real account and belongs to nobody.
func publishActorID(ctx context.Context, fallbackAuthorID int) (int64, bool) {
	if actor, ok := importPrincipalUserID(ctx); ok {
		return actor.Int64(), true
	}
	if fallbackAuthorID > 0 {
		return int64(fallbackAuthorID), true
	}
	return 0, false
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionID")
	ctx := r.Context()

	// The publishing guardrail, enforced.
	//
	// Until this unit the admin Features page offered "Block Agent Publishing"
	// and this — the only publish path in the service — never asked. The switch
	// was a form control over a value nothing read, which on a guardrail is
	// worse than a missing feature: an operator who blocked publishing during an
	// incident would have been told it was blocked while every publish kept
	// succeeding.
	//
	// Checked FIRST, before the body is decoded and before the version is
	// looked up. A refusal that arrives after validation leaks which version
	// ids exist and whether their names collide, to a caller the platform has
	// just decided may not publish at all.
	if guard := h.publishGuardrail(ctx); !guard.allows(parseProjectID(projectID)) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "publishing is blocked on this deployment",
		})
		return
	}

	// The tenant schema is resolved AFTER the guardrail, and the order matters.
	// A blocked deployment must answer 403 for every caller, including one who
	// sends a project id this service cannot parse. Resolving the schema first
	// answers such a caller with 400, which tells them their id was the problem
	// and not the block — and it makes "is publishing blocked?" answerable by
	// the shape of the URL. TestGuardrailRefusesAProjectIdItCannotParse pins
	// this order.
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	var body struct {
		VersionName     string `json:"version_name"`
		ValidationToken string `json:"validation_token"`
		Category        string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if body.VersionName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": []map[string]any{
				{"loc": []string{"body", "version_name"}, "msg": "field required", "type": "value_error.missing"},
			},
		})
		return
	}

	// Validate version name: only alphanumeric, hyphens, underscores, dots
	for _, c := range body.VersionName {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": []map[string]any{
					{"loc": []string{"body", "version_name"}, "msg": "string does not match regex \"^[a-zA-Z0-9._-]+$\"", "type": "value_error.str.regex"},
				},
			})
			return
		}
	}

	// Verify version exists and is not 'base'
	//
	// `author_id` rides along for the attribution the published row carries.
	// It is the FALLBACK, not the answer: the person who publishes an agent
	// need not be the person who wrote the draft, so the acting principal wins
	// where there is one (publishActorID below).
	var appID int
	var vName, vStatus, agentType string
	var vAuthorID int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id, name, status, COALESCE(agent_type, ''), COALESCE(author_id, 0) FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&appID, &vName, &vStatus, &agentType, &vAuthorID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	}
	if agentType == "pipeline" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pipeline_not_publishable", "msg": "pipeline agents cannot be published"})
		return
	}
	if vStatus == "published" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "version is already published"})
		return
	}

	// Pre-check: does a version with this name already exist for this application?
	var nameExists bool
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND name = $2)`, s),
		appID, body.VersionName).Scan(&nameExists) // failure leaves nameExists=false, safe to continue
	if nameExists {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "validation_failed",
			"validation_result": map[string]any{
				"issues": []map[string]any{
					{"rule": "version_name_exists_in_source", "field": "version_name", "issue": "version name already exists", "source": "deterministic"},
				},
			},
		})
		return
	}

	// Validate category if provided
	validCategories := map[string]bool{
		"Business Analyst": true, "Quality Assurance": true, "Development": true,
		"DevOps": true, "Project Management": true, "Knowledge & Documentation": true,
		"Elitea": true, "Epam": true, "Other": true,
	}
	if body.Category != "" && !validCategories[body.Category] {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "validation_failed",
			"validation_result": map[string]any{
				"issues": []map[string]any{
					{"rule": "invalid_category", "field": "category", "issue": "unknown category", "source": "deterministic"},
				},
			},
		})
		return
	}

	// Hard-check: the model must come from the public project (runs before
	// validation).
	//
	// ONE project, resolved once. This used to accept `PUBLIC_PROJECT_ID`
	// (default 1) OR `SHARED_PROJECT_ID` (default 4), which made the guard
	// admit a second project no other surface knew about and that the
	// reference does not have: legacy `_check_shared_llm` compares against a
	// single `public_project_id`. Both names still resolve, as deprecated
	// aliases of ELITEA_AI_PROJECT_ID — see internal/publicproject.
	publicProjID := publicproject.IDString()
	var llmSettingsStr *string
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT llm_settings::text FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&llmSettingsStr) // failure leaves nil, safe
	if llmSettingsStr != nil {
		var llmSettings map[string]any
		_ = json.Unmarshal([]byte(*llmSettingsStr), &llmSettings) // DB jsonb column; malformed means empty map
		if modelProjID, ok := llmSettings["model_project_id"]; ok && modelProjID != nil {
			mpid := fmt.Sprintf("%v", modelProjID)
			if mpid != publicProjID {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "llm_not_shared"})
				return
			}
			// The model IS the catalogue's — and the catalogue's own models are
			// no longer one set. A platform model carries a GRANT, and a
			// published agent is offered to every project that reads the
			// catalogue, so it may only name a model granted to all of them.
			// Same code, because it is the same finding; the `msg` names the
			// scope that was found. See publish_model_grant.go.
			if refusal := h.publishGrantRefusal(ctx, llmSettings); refusal != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "llm_not_shared", "msg": refusal,
				})
				return
			}
		}
	}

	// Validation gate: if no token provided, run inline validation
	if body.ValidationToken == "" {
		valResult, _ := h.runPublishValidation(ctx, s, versionID, body.VersionName)
		if valResult != nil && valResult["status"] == "FAIL" {
			criticals, _ := valResult["critical_issues"].([]map[string]any)
			issues := make([]map[string]any, len(criticals))
			for i, c := range criticals {
				issues[i] = map[string]any{"rule": c["field"], "message": c["issue"]}
				if r, ok := c["rule"]; ok {
					issues[i]["rule"] = r
				}
			}
			valResult["issues"] = issues
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":             "validation_failed",
				"validation_result": valResult,
			})
			return
		}
	} else {
		// The token is VERIFIED, not merely shaped.
		//
		// This used to accept any string of sixteen or more lowercase
		// hexadecimal characters, which made the quality gate optional for
		// anyone willing to type one: the token was never recorded, never
		// bound to a version and never looked up (issue 855). It is now a
		// signed grant that names the version it was issued for and carries
		// its own expiry — see publish_validation_token.go.
		if verdict := verifyPublishValidationToken(body.ValidationToken, versionID,
			h.publishTokenContentHash(ctx, s, versionID), time.Now()); verdict != publishTokenValid {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "validation_token_invalid",
				"msg":   publishTokenRefusal(verdict),
			})
			return
		}
	}

	// Clone: insert a new version with status 'published' using same application_id
	//
	// The overlay is MARSHALLED rather than formatted. It used to be built with
	// fmt.Sprintf, which put a caller-supplied category straight inside a JSON
	// string literal; the category is whitelisted above, so nothing could reach
	// it, but a second key added later would not have been.
	//
	// `published_by` is the key the operator's published-agents dashboard reads
	// (admin_published_agents.go). The SKILL publish path has always written it
	// (internal/api/v2/skillpublish/publish.go); this path wrote only
	// `source_version_id` and `category`, so the dashboard's "published by"
	// column was null for every agent on every deployment, permanently, and an
	// operator could not tell an unattributed publish from an unmeasured one.
	// It rides into the catalogue twin as well, because the twin copies this
	// row's meta (catalog_mirror.go).
	var cloneID int
	overlay := map[string]any{"source_version_id": versionID}
	if publishedBy, ok := publishActorID(ctx, vAuthorID); ok {
		overlay["published_by"] = publishedBy
	}
	if body.Category != "" {
		overlay["category"] = body.Category
	}
	metaOverlayJSON, overlayErr := json.Marshal(overlay)
	if overlayErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
		return
	}
	metaOverlay := string(metaOverlayJSON)
	cloneQ := fmt.Sprintf(`
		INSERT INTO %s.application_versions
			(application_id, name, status, author_id, llm_settings, instructions,
			 conversation_starters, welcome_message, agent_type, meta, pipeline_settings)
		SELECT application_id, $2, 'published', author_id, llm_settings, instructions,
			   conversation_starters, welcome_message, agent_type,
			   COALESCE(meta, '{}'::jsonb) || $3::jsonb,
			   pipeline_settings
		FROM %s.application_versions WHERE id = $1
		RETURNING id`, s, s)

	// A publish is all-or-nothing, for the reason ForkAgent is
	// (internal/api/oapiserver/publishing.go). The version clone and the two
	// attachment copies below run in one transaction. The tool copy used to run on
	// the pool with its error discarded, so a failed copy published a version and
	// answered 200. The user then saw a published agent that had lost every
	// toolkit, and nothing was logged. A published shell is worse than a refusal
	// the caller can retry, because the caller cannot see that it must retry.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has run

	err = tx.QueryRow(ctx, cloneQ, versionID, body.VersionName, metaOverlay).Scan(&cloneID)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "_application_version_name_uc") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "validation_failed",
				"validation_result": map[string]any{
					"issues": []map[string]any{
						{"rule": "version_name_exists_in_source", "field": "version_name", "issue": "version name already exists", "source": "deterministic"},
					},
				},
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Clone entity_tool_mapping rows from source version to new published version.
	// The copy keeps the source `entity_type` and `entity_id`: the clone above
	// selects the source `application_id`, so the published version belongs to the
	// same agent, and the chat read joins `entity_id` to that application
	// (internal/db/queries/agent_chat.sql:107).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
		SELECT $2, entity_id, entity_type, tool_id, selected_tools
		FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, s, s), versionID, cloneID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent tool attachments"})
		return
	}

	// Clone entity_skill_mapping rows from source version to new published version.
	// Publish copied tool mappings only, so a published agent carried no skills
	// (#351). Fork sets the precedent it now matches
	// (internal/api/oapiserver/publishing.go:149).
	//
	// The table has no `entity_id` column (001_initial.sql:422-432): a skill
	// attachment is keyed by (entity_version_id, skill_id, entity_type) alone.
	// `entity_type` is carried from the source row rather than defaulted, because
	// it is part of that key and the chat read matches on it
	// (internal/db/queries/agent_chat.sql:132). `skill_version_id` rides along
	// because the same read LEFT JOINs it for the skill instructions — dropping it
	// publishes a named skill with an empty body.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		SELECT $2, entity_type, skill_id, skill_version_id
		FROM %s.entity_skill_mapping WHERE entity_version_id = $1`, s, s), versionID, cloneID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent skill attachments"})
		return
	}

	// The catalogue twin, in the SAME transaction as the clone.
	//
	// Without it a publish from any project other than the public one wrote a
	// `published` row into a schema ELITEA Catalog never reads: the Published
	// tab listed the agent and the catalogue stayed empty, with no error on
	// either side. See catalog_mirror.go for why the twin carries no tool or
	// skill attachments.
	twin, mirrorErr := mirrorPublishedVersion(ctx, tx, s, projectID, appID, cloneID, body.VersionName, body.Category)
	if mirrorErr != nil {
		if errors.Is(mirrorErr, errCatalogVersionNameTaken) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "validation_failed",
				"validation_result": map[string]any{
					"issues": []map[string]any{
						{"rule": "version_name_exists_in_catalog", "field": "version_name", "issue": "version name already published to the catalog", "source": "deterministic"},
					},
				},
			})
			return
		}
		slog.ErrorContext(ctx, "publish: catalog mirror failed", "schema", s, "version_id", cloneID, "error", mirrorErr)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent to the catalog"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to publish agent"})
		return
	}

	// Embed sub-agents: clone application_tools of type 'application' recursively
	h.embedSubAgents(ctx, s, projectID, versionID, cloneID)

	// `public_agent_id` and `public_version_id` name the rows in the AUTHOR's
	// schema, which is what they have always named and what
	// publish_tool_copy_postgres_integration_test.go and
	// publish_skill_copy_postgres_integration_test.go read them for. The
	// catalogue rows are a different pair, so they get their own two keys
	// rather than a changed meaning of these.
	//
	// The catalogue keys are OMITTED when there is no twin — that is, when the
	// author already stands in the public project and the clone above IS the
	// catalogue row. A zero would read as "the catalogue has row 0".
	// `source_version_id` names the DRAFT this publish was made from, and it
	// agrees with the key of the same name in the published row's own stored
	// meta (the overlay above). It used to repeat `public_version_id` — the id
	// of the row this request had just created — so a client following the
	// response back to the version the author keeps editing arrived at the
	// published copy instead, and the two answers to "where did this come
	// from?" disagreed inside one response.
	response := map[string]any{
		"public_agent_id":   strconv.Itoa(appID),
		"public_version_id": strconv.Itoa(cloneID),
		"version_name":      body.VersionName,
		"source_version_id": versionID,
	}
	if twin.VersionID != 0 {
		response["catalog_agent_id"] = strconv.Itoa(twin.ApplicationID)
		response["catalog_version_id"] = strconv.Itoa(twin.VersionID)
	}
	if h.events != nil {
		h.events.Emit(ctx, projectID, "agent.version.published", map[string]any{
			"application_id":     appID,
			"published_agent_id": cloneID,
			"source_version_id":  versionID,
			"version_name":       body.VersionName,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// deleteEmbeddedSubAgents removes the EMBEDDED COPIES a publish made under
// versionID. It does not touch the author's own agents.
//
// THE DISTINCTION IS THE WHOLE FUNCTION. A published version carries TWO
// sub-agent references for each sub-agent its draft had: the mapping the
// publish copied, which still names the AUTHOR'S OWN child agent, and the link
// to the private copy `embedSubAgents` then made. This walked both and deleted
// every application either of them named — so unpublishing a parent, or
// deleting it, destroyed the child agent the author still had open in the
// list. Nothing reported it: the deletes are best-effort by design, and the
// author's next read of that agent was a plain 404.
//
// `status = 'embedded'` is the marker `embedSubAgentsRecursive` writes on the
// copy it creates, and the same marker the recursion below already reads, so
// the two halves of this function now agree on what an embedded copy is. A
// reference whose version cannot be read leaves the application alone: an
// orphaned copy costs storage, and the alternative costs somebody their agent.
func (h *Handler) deleteEmbeddedSubAgents(ctx context.Context, schema string, versionID string) {
	refs, err := listApplicationToolReferences(ctx, h.pool, schema, versionID)
	if err != nil {
		return
	}
	var embeddedAppIDs []string
	for _, ref := range refs {
		if ref.ApplicationID == "" || ref.VersionID == "" {
			continue
		}
		var refStatus string
		if statusErr := h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(status, '') FROM %s.application_versions WHERE id = $1`, schema),
			ref.VersionID).Scan(&refStatus); statusErr != nil {
			continue
		}
		if refStatus != "embedded" {
			continue
		}
		embeddedAppIDs = append(embeddedAppIDs, ref.ApplicationID)
	}

	for _, eAppID := range embeddedAppIDs {
		// ONLY an application that really holds an embedded version may be
		// deleted here, and the read below is the proof — not a lookup for the
		// recursion alone.
		//
		// A published version carries TWO application references per sub-agent,
		// not one. Publish copies the source version's entity_tool_mapping rows
		// onto the clone verbatim, which brings the author's own reference
		// across, and embedSubAgents then links the embedded copy beside it. So
		// the list above holds the author's private sub-agent as well as the
		// embedded snapshot, and the unguarded delete that used to follow
		// removed the author's agent — every version of it — the moment they
		// withdrew the parent. The agent simply vanished from their project,
		// with a 200 on the withdrawal and nothing in the log.
		//
		// `status = 'embedded'` is the discriminator because embedSubAgents
		// writes exactly that status on every copy it makes, at every level, and
		// nothing else does.
		var eVerID string
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT id FROM %s.application_versions WHERE application_id = $1 AND status = 'embedded' LIMIT 1`, schema), eAppID).Scan(&eVerID) // failure leaves eVerID empty, safe
		if eVerID == "" {
			continue
		}
		// Recursively delete sub-agents of this embedded agent
		h.deleteEmbeddedSubAgents(ctx, schema, eVerID)
		// Delete in FK-safe order: tool references → versions → application.
		// The reference cleanup walks the embedded app's versions so each
		// mapping is removed before its tool row is considered orphaned.
		versionRows, versionErr := h.pool.Query(ctx, fmt.Sprintf(
			`SELECT id FROM %s.application_versions WHERE application_id = $1`, schema), eAppID)
		if versionErr == nil {
			var embeddedVersionIDs []string
			for versionRows.Next() {
				var id string
				if versionRows.Scan(&id) == nil {
					embeddedVersionIDs = append(embeddedVersionIDs, id)
				}
			}
			versionRows.Close()
			for _, embeddedVersionID := range embeddedVersionIDs {
				_, _ = deleteApplicationToolReferences(ctx, h.pool, schema, embeddedVersionID, "", "")
			}
		}
		_, _ = h.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.application_versions WHERE application_id = $1`, schema), eAppID)
		_, _ = h.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.applications WHERE id = $1`, schema), eAppID)
	}
	// Clean up the sub-agent references on this version itself
	_, _ = deleteApplicationToolReferences(ctx, h.pool, schema, versionID, "", "")
}

// embedSubAgents clones application-type tools from sourceVersionID onto targetVersionID.
// For each sub-agent tool, it creates a new embedded application+version, copies the
// tool and skill attachments of that sub-agent onto the embedded version, and links it.
func (h *Handler) embedSubAgents(ctx context.Context, schema, projectID string, sourceVersionID string, targetVersionID int) {
	h.embedSubAgentsRecursive(ctx, schema, projectID, sourceVersionID, targetVersionID, 0)
}

func (h *Handler) embedSubAgentsRecursive(ctx context.Context, schema, projectID string, sourceVersionID string, targetVersionID int, depth int) {
	if depth > 5 {
		return
	}

	// Look up the parent published app ID (the application that owns targetVersionID)
	var parentAppID int
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id FROM %s.application_versions WHERE id = $1`, schema), targetVersionID).Scan(&parentAppID) // failure leaves parentAppID=0

	refs, err := listApplicationToolReferences(ctx, h.pool, schema, sourceVersionID)
	if err != nil {
		slog.ErrorContext(ctx, "embed_sub_agents: sub-agent reference read failed",
			"schema", schema, "source_version_id", sourceVersionID, "err", err)
		return
	}

	for _, ref := range refs {
		// Skip pipeline sub-agents — they cannot be published/embedded
		var subAgentType string
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(agent_type, '') FROM %s.application_versions WHERE id = $1`, schema), ref.VersionID).Scan(&subAgentType) // failure leaves subAgentType empty
		if subAgentType == "pipeline" {
			continue
		}

		// Clone the sub-agent application
		var embeddedAppID int
		err = h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.applications (name, description, owner_id)
			SELECT name, description, owner_id
			FROM %s.applications WHERE id = $1
			RETURNING id`, schema, schema), ref.ApplicationID).Scan(&embeddedAppID)
		if err != nil {
			slog.ErrorContext(ctx, "embed_sub_agents: sub-agent application clone failed",
				"schema", schema, "source_application_id", ref.ApplicationID,
				"parent_version_id", targetVersionID, "err", err)
			continue
		}

		// Clone the sub-agent version as 'embedded', adding source and parent metadata.
		// projectID is the caller's validated project id. It is NOT recovered
		// from schema: schema is a quoted identifier, so trimming the "p_"
		// prefix off it would leave the quotes in source_project_id.
		var embeddedVerID int
		err = h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_versions
				(application_id, name, status, author_id, llm_settings, instructions,
				 conversation_starters, welcome_message, agent_type, meta, pipeline_settings)
			SELECT $1, name, 'embedded', author_id, llm_settings, instructions,
				   conversation_starters, welcome_message, agent_type,
				   COALESCE(meta, '{}'::jsonb) || jsonb_build_object(
					   'source_version_id', $3::text,
					   'source_application_id', $4::text,
					   'source_project_id', $5::text,
					   'parent_published_app_id', $6::text,
					   'parent_published_version_id', $7::text
				   ),
				   pipeline_settings
			FROM %s.application_versions WHERE id = $2
			RETURNING id`, schema, schema),
			embeddedAppID, ref.VersionID, ref.VersionID, ref.ApplicationID, projectID,
			strconv.Itoa(parentAppID), strconv.Itoa(targetVersionID)).Scan(&embeddedVerID)
		if err != nil {
			slog.ErrorContext(ctx, "embed_sub_agents: sub-agent version clone failed",
				"schema", schema, "source_version_id", ref.VersionID,
				"parent_version_id", targetVersionID, "err", err)
			continue
		}

		// Clone entity_tool_mapping for the embedded version.
		//
		// The copy stays outside the publish transaction, because the publish
		// committed before embedSubAgents ran. Its error is logged rather than
		// discarded: the publish cannot be withdrawn, so the caller keeps its 200,
		// and the log is the only channel that can report the loss. See the
		// pull request for issue #406 for the full reasoning.
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
			SELECT $2, $3, entity_type, tool_id, selected_tools
			FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, schema, schema),
			ref.VersionID, embeddedVerID, embeddedAppID); err != nil {
			slog.ErrorContext(ctx, "embed_sub_agents: tool attachment copy failed",
				"schema", schema, "source_version_id", ref.VersionID,
				"embedded_version_id", embeddedVerID, "err", err)
		}

		// Clone entity_skill_mapping for the embedded version. The embed copied
		// tool attachments only, so an embedded sub-agent ran without the skills
		// its author gave it (#406). Publish has the same copy (#351), and fork
		// has it too (internal/api/oapiserver/publishing.go:149).
		//
		// The table has no `entity_id` column (001_initial.sql:422-432): a skill
		// attachment is keyed by (entity_version_id, skill_id, entity_type) alone.
		// That is why this statement names four columns where the tool copy above
		// names five. `entity_type` is carried from the source row rather than
		// defaulted, because it is part of that key and the chat read matches on it
		// (internal/db/queries/agent_chat.sql:132). `skill_version_id` rides along
		// because the same read LEFT JOINs it for the skill instructions — dropping
		// it embeds a named skill with an empty body.
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
			SELECT $2, entity_type, skill_id, skill_version_id
			FROM %s.entity_skill_mapping WHERE entity_version_id = $1`, schema, schema),
			ref.VersionID, embeddedVerID); err != nil {
			slog.ErrorContext(ctx, "embed_sub_agents: skill attachment copy failed",
				"schema", schema, "source_version_id", ref.VersionID,
				"embedded_version_id", embeddedVerID, "err", err)
		}

		// Link the embedded copy onto the published version through the real
		// pair of tables. The author is the embedded version's own author and
		// the owner is the project, mirroring the attach path.
		var embeddedAuthorID int
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(author_id, 0) FROM %s.application_versions WHERE id = $1`, schema),
			embeddedVerID).Scan(&embeddedAuthorID) // failure leaves 0; the link is still written
		ownerProjectID, ownerErr := strconv.Atoi(projectID)
		includeOwnerID := false
		if ownerErr == nil {
			if hasOwner, probeErr := eliteaToolsHasOwnerID(ctx, h.pool, ownerProjectID); probeErr == nil {
				includeOwnerID = hasOwner
			}
		}
		if err := insertApplicationToolReference(
			ctx, h.pool, schema, int64(targetVersionID), ref.Name,
			int64(embeddedAppID), int64(embeddedVerID),
			ownerProjectID, includeOwnerID, embeddedAuthorID,
		); err != nil {
			slog.ErrorContext(ctx, "embed_sub_agents: sub-agent link failed",
				"schema", schema, "parent_version_id", targetVersionID,
				"embedded_version_id", embeddedVerID, "err", err)
		}

		// Recursively embed sub-agents of this sub-agent
		h.embedSubAgentsRecursive(ctx, schema, projectID, ref.VersionID, embeddedVerID, depth+1)
	}
}

func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // optional body; ignore decode errors

	// Check version exists and get meta
	var status string
	var metaStr string
	var authorID *int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT status, COALESCE(meta::text, '{}'), author_id FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&status, &metaStr, &authorID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(metaStr), &meta) // DB jsonb column; malformed means nil meta

	// The catalogue rows this unpublish must also take down. Collected per
	// branch below and removed once, after the source revert, so that a
	// failure to reach the catalogue is reported rather than swallowed: an
	// agent that stays in ELITEA Catalog after its author unpublished it is
	// the one outcome this route may not answer 200 for.
	var revertedVersionIDs []int

	switch status {
	case "published", "embedded":
		h.deleteEmbeddedSubAgents(ctx, s, versionID)

		// Revert to draft
		_, _ = h.pool.Exec(ctx, fmt.Sprintf(
			`UPDATE %s.application_versions SET status = 'draft' WHERE id = $1`, s), versionID) // best-effort revert
		// …and give the release name back. The reverted row used to keep it,
		// which spent the name for good (issue 854).
		h.releaseWithdrawnVersionName(ctx, s, versionID)
		if numericVersionID, convErr := strconv.Atoi(versionID); convErr == nil {
			revertedVersionIDs = append(revertedVersionIDs, numericVersionID)
		}
	case "draft":
		// Unpublish via the source draft version: find all published clones and delete them
		var appID int
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT application_id FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&appID) // failure leaves appID=0
		var hasPublished bool
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND status IN ('published', 'embedded') AND id != $2)`, s), appID, versionID).Scan(&hasPublished) // failure leaves hasPublished=false
		if !hasPublished {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "version is not published"})
			return
		}
		pubRows, _ := h.pool.Query(ctx, fmt.Sprintf(
			`SELECT id FROM %s.application_versions WHERE application_id = $1 AND status IN ('published','embedded') AND id != $2`, s), appID, versionID)
		if pubRows != nil {
			var pubVerIDs []string
			for pubRows.Next() {
				var pvid string
				if pubRows.Scan(&pvid) != nil {
					continue
				}
				pubVerIDs = append(pubVerIDs, pvid)
			}
			pubRows.Close()
			for _, pvid := range pubVerIDs {
				h.deleteEmbeddedSubAgents(ctx, s, pvid)
				if numericVersionID, convErr := strconv.Atoi(pvid); convErr == nil {
					revertedVersionIDs = append(revertedVersionIDs, numericVersionID)
				}
			}
		}
		_, _ = h.pool.Exec(ctx, fmt.Sprintf(
			`UPDATE %s.application_versions SET status = 'draft' WHERE application_id = $1 AND status IN ('published', 'embedded')`, s), appID) // best-effort revert
		// Each reverted clone gives its release name back, one at a time: the
		// replacement names are per-row and the counter has to see the names
		// the previous rename wrote (issue 854).
		for _, revertedID := range revertedVersionIDs {
			h.releaseWithdrawnVersionName(ctx, s, strconv.Itoa(revertedID))
		}
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "version is not published"})
		return
	}

	if err := removeCatalogTwins(ctx, h.pool, projectID, revertedVersionIDs); err != nil {
		slog.ErrorContext(ctx, "unpublish: catalog twin removal failed", "project_id", projectID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to remove the agent from the catalog"})
		return
	}

	if h.events != nil {
		h.events.Emit(ctx, projectID, "agent.version.unpublished", map[string]any{
			"source_version_id":    versionID,
			"reverted_version_ids": revertedVersionIDs,
			"reason":               body.Reason,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (h *Handler) runPublishValidation(ctx context.Context, s, versionID, versionName string) (map[string]any, int) {
	criticalIssues := []map[string]any{}
	warnings := []map[string]any{}
	recommendations := []map[string]any{}

	var vName, vStatus string
	var appID int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id, name, status FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&appID, &vName, &vStatus)
	if err != nil {
		return nil, 0
	}

	if vStatus == "published" {
		criticalIssues = append(criticalIssues, map[string]any{"field": "version", "issue": "version is already published", "source": "deterministic"})
	}

	// Check version name collision
	var nameExists bool
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND name = $2)`, s),
		appID, versionName).Scan(&nameExists) // failure leaves nameExists=false, safe
	if nameExists {
		criticalIssues = append(criticalIssues, map[string]any{"rule": "version_name_exists_in_source", "field": "version_name", "issue": "version name already exists", "source": "deterministic"})
	}

	// Check for generic version names
	genericNames := map[string]bool{"v1": true, "v2": true, "v3": true, "latest": true, "new": true, "test": true}
	if genericNames[strings.ToLower(versionName)] {
		warnings = append(warnings, map[string]any{"field": "version_name", "issue": fmt.Sprintf("'%s' is a generic version name — consider something more descriptive", versionName), "source": "deterministic"})
	}

	// Collect sub-agent references
	type subAgentInfo struct {
		name      string
		appID     string
		versionID string
		appName   string
		verName   string
		agentType string
	}
	toolRefs, saErr := listApplicationToolReferences(ctx, h.pool, s, versionID)
	var subAgents []subAgentInfo
	if saErr == nil {
		for _, ref := range toolRefs {
			var saAppName, saVerName, saAgentType string
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(name,'') FROM %s.applications WHERE id = $1`, s), ref.ApplicationID).Scan(&saAppName)
			_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(name,''), COALESCE(agent_type,'') FROM %s.application_versions WHERE id = $1`, s), ref.VersionID).Scan(&saVerName, &saAgentType)
			subAgents = append(subAgents, subAgentInfo{name: ref.Name, appID: ref.ApplicationID, versionID: ref.VersionID, appName: saAppName, verName: saVerName, agentType: saAgentType})
		}
	}

	// Cycle and depth detection — if either found, short-circuit
	const maxSubAgentDepth = 3
	if len(subAgents) > 0 {
		visited := map[string]bool{versionID: true}
		var hasCycle bool
		var depthExceeded bool
		var checkGraph func(verID string, depth int)
		checkGraph = func(verID string, depth int) {
			if hasCycle || depthExceeded {
				return
			}
			if depth > maxSubAgentDepth {
				depthExceeded = true
				return
			}
			childRefs, err2 := listApplicationToolReferences(ctx, h.pool, s, verID)
			if err2 != nil {
				return
			}
			for _, childRef := range childRefs {
				if visited[childRef.VersionID] {
					hasCycle = true
					return
				}
				visited[childRef.VersionID] = true
				checkGraph(childRef.VersionID, depth+1)
				if hasCycle || depthExceeded {
					return
				}
			}
		}
		for _, sa := range subAgents {
			if visited[sa.versionID] {
				hasCycle = true
				break
			}
			visited[sa.versionID] = true
			checkGraph(sa.versionID, 1)
			if hasCycle || depthExceeded {
				break
			}
		}
		if hasCycle {
			return map[string]any{
				"status": "FAIL",
				"critical_issues": []map[string]any{{
					"field":   "sub_agents",
					"issue":   "circular dependency detected among sub-agents",
					"source":  "deterministic",
					"context": nil,
					"fix":     "Remove one of the circular sub-agent references to break the cycle",
				}},
				"warnings":                []map[string]any{},
				"recommendations":         []map[string]any{},
				"summary":                 fmt.Sprintf("Validation FAIL for version %s", versionID),
				"counts":                  map[string]any{"critical": 1, "warnings": 0, "suggestions": 0},
				"ai_validation_available": false,
				"validation_token":        nil,
			}, http.StatusUnprocessableEntity
		}
		if depthExceeded {
			return map[string]any{
				"status": "FAIL",
				"critical_issues": []map[string]any{{
					"field":   "sub_agents",
					"issue":   "sub-agent nesting depth exceeds maximum allowed",
					"source":  "deterministic",
					"context": nil,
					"fix":     "Reduce the nesting depth of sub-agent references",
				}},
				"warnings":                []map[string]any{},
				"recommendations":         []map[string]any{},
				"summary":                 fmt.Sprintf("Validation FAIL for version %s", versionID),
				"counts":                  map[string]any{"critical": 1, "warnings": 0, "suggestions": 0},
				"ai_validation_available": false,
				"validation_token":        nil,
			}, http.StatusUnprocessableEntity
		}
	}

	// Load version details for content validation
	var instructions, welcomeMsg string
	var conversationStarters []byte
	var tagCount, toolCount int
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(instructions, ''), COALESCE(welcome_message, ''), COALESCE(conversation_starters::text, '[]')::bytea FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&instructions, &welcomeMsg, &conversationStarters) // failure leaves empty strings
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, s), versionID).Scan(&toolCount) // failure leaves toolCount=0
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.application_version_tag_association WHERE version_id = $1`, s), versionID).Scan(&tagCount) // failure leaves tagCount=0

	// Parse conversation_starters
	var starters []string
	_ = json.Unmarshal(conversationStarters, &starters) // DB jsonb column; malformed means empty starters

	// Sparse agent: instructions too short (< 50 chars)
	if len(strings.TrimSpace(instructions)) < 50 {
		criticalIssues = append(criticalIssues, map[string]any{"field": "instructions", "issue": "agent instructions are too short for a meaningful public agent", "source": "deterministic"})
	}

	// No conversation starters
	if len(starters) == 0 {
		warnings = append(warnings, map[string]any{"field": "conversation_starters", "issue": "no conversation starters — users won't know how to begin", "source": "deterministic"})
	}

	// Tag discoverability recommendation
	if tagCount < 3 {
		recommendations = append(recommendations, map[string]any{"field": "tags", "suggestion": "add more tags to improve discoverability in the marketplace", "source": "deterministic"})
	}

	// Sub-agent validation (no cycle): check duplicates, skip pipelines, recurse
	if len(subAgents) > 0 {
		// Detect duplicate sub-agent names (excluding pipelines)
		nameCount := map[string]int{}
		for _, sa := range subAgents {
			if sa.agentType == "pipeline" {
				continue
			}
			nameCount[sa.appName]++
		}
		for name, count := range nameCount {
			if count > 1 {
				ctx1 := fmt.Sprintf("sub-agent: %s", name)
				criticalIssues = append(criticalIssues, map[string]any{
					"field":   "name",
					"issue":   fmt.Sprintf("sub-agent name is not unique (%d occurrences found)", count),
					"source":  "deterministic",
					"context": ctx1,
				})
			}
		}

		// Recursively validate sub-agents (skip pipelines)
		var validateSubAgents func(verID string, depth int)
		validateSubAgents = func(verID string, depth int) {
			if depth > 5 {
				return
			}
			saRefs, saErr2 := listApplicationToolReferences(ctx, h.pool, s, verID)
			if saErr2 != nil {
				return
			}
			for _, ref := range saRefs {
				var saAppName, saVerName, saAgentType, saInstr, saDesc string
				_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(name,''), COALESCE(description,'') FROM %s.applications WHERE id = $1`, s), ref.ApplicationID).Scan(&saAppName, &saDesc)
				_ = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(name,''), COALESCE(agent_type,''), COALESCE(instructions,'') FROM %s.application_versions WHERE id = $1`, s), ref.VersionID).Scan(&saVerName, &saAgentType, &saInstr)
				if saAgentType == "pipeline" {
					continue
				}
				saContext := fmt.Sprintf("sub-agent: %s (%s)", saAppName, saVerName)
				if len(strings.TrimSpace(saInstr)) < 50 {
					criticalIssues = append(criticalIssues, map[string]any{
						"field":   "instructions",
						"issue":   "agent instructions are too short for a meaningful public agent",
						"source":  "deterministic",
						"context": saContext,
					})
				}
				if len(strings.TrimSpace(saDesc)) < 20 {
					warnings = append(warnings, map[string]any{
						"field":   "description",
						"issue":   "sub-agent description is too short for meaningful discovery",
						"source":  "deterministic",
						"context": saContext,
					})
				}
				validateSubAgents(ref.VersionID, depth+1)
			}
		}
		validateSubAgents(versionID, 0)
	}

	// Check LLM model is from an accessible project
	var llmStr *string
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT llm_settings::text FROM %s.application_versions WHERE id = $1`, s), versionID).Scan(&llmStr) // failure leaves nil, safe
	if llmStr != nil {
		var llm map[string]any
		_ = json.Unmarshal([]byte(*llmStr), &llm) // DB jsonb column; malformed means empty map
		if mpid, ok := llm["model_project_id"]; ok && mpid != nil {
			mpidStr := fmt.Sprintf("%v", mpid)
			// One project, for the reason Publish's hard-check above states.
			if mpidStr != publicproject.IDString() {
				criticalIssues = append(criticalIssues, map[string]any{
					"field":  "llm_settings",
					"issue":  "model is not shared and cannot be used in published agents",
					"source": "deterministic",
				})
			} else if refusal := h.publishGrantRefusal(ctx, llm); refusal != "" {
				// The same rule the publish itself applies, raised HERE so the
				// author reads it in the pre-publish check rather than only in
				// the refusal of the publish they then attempt.
				criticalIssues = append(criticalIssues, map[string]any{
					"field":  "llm_settings",
					"issue":  refusal,
					"source": "deterministic",
				})
			}
		}
	}

	status := "PASS"
	httpStatus := http.StatusOK
	if len(criticalIssues) > 0 {
		status = "FAIL"
		httpStatus = http.StatusUnprocessableEntity
	} else if len(warnings) > 0 {
		status = "WARN"
	}

	// Generate validation token (non-FAIL only)
	//
	// The token is a signed grant for THIS version, and it expires. It used to
	// be two random ids the publish route recognised by their shape alone, so
	// the check it stands in for could be skipped with any hexadecimal string
	// (issue 855).
	var token *string
	if status != "FAIL" {
		if hash := h.publishTokenContentHash(ctx, s, versionID); hash != "" {
			t := issuePublishValidationToken(versionID, hash, time.Now().Add(publishValidationTokenTTL))
			token = &t
		}
	}

	resp := map[string]any{
		"status":                  status,
		"critical_issues":         criticalIssues,
		"warnings":                warnings,
		"recommendations":         recommendations,
		"summary":                 fmt.Sprintf("Validation %s for version %s", status, versionID),
		"counts":                  map[string]any{"critical": len(criticalIssues), "warnings": len(warnings), "suggestions": len(recommendations)},
		"ai_validation_available": false,
		"validation_token":        token,
	}

	return resp, httpStatus
}

func (h *Handler) PublishValidate(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	// Parse body for version_name
	var body struct {
		VersionName string `json:"version_name"`
		Category    string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// Validate version_name
	if body.VersionName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": []map[string]any{
				{"loc": []string{"body", "version_name"}, "msg": "field required", "type": "value_error.missing"},
			},
		})
		return
	}
	for _, c := range body.VersionName {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": []map[string]any{
					{"loc": []string{"body", "version_name"}, "msg": "string does not match regex \"^[a-zA-Z0-9._-]+$\"", "type": "value_error.str.regex"},
				},
			})
			return
		}
	}

	// Check version exists
	var exists bool
	_ = h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE id = $1)`, s), versionID).Scan(&exists) // failure leaves exists=false, returns 404
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	}

	resp, httpStatus := h.runPublishValidation(ctx, s, versionID, body.VersionName)
	if resp == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "version not found"})
		return
	}

	writeJSON(w, httpStatus, resp)
}

func (h *Handler) VersionValidator(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	versionID := chi.URLParam(r, "versionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()
	q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE id = $1 AND application_id = $2)`, s)
	var valid bool
	_ = h.pool.QueryRow(ctx, q, versionID, applicationID).Scan(&valid) // failure leaves valid=false, which is correct (not found)
	writeJSON(w, http.StatusOK, map[string]any{"valid": valid})
}

func (h *Handler) publicApplicationDetail(w http.ResponseWriter, r *http.Request, ctx context.Context, applicationID string) {
	versionName := chi.URLParam(r, "versionName")
	publicProjectID := publicProjectIDOrDefault()
	schema := publicTenantSchema()

	// Find the application and its published version
	var appName, appDesc string
	var appID int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id, name, COALESCE(description, '')
		FROM %s.applications WHERE id = $1`, schema), applicationID).Scan(&appID, &appName, &appDesc)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "application not found"})
		return
	}

	// Find the version
	var versionQuery string
	if versionName != "" {
		versionQuery = fmt.Sprintf(`
			SELECT id, name, status, agent_type, COALESCE(instructions, ''),
				COALESCE(welcome_message, ''), COALESCE(llm_settings::text, '{}'),
				COALESCE(meta::text, '{}'), COALESCE(conversation_starters::text, '[]'),
				COALESCE(pipeline_settings::text, '{}'), author_id
			FROM %s.application_versions
			WHERE application_id = $1 AND name = $2 AND status = 'published'`, schema)
	} else {
		versionQuery = fmt.Sprintf(`
			SELECT id, name, status, agent_type, COALESCE(instructions, ''),
				COALESCE(welcome_message, ''), COALESCE(llm_settings::text, '{}'),
				COALESCE(meta::text, '{}'), COALESCE(conversation_starters::text, '[]'),
				COALESCE(pipeline_settings::text, '{}'), author_id
			FROM %s.application_versions
			WHERE application_id = $1 AND status = 'published'
			ORDER BY id DESC LIMIT 1`, schema)
	}

	var vID int
	var vName, vStatus, agentType, instrVal, welcomeVal string
	var llmJSON, metaJSON, startersJSON, pipelineJSON []byte
	var authorID *int

	var scanErr error
	if versionName != "" {
		scanErr = h.pool.QueryRow(ctx, versionQuery, applicationID, versionName).Scan(
			&vID, &vName, &vStatus, &agentType, &instrVal, &welcomeVal,
			&llmJSON, &metaJSON, &startersJSON, &pipelineJSON, &authorID)
	} else {
		scanErr = h.pool.QueryRow(ctx, versionQuery, applicationID).Scan(
			&vID, &vName, &vStatus, &agentType, &instrVal, &welcomeVal,
			&llmJSON, &metaJSON, &startersJSON, &pipelineJSON, &authorID)
	}
	if scanErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "published version not found"})
		return
	}

	var llmSettings, meta, starters, pipelineSettings any
	_ = json.Unmarshal(llmJSON, &llmSettings)           // DB jsonb columns
	_ = json.Unmarshal(metaJSON, &meta)                 // DB jsonb columns
	_ = json.Unmarshal(startersJSON, &starters)         // DB jsonb columns
	_ = json.Unmarshal(pipelineJSON, &pipelineSettings) // DB jsonb columns

	projIDInt, _ := strconv.Atoi(publicProjectID)

	// Fetch tools
	tools := make([]map[string]any, 0)
	toolRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT etm.id, etm.tool_id, etm.entity_type, COALESCE(etm.selected_tools::text, '{}'),
			t.name, t.type, t.settings
		FROM %s.entity_tool_mapping etm
		LEFT JOIN %s.elitea_tools t ON t.id = etm.tool_id
		WHERE etm.entity_version_id = $1
		  AND COALESCE(t.type, '') <> 'application'`, schema, schema), vID)
	if err == nil {
		defer toolRows.Close()
		for toolRows.Next() {
			var etmID, toolID int
			var entityType, selectedToolsStr string
			var tName, tType *string
			var tSettings []byte
			if toolRows.Scan(&etmID, &toolID, &entityType, &selectedToolsStr, &tName, &tType, &tSettings) != nil {
				continue
			}
			var selectedTools any
			_ = json.Unmarshal([]byte(selectedToolsStr), &selectedTools) // DB jsonb column
			var settings any
			if tSettings != nil {
				_ = json.Unmarshal(tSettings, &settings) // DB jsonb column
			}
			tool := map[string]any{
				"id":             etmID,
				"tool_id":        toolID,
				"entity_type":    entityType,
				"selected_tools": selectedTools,
			}
			if tName != nil {
				tool["name"] = *tName
			}
			if tType != nil {
				tool["type"] = *tType
			}
			if settings != nil {
				tool["settings"] = settings
			}
			tools = append(tools, tool)
		}
	}

	// Fetch the sub-agent references. They live in the same two tables the
	// generic tool read above walks, but the baseline serialises them as their
	// own entries with the settings pair surfaced, so the expanded view keeps
	// that shape.
	if subAgentRefs, refErr := listApplicationToolReferences(ctx, h.pool, schema, strconv.Itoa(vID)); refErr == nil {
		for _, ref := range subAgentRefs {
			tools = append(tools, map[string]any{
				"id":   ref.ToolID,
				"name": ref.Name,
				"type": "application",
				"settings": map[string]any{
					"application_id":         jsonNumberIfNumeric(ref.ApplicationID),
					"application_version_id": jsonNumberIfNumeric(ref.VersionID),
				},
				"project_id": projIDInt,
			})
		}
	}

	authorIDStr := ""
	if authorID != nil {
		authorIDStr = strconv.Itoa(*authorID)
	}

	versionDetails := map[string]any{
		"id":                    strconv.Itoa(vID),
		"application_id":        strconv.Itoa(appID),
		"name":                  vName,
		"status":                vStatus,
		"agent_type":            agentType,
		"instructions":          instrVal,
		"welcome_message":       welcomeVal,
		"llm_settings":          llmSettings,
		"meta":                  meta,
		"conversation_starters": starters,
		"pipeline_settings":     pipelineSettings,
		"author_id":             authorIDStr,
		"tools":                 tools,
		"tags":                  []any{},
	}

	resp := map[string]any{
		"id":              strconv.Itoa(appID),
		"name":            appName,
		"description":     appDesc,
		"version_details": versionDetails,
	}
	writeJSON(w, http.StatusOK, resp)
}

// `AdminPublishedAgents` used to sit here: dead code with no caller — no route
// in internal/api/router.go referenced it — that walked EVERY `p_%` schema in
// information_schema, swallowed any query error into an empty page, reported
// `adoption` as two hardcoded zeroes and returned no pagination at all. The
// real listing is internal/api/v2/eliteacore/admin_published_agents.go, which
// reads the public project the way the pylon original does and is registered on
// `GET /elitea_core/admin_published_agents/administration`.

func (h *Handler) TrendingAuthors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// `ModerationStatus` used to sit here, answering `{"status":"approved"}` to
// every caller for every entity — a gate that always said yes — while the POST
// registered on the same handler created nothing. Unit A14 replaces both with
// real project-scoped handlers over `centry.moderation_state`; see
// internal/api/v2/moderation/requests.go.

func (h *Handler) ApplicationRelation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	items := make([]map[string]any, 0)

	// Get skill mappings
	q := fmt.Sprintf(`SELECT skill_id FROM %s.entity_skill_mapping WHERE entity_version_id = $1`, s)
	rows, err := h.pool.Query(ctx, q, versionID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var skillID string
			if rows.Scan(&skillID) != nil {
				continue
			}
			items = append(items, map[string]any{"type": "skill", "id": skillID})
		}
	}

	// Get tool mappings
	q2 := fmt.Sprintf(`SELECT tool_id FROM %s.entity_tool_mapping WHERE entity_version_id = $1`, s)
	rows2, err := h.pool.Query(ctx, q2, versionID)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var toolID string
			if rows2.Scan(&toolID) != nil {
				continue
			}
			items = append(items, map[string]any{"type": "tool", "id": toolID})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) Recommendations(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	q := fmt.Sprintf(`
		SELECT a.id, a.name, COALESCE(a.description, ''), COUNT(sl.id) as likes
		FROM %s.applications a
		LEFT JOIN %s.social_likes sl ON sl.entity_id = a.id AND sl.entity_name = 'application'
		GROUP BY a.id, a.name, a.description
		ORDER BY likes DESC
		LIMIT 10`, s, s)

	rows, err := h.pool.Query(ctx, q)
	items := make([]map[string]any, 0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var name, desc string
			var likes int
			if rows.Scan(&id, &name, &desc, &likes) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": fmt.Sprintf("%d", id), "name": name, "description": desc, "likes": likes,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": items, "total": len(items)})
}

func (h *Handler) Feedbacks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	q := fmt.Sprintf(`SELECT id, entity_name, entity_id, user_id, rating, COALESCE(comment, ''), created_at FROM %s.social_feedbacks ORDER BY created_at DESC LIMIT 50`, s)
	rows, err := h.pool.Query(ctx, q)
	items := make([]map[string]any, 0)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var entityName, entityID, userID, comment string
			var rating int
			var createdAt interface{}
			if rows.Scan(&id, &entityName, &entityID, &userID, &rating, &comment, &createdAt) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": fmt.Sprintf("%d", id), "entity_name": entityName, "entity_id": entityID,
				"user_id": userID, "rating": rating, "comment": comment,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) UpdateAttachmentStorage(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	toolkitID, _ := body["toolkit_id"].(string)
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	_, _ = h.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.application_versions
		SET meta = jsonb_set(COALESCE(meta, '{}')::jsonb, '{attachment_storage}', $1::jsonb)
		WHERE id = $2`, s),
		fmt.Sprintf(`{"toolkit_id":"%s"}`, toolkitID), versionID)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DefaultIconURLPrefix is the public path the default entity icons are served
// under. It follows /app/application_icon/ and /app/application_tool_icon/,
// the two static directories internal/api/router.go already mounts.
//
// The two-segment /icons/{projectID}/{filename} route is NOT this prefix: it
// serves the per-project icons a user uploads, out of the object store.
//
// NO ROUTE SERVES THIS PREFIX YET, and no build bakes icon files into the
// image. Read the consequence plainly. DefaultIconCatalogue enumerates a
// directory that no current image creates. GET
// /api/v2/elitea_core/default_icons/prompt_lib/{projectID} therefore answers
// `[]` in every build, and the "Default" section of the icon picker is empty.
//
// This state corrects a worse one. It is not the finished feature. The
// endpoint answered with five invented URLs before. Every one of them
// returned 404, and the picker drew five broken images, because it has no
// fallback for a url that fails. An empty section is honest.
//
// Three items complete the feature. Legacy does all three
// (legacy/plugins/elitea_core/routes/application_icon.py:15 and
// legacy/plugins/elitea_core/module.py:599-616):
//
//  1. Mount a static file server on this prefix in internal/api/router.go,
//     beside /app/application_icon/ and /app/application_tool_icon/.
//  2. Add the prefix to internal/api/main_public_rules.go. A browser <img>
//     tag sends no Authorization header.
//  3. Give the pod the icon files. Legacy downloads the
//     EliteaAI/elitea_static archive into /data/static at start-up.
const DefaultIconURLPrefix = "/app/default_entity_icons/"

// defaultIconDataDir is the directory that holds the default entity icons.
// DEFAULT_ICON_DATA_DIR overrides it.
const defaultIconDataDir = "/data/static/elitea_static-main/default_entity_icons"

// iconDirEnvOr reads the icon directory override, or gives the compiled-in
// default.
//
// The `*Or(...)` NAME IS LOAD-BEARING, not a style choice. The env-drift gate
// (services/elitea-llm-gateway/scripts/env-drift-check.sh:117) sorts every env
// name the code reads by the shape of the call. A bare `os.Getenv("X")` counts
// as REQUIRED. It fails the gate when the chart never sets it. An
// `*Or("X", fallback)` call counts as DEFAULTED and only warns. This variable
// has a compiled-in default, so the second tier is the honest one. Read through
// a bare os.Getenv, it failed the gate. It then read as a feature that is
// silently broken in the deployed pod. It is not broken.
func iconDirEnvOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// DefaultIconCatalogue lists the default entity icons that exist on disk.
//
// It returns an empty slice when the directory is absent or empty. It NEVER
// invents a name: an entry the client cannot load renders as a broken image,
// because the icon picker has no fallback for a url that answers 404.
func DefaultIconCatalogue() []map[string]any {
	directory := iconDirEnvOr("DEFAULT_ICON_DATA_DIR", defaultIconDataDir)
	icons := make([]map[string]any, 0)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return icons
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if strings.HasPrefix(filename, ".") {
			continue
		}
		name := strings.TrimSuffix(filename, filepath.Ext(filename))
		icons = append(icons, map[string]any{
			"name": name,
			"url":  DefaultIconURLPrefix + url.PathEscape(filename),
		})
	}
	return icons
}

func (h *Handler) DefaultIcons(w http.ResponseWriter, _ *http.Request) {
	// The local is load-bearing. scripts/validate_contract_static.sh reads the
	// third argument of writeJSON to prove this route answers a plain array. Its
	// pattern accepts a slice literal or a lowercase identifier, so an inline
	// `DefaultIconCatalogue()` call left the shape unmeasured and failed the gate.
	icons := DefaultIconCatalogue()
	writeJSON(w, http.StatusOK, icons)
}

// iconBucket is the reserved system bucket every uploaded icon lands in
// (S20b) — fixed, not per-project-policy-configurable like S20a's
// attachment_bucket, since nothing in this stage's scope asks for that and
// icons have no retention/quota requirement distinct from "keep them".
const iconBucket = "icons"

func (h *Handler) UploadIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	_ = r.ParseMultipartForm(512 * 1024) // ignore parse errors; FormFile will fail if parsing failed
	file, header, err := r.FormFile("file")
	if err != nil {
		// No file is a legitimate, storage-independent no-op (matches this
		// handler's pre-S20b behavior exactly) — do not gate it on h.store,
		// which only a real upload attempt needs.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": ""})
		return
	}
	defer func() { _ = file.Close() }()

	if h.store == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "icon storage is not configured"})
		return
	}

	if !validIconPathSegment(projectID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	filename := generateID() + safeIconExtension(header.Filename)
	ref, err := storage.NewObjectRef(projectID, iconBucket, filename)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	contentType := mime.TypeByExtension(safeIconExtension(header.Filename))
	if _, err := h.store.Put(r.Context(), ref, file, storage.PutOptions{ContentType: contentType, ContentLength: -1}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save file"})
		return
	}

	url := fmt.Sprintf("/icons/%s/%s", projectID, filename)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"url": url,
		"icon_meta": map[string]any{
			"url":    url,
			"width":  64,
			"height": 64,
		},
	})
}

// DownloadIcon serves an uploaded icon at the exact URL UploadIcon already
// returns (/icons/{projectID}/{filename}) — before S20b nothing served this
// path at all (no route, no Traefik rule, no volume: every uploaded icon
// was unreachable). Deliberately not a *Handler method: this route must be
// mounted outside the authenticated /elitea_core route group — a browser
// <img src="..."> request carries no Authorization header — the same
// public, unauthenticated placement router.go already uses for the
// unrelated /app/application_icon/* and /app/application_tool_icon/*
// static file servers.
func DownloadIcon(store storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		projectID := chi.URLParam(r, "projectID")
		filename := chi.URLParam(r, "filename")
		ref, err := storage.NewObjectRef(projectID, iconBucket, filename)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, _, err := store.Get(r.Context(), ref, nil)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer func() { _ = body.Close() }()

		// nosniff + a content type derived only from the allowlisted
		// extension safeIconExtension itself enforces (never the backend-
		// reported ObjectInfo.ContentType, which for an object written
		// before this allowlist existed could still be arbitrary) — an
		// adversarial-review finding confirmed a stored-XSS path through
		// this route otherwise: it is public/unauthenticated by design (a
		// browser <img src> carries no auth header), so anything it serves
		// with a browser-sniffable or attacker-chosen Content-Type is
		// script-executable in the app's own origin.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// nosniff is NOT sufficient on its own, because `.svg` is an allowlisted
		// icon extension and `image/svg+xml` is a genuine image type that a
		// browser executes script in when the response is rendered as a
		// DOCUMENT (top-level navigation, <object>, <embed>). nosniff only
		// suppresses MIME sniffing, so an uploaded SVG carrying a <script> would
		// otherwise run in this app's own origin, against the viewer's session
		// cookies, on a route that is public and unauthenticated by design.
		//
		// The upload side cannot close this: neither uploader validates the file
		// CONTENT, only the client-supplied extension. So it is closed here,
		// once, for every object this route can ever serve — including the ones
		// already stored before this header existed.
		//
		// `sandbox` with no tokens puts any rendered document in an opaque
		// origin with scripting disabled; `default-src 'none'` stops it fetching
		// anything. `Content-Disposition: attachment` makes a direct navigation
		// download the file instead of rendering it. None of the three affects
		// <img src>, which is how the product actually consumes these icons:
		// Content-Disposition is ignored for subresource loads, and an image is
		// not a document.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Content-Disposition", "attachment")
		if contentType := mime.TypeByExtension(safeIconExtension(filename)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, body)
	}
}

func validIconPathSegment(value string) bool {
	return value != "" &&
		value != "." &&
		value != ".." &&
		len(value) <= 255 &&
		!strings.ContainsAny(value, "/\\\x00")
}

// allowedIconExtensions is the entire set safeIconExtension can return.
// S20b's adversarial review confirmed a stored-XSS path: without this
// allowlist, a caller could upload a "file.html"/"file.svg-with-script"-
// named part, have it stored and served back by DownloadIcon (a public,
// unauthenticated route, since a browser <img src> carries no auth header)
// with a browser-sniffable or attacker-chosen Content-Type, executing
// script in the app's own origin. Restricting storage to genuine image
// extensions closes this at the write path, which is more robust than
// relying on Content-Type alone at the read path (also hardened below).
var allowedIconExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".svg": true, ".webp": true, ".ico": true, ".bmp": true,
}

func safeIconExtension(filename string) string {
	const defaultExtension = ".png"

	lastSeparator := strings.LastIndexAny(filename, "/\\")
	if lastSeparator >= 0 {
		filename = filename[lastSeparator+1:]
	}
	lastDot := strings.LastIndexByte(filename, '.')
	if lastDot <= 0 || lastDot == len(filename)-1 {
		return defaultExtension
	}

	extension := strings.ToLower(filename[lastDot:])
	if len(extension) > 16 {
		return defaultExtension
	}
	for _, char := range extension[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return defaultExtension
		}
	}
	if !allowedIconExtensions[extension] {
		return defaultExtension
	}
	return extension
}

func (h *Handler) ListUploadedIcons(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}, "total": 0})
}

func (h *Handler) UpdateIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	versionID := chi.URLParam(r, "versionId")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var iconMeta map[string]any
	if err := json.NewDecoder(r.Body).Decode(&iconMeta); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// Update meta.icon_meta on the version
	_, _ = h.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.application_versions SET meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('icon_meta', $2::jsonb) WHERE id = $1`, s),
		versionID, mustJSON(iconMeta)) // best-effort update

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) DeleteIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")
	if !validIconPathSegment(projectID) || !validIconPathSegment(name) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx := r.Context()
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	// Clear icon_meta from all versions referencing this icon
	if h.pool != nil {
		_, _ = h.pool.Exec(ctx, fmt.Sprintf(
			`UPDATE %s.application_versions SET meta = jsonb_set(meta, '{icon_meta}', '{}'::jsonb) WHERE meta->'icon_meta'->>'name' = $1`, s), name) // best-effort clear
	}

	// Best-effort remove — Delete is documented idempotent (S1 errors.go),
	// and this handler already returns 204 unconditionally below regardless
	// of outcome, matching its pre-S20b behavior for a missing/invalid file.
	if h.store != nil {
		if ref, err := storage.NewObjectRef(projectID, iconBucket, name); err == nil {
			_ = h.store.Delete(ctx, ref)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// storedAgentType maps the `agent_type` an IMPORTED document carries onto the
// value `application_versions.agent_type` stores, and reports whether the
// import accepts it at all.
//
// The mapping exists because the platform's own markdown export does not write
// the stored spelling. `markdownAgentType` (export_markdown.go) writes `agent`
// for a stored `openai` agent, faithfully mirroring pylon's
// `_application_to_md`. The import's allow-list never had an `agent` member —
// pylon's `AgentTypes` enum has none either — so exporting an agent as markdown
// and posting its frontmatter back answered
// `Import function has been failed: invalid agent_type` (#845). The round trip
// closed only in a browser: both web clients rename `agent` to `openai` in
// their own file parser, and nothing in the API contract said they had to.
//
// The rename is done HERE rather than by changing the exporter, so that every
// `.agent.md` file already written — by this platform, by pylon, or by hand
// from the documented format — keeps importing.
//
// The empty string maps to `openai` as well: that is the column default this
// path applied before, and an entry that names no type is a plain agent.
func storedAgentType(raw string) (string, bool) {
	switch raw {
	case "", "agent", "openai":
		return "openai", true
	case "react", "dial", "pipeline":
		return raw, true
	default:
		return "", false
	}
}

func (h *Handler) ExportImportPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Checked before the body AND before the project id, because it is the
	// handler's own precondition and not the caller's fault. It shared a
	// branch with an empty entity list and answered 201, so an import that
	// could reach no database at all reported that it had imported
	// everything (#505).
	if h.pool == nil {
		slog.ErrorContext(ctx, "import: "+importWriteFailed)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}

	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	// Body can be either a flat array (import_wizard) or a map with an
	// "applications" key. Three faults lived in the ten lines this replaces,
	// and all three ended at the same 201 with an empty result:
	//
	//   - the io.ReadAll error was discarded, so a body that was cut off part
	//     way through was imported as far as it got, which is nothing;
	//   - both json.Unmarshal errors were discarded under a comment that said
	//     "malformed means empty entities", so a corrupt export file was a
	//     successful import of no entities;
	//   - the array branch tested bodyBytes[0], so a body with any leading
	//     whitespace — a newline from a text editor is enough — went to the map
	//     branch, failed to decode as a map, and took the same silent exit.
	//
	// bytes.TrimSpace removes the third. The other two are reported.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(ctx, "import: unable to read the request body", "schema", s, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unable to read the request body"})
		return
	}
	trimmed := bytes.TrimSpace(bodyBytes)
	var entities []any
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entities); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body: " + err.Error()})
			return
		}
	} else {
		var bodyMap map[string]any
		if err := json.Unmarshal(trimmed, &bodyMap); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body: " + err.Error()})
			return
		}
		// A body that carries no `applications` array names nothing to import.
		// It used to answer 201 with an empty result, which the wizard reads as
		// a completed import. An `applications` key that IS an array and IS
		// empty keeps that answer: it asks for nothing and gets nothing.
		apps, ok := bodyMap["applications"].([]any)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body must carry an applications array"})
			return
		}
		entities = apps
	}

	if len(entities) == 0 {
		writeJSON(w, http.StatusCreated, map[string]any{
			"result": importChannels[any](nil, nil, nil),
			"errors": importChannels[any](nil, nil, nil),
		})
		return
	}

	userID, ok := importPrincipalUserID(ctx)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "an authenticated principal is required to import"})
		return
	}

	resultAgents := make([]map[string]any, 0)
	errorAgents := make([]any, 0)
	resultToolkits := make([]map[string]any, 0)
	errorToolkits := make([]any, 0)
	resultSkills := make([]map[string]any, 0)
	errorSkills := make([]any, 0)

	// Separate entities by type, preserving original indices for error reporting
	type toolkitEntry struct {
		entityIdx  int
		importUUID string
		raw        map[string]any
	}
	type agentEntry struct {
		entityIdx int
		raw       map[string]any
	}
	// A skill is an entity of its own, beside an agent and a toolkit. It had no
	// branch here, so every `entity: "skills"` entry the wizard sent fell to the
	// agent branch below and imported a phantom AGENT named after the skill
	// (#611). See import_skills.go.
	type skillEntry struct {
		entityIdx  int
		importUUID string
		raw        map[string]any
	}
	var agentEntries []agentEntry
	var toolkitEntries []toolkitEntry
	var skillEntries []skillEntry

	// An entry that is not a JSON object was dropped by a bare `continue`, so
	// the wizard showed it neither in the result nor in the errors and the
	// import still answered 201. It is reported on the agents channel because
	// nothing in an entry the handler cannot read says which channel it belongs
	// to, and the wizard maps an error entry by its index, not by its channel
	// (apps/elitea-ui, getErrorImportUUID).
	for i, raw := range entities {
		ent, ok := raw.(map[string]any)
		if !ok {
			errorAgents = append(errorAgents, map[string]any{
				"index": i, "name": "",
				"msg": "Import function has been failed: the entry is not a JSON object",
			})
			continue
		}
		entity, _ := ent["entity"].(string)
		switch entity {
		case "toolkits":
			iuuid, _ := ent["import_uuid"].(string)
			toolkitEntries = append(toolkitEntries, toolkitEntry{entityIdx: i, importUUID: iuuid, raw: ent})
		case "skills":
			iuuid, _ := ent["import_uuid"].(string)
			skillEntries = append(skillEntries, skillEntry{entityIdx: i, importUUID: iuuid, raw: ent})
		default:
			agentEntries = append(agentEntries, agentEntry{entityIdx: i, raw: ent})
		}
	}

	// Phase 0: Import the skills, so the per-version references the agents carry
	// have something to resolve against. `owner_id` on this table is the
	// DESTINATION PROJECT (#533), which is the same value the toolkit import
	// below reads through `tenantOwnerID`.
	// The destination project, resolved ONCE for both entity types. `owner_id`
	// on `skills` and on `elitea_tools` is the OWNING PROJECT and not a user
	// (#533), and both come from the same immutable path segment, so a second
	// resolution could only ever agree with the first — or drift away from it
	// after a later edit.
	destinationOwnerID, destinationOwnerErr := tenantOwnerID(projectID)

	importedSkills := map[string]importedSkill{}
	for _, se := range skillEntries {
		skillName, _ := se.raw["name"].(string)
		if destinationOwnerErr != nil {
			errorSkills = append(errorSkills, map[string]any{
				"index": se.entityIdx, "name": skillName,
				"msg": "Import function has been failed: " + destinationOwnerErr.Error(),
			})
			continue
		}
		created, err := h.importSkill(ctx, s, destinationOwnerID, userID, se.raw)
		// A skill can be written and still fail, because the row is inserted
		// before its versions are. The row then exists, so it is REPORTED and
		// REGISTERED rather than dropped: a caller that sees only
		// "it is not among the imported skills" cannot find the row it now owns,
		// and a reference to it can still resolve to the versions that landed.
		// The error is kept beside the result, which is the shape a failed
		// toolkit link already answers with (#420).
		if created.id != 0 {
			if se.importUUID != "" {
				importedSkills[se.importUUID] = created
			}
			resultSkills = append(resultSkills, map[string]any{
				"id": strconv.Itoa(created.id), "name": skillName,
			})
		}
		if err != nil {
			slog.ErrorContext(ctx, "import: skill import failed",
				"schema", s, "name", skillName, "skill_id", created.id, "error", err)
			message := "Import function has been failed: " + err.Error()
			if created.id != 0 {
				message = fmt.Sprintf(
					"Import function has been failed: skill %d was written and is incomplete: %s",
					created.id, err.Error())
			}
			errorSkills = append(errorSkills, map[string]any{
				"index": se.entityIdx, "name": skillName, "msg": message,
			})
		}
	}

	// Phase 1: Import agents and build import_uuid -> appID maps
	agentImportUUIDToAppID := map[string]int{}
	agentVersionImportUUIDToVerID := map[string]int{}

	type importedAgentInfo struct {
		appID    int
		versions [][]map[string]any // per-version tool refs to resolve later
		skills   [][]any            // per-version skill refs, in the same order
	}
	importedAgents := make([]importedAgentInfo, 0)

	for _, ae := range agentEntries {
		app := ae.raw
		name, _ := app["name"].(string)
		desc, _ := app["description"].(string)

		versions, hasVersions := app["versions"].([]any)
		if !hasVersions || len(versions) == 0 {
			errorAgents = append(errorAgents, map[string]any{"index": ae.entityIdx, "name": name, "msg": "Import function has been failed: no versions provided"})
			importedAgents = append(importedAgents, importedAgentInfo{appID: -1})
			continue
		}

		if firstVer, ok := versions[0].(map[string]any); ok {
			at, _ := firstVer["agent_type"].(string)
			if _, ok := storedAgentType(at); !ok {
				errorAgents = append(errorAgents, map[string]any{"index": ae.entityIdx, "name": name, "msg": "Import function has been failed: invalid agent_type"})
				importedAgents = append(importedAgents, importedAgentInfo{appID: -1})
				continue
			}
		}

		if destinationOwnerErr != nil {
			errorAgents = append(errorAgents, map[string]any{"index": ae.entityIdx, "name": name, "msg": "Import function has been failed: " + destinationOwnerErr.Error()})
			importedAgents = append(importedAgents, importedAgentInfo{appID: -1})
			continue
		}
		// `applications.owner_id` is the DESTINATION PROJECT (#533), the same
		// value the skill and the toolkit imports above write. This statement
		// put the caller user id there, which made the agent invisible to every
		// legacy read: the legacy runtime filters `Application.owner_id ==
		// project_id`. The caller is the version author, one statement below.
		var appID int
		err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.applications (name, description, owner_id)
			VALUES ($1, $2, $3) RETURNING id`, s),
			name, desc, destinationOwnerID.Int64()).Scan(&appID)
		if err != nil {
			errorAgents = append(errorAgents, map[string]any{"index": ae.entityIdx, "name": name, "msg": "Import function has been failed: " + err.Error()})
			importedAgents = append(importedAgents, importedAgentInfo{appID: -1})
			continue
		}

		appImportUUID, _ := app["import_uuid"].(string)
		if appImportUUID != "" {
			agentImportUUIDToAppID[appImportUUID] = appID
		}

		createdVersions := make([]map[string]any, 0)
		var versionDetails map[string]any
		var versionToolRefs [][]map[string]any
		var versionSkillRefs [][]any

		for _, vRaw := range versions {
			v, ok := vRaw.(map[string]any)
			if !ok {
				errorAgents = append(errorAgents, map[string]any{
					"index": ae.entityIdx,
					"name":  name,
					"msg":   "Import function has been failed: a version entry is not a JSON object",
				})
				continue
			}
			vName, _ := v["name"].(string)
			if vName == "" {
				vName = "latest"
			}
			agentType, _ := v["agent_type"].(string)
			if stored, ok := storedAgentType(agentType); ok {
				agentType = stored
			}
			instructions, _ := v["instructions"].(string)
			welcomeMsg, _ := v["welcome_message"].(string)

			// Three jsonb columns, one rule. A key that is absent, or null,
			// keeps the column default. A key of the wrong JSON type, or one
			// that cannot be encoded, is reported. It used to take the same
			// empty default as an absent key, so a file whose llm_settings was
			// not an object imported an agent with no model, and answered 201.
			llmJSON, llmErr := importedJSONObject(v, "llm_settings")
			startersJSON, startersErr := importedJSONArray(v, "conversation_starters")
			metaJSON, metaErr := importedJSONObject(v, "meta")
			if columnErr := errors.Join(llmErr, startersErr, metaErr); columnErr != nil {
				errorAgents = append(errorAgents, map[string]any{
					"index": ae.entityIdx,
					"name":  name,
					"msg":   "Import function has been failed: unable to import version " + vName + ": " + columnErr.Error(),
				})
				continue
			}

			var vID int
			err = h.pool.QueryRow(ctx, fmt.Sprintf(`
				INSERT INTO %s.application_versions (application_id, name, status, agent_type, instructions, welcome_message, llm_settings, conversation_starters, author_id, meta, pipeline_settings)
				VALUES ($1, $2, 'draft', $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9::jsonb, '{}'::jsonb) RETURNING id`, s),
				appID, vName, agentType, instructions, welcomeMsg, llmJSON, startersJSON, userID.Int64(), metaJSON).Scan(&vID)
			if err != nil {
				// Sibling of the tool-link defect below (#420). The bare
				// `continue` dropped the version and told nobody. The insert
				// above had already written the agent row. The import therefore
				// answered 201, and the agent held fewer versions than the file.
				// An agent whose every version failed held no version at all.
				slog.ErrorContext(ctx, "import: application version insert failed",
					"schema", s, "application_id", appID, "version_name", vName, "error", err)
				errorAgents = append(errorAgents, map[string]any{
					"index": ae.entityIdx,
					"name":  name,
					"msg":   "Import function has been failed: unable to import version " + vName + ": " + err.Error(),
				})
				continue
			}

			vImportUUID, _ := v["import_version_uuid"].(string)
			if vImportUUID != "" {
				agentVersionImportUUIDToVerID[vImportUUID] = vID
			}

			// The version's authored variables. This path read the key not at
			// all, and the loss was total and silent: the export writes the
			// array from `p_<id>.application_variables`
			// (export_import.go, exportedVersionVariables), and every reader of
			// an agent reads that same table back — the version-detail GET the
			// editor reloads through, `GetVersionExpanded`, and the next
			// export. An import therefore answered 201 with an agent whose
			// declared variables were gone from the screen, gone from the API,
			// gone from the runtime, and gone from any document made of it
			// afterwards, so the round trip could not survive a second trip.
			//
			// `meta` is not the store and cannot stand in for it: it is carried
			// verbatim from the file above, so it holds a variable only when
			// the source project was this platform (`versionFromBody` mirrors
			// the list into it for the write echo) and never when the source
			// was pylon, whose `update_version` dumps with
			// `exclude={'tags', 'variables', 'tools'}`.
			//
			// Reported, not best-effort. A variable is a value the agent needs
			// to run, so a lost one is the same 201-with-a-broken-agent the
			// fork path's identical insert was changed away from, and the
			// caller reads both through the same wizard channel.
			//
			// `importedVariables` collects the rows that were WRITTEN, so the
			// echo below can report what the database holds rather than what
			// the file asked for.
			importedVariables := make([]map[string]any, 0)
			if vars, ok := v["variables"].([]any); ok {
				for _, varRaw := range vars {
					varMap, _ := varRaw.(map[string]any)
					if varMap == nil {
						continue
					}
					varName, _ := varMap["name"].(string)
					varValue, _ := varMap["value"].(string)
					if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
						INSERT INTO %s.application_variables (application_version_id, name, value) VALUES ($1, $2, $3)`, s),
						vID, varName, varValue); err != nil {
						slog.ErrorContext(ctx, "import: application variable insert failed",
							"schema", s, "version_id", vID, "variable", varName, "error", err)
						errorAgents = append(errorAgents, map[string]any{
							"index": ae.entityIdx,
							"name":  name,
							"msg":   "Import function has been failed: unable to import variable " + varName + ": " + err.Error(),
						})
						continue
					}
					importedVariables = append(importedVariables, map[string]any{
						"name": varName, "value": varValue,
					})
				}
			}

			// The version's tags, one key over from the variables and lost the
			// same way: the file carries them (export_import.go,
			// exportedVersionTags), and this path read the key not at all, so an
			// exported agent could not bring its tags home. They go to the store
			// the editor, the version-detail GET, the publish validation and the
			// next export all use. See importVersionTags for the store, the
			// conflict rule and why each failure is reported.
			importedTags, tagFailures := h.importVersionTags(ctx, s, vName, vID, v)
			for _, message := range tagFailures {
				slog.ErrorContext(ctx, "import: application version tag write failed",
					"schema", s, "version_id", vID, "message", message)
				errorAgents = append(errorAgents, map[string]any{
					"index": ae.entityIdx,
					"name":  name,
					"msg":   "Import function has been failed: " + message,
				})
			}

			// Collect tool refs for later resolution
			var toolRefs []map[string]any
			if tools, ok := v["tools"].([]any); ok {
				for _, toolRaw := range tools {
					if tr, ok := toolRaw.(map[string]any); ok {
						toolRefs = append(toolRefs, tr)
					}
				}
			}
			versionToolRefs = append(versionToolRefs, toolRefs)
			// Appended beside the tool refs and after the same successful
			// insert, so phase 3 can index all three against `createdVersions`.
			versionSkillRefs = append(versionSkillRefs, versionSkillReferences(v))

			createdVersions = append(createdVersions, map[string]any{
				"id":             fmt.Sprintf("%d", vID),
				"application_id": fmt.Sprintf("%d", appID),
				"name":           vName,
				"status":         "draft",
			})

			var llmParsed, startersParsed, metaParsed any
			_ = json.Unmarshal([]byte(llmJSON), &llmParsed)           // already marshaled above; can't fail
			_ = json.Unmarshal([]byte(startersJSON), &startersParsed) // already marshaled above; can't fail
			_ = json.Unmarshal([]byte(metaJSON), &metaParsed)         // already marshaled above; can't fail

			// `meta`, `variables` and `tags` were all absent from this map,
			// while the sibling fork echo and the version-detail GET both carry
			// them. The echo therefore UNDER-REPORTED the write: the wizard read
			// back an agent with no variables and no tags from a request that
			// had just stored both, and a caller comparing the echo with the
			// file it sent would conclude the import had dropped them.
			//
			// Each key is what was PERSISTED, not what was asked for:
			// `variables` and `tags` hold the rows that were actually written —
			// a failed one is in `errors.agents` and must not also appear here —
			// and `tags` carries each tag's stored id and stored `data`, which
			// is the {id, name, data} shape the version-detail GET answers with
			// (applications/handler.go, versionTagsOrEmpty). `meta` is the value
			// the column took.
			versionDetails = map[string]any{
				"id":                    fmt.Sprintf("%d", vID),
				"application_id":        fmt.Sprintf("%d", appID),
				"name":                  vName,
				"status":                "draft",
				"author_id":             fmt.Sprintf("%d", userID),
				"agent_type":            agentType,
				"instructions":          instructions,
				"welcome_message":       welcomeMsg,
				"llm_settings":          llmParsed,
				"conversation_starters": startersParsed,
				"meta":                  metaParsed,
				"variables":             importedVariables,
				"tags":                  importedTags,
				"tools":                 []any{},
			}
		}

		agentResult := map[string]any{
			"id":              fmt.Sprintf("%d", appID),
			"name":            name,
			"description":     desc,
			"owner_id":        projectID,
			"versions":        createdVersions,
			"version_details": versionDetails,
		}
		resultAgents = append(resultAgents, agentResult)
		importedAgents = append(importedAgents, importedAgentInfo{
			appID: appID, versions: versionToolRefs, skills: versionSkillRefs,
		})
	}

	// Phase 2: Import toolkits, resolving settings.import_uuid for type=application
	importUUIDToToolID := map[string]int{}
	failedToolkitImportUUIDs := map[string]bool{}

	// What each imported toolkit IS, by the row id the link phase resolves to.
	//
	// Phase 3 used to describe every link it wrote as
	// `{"type": "custom", "name": ""}`, a literal, so the import answer named
	// no toolkit at all: an import of a real `application`, `github` or `mcp`
	// toolkit reported an unnamed custom one on the version it had just
	// attached it to, while `result.toolkits` — built from the same two values,
	// one phase earlier — named it correctly. The wizard shows the version's
	// tool list back to the user as the account of what the import did, so the
	// one screen that reports the attachment could not say which toolkit was
	// attached.
	//
	// The map is filled by the SAME statement that registers the id in
	// `importUUIDToToolID`, and phase 3 resolves a tool id only through that
	// map, so the two cannot disagree about a toolkit.
	type importedToolkitSummary struct {
		name        string
		toolkitType string
	}
	importedToolkitByID := map[int]importedToolkitSummary{}

	// elitea_tools.owner_id is the DESTINATION PROJECT and is NOT NULL on a
	// schema this repository's migration corpus made — see
	// importToolkitInsertSQL for the column's meaning and the evidence for it.
	// It is the value phase 0 already resolved.
	toolkitOwnerID, toolkitOwnerErr := destinationOwnerID, destinationOwnerErr

	for _, tk := range toolkitEntries {
		tkName, _ := tk.raw["name"].(string)
		tkType, _ := tk.raw["type"].(string)
		if tkType == "" {
			tkType = "custom"
		}

		// A toolkit's settings hold its URL, its repository and everything else
		// the toolkit needs to reach its service. A `settings` key of the wrong
		// JSON type was replaced with an empty object, so the toolkit imported,
		// answered 201 and could reach nothing.
		settings, hasSettings := tk.raw["settings"].(map[string]any)
		if !hasSettings {
			if raw, present := tk.raw["settings"]; present && raw != nil {
				errorToolkits = append(errorToolkits, map[string]any{
					"index": tk.entityIdx, "name": tkName,
					"msg": "Import function has been failed: settings must be a JSON object",
				})
				failedToolkitImportUUIDs[tk.importUUID] = true
				continue
			}
			settings = map[string]any{}
		}

		// For type=application, resolve settings.import_uuid to actual agent ID
		if tkType == "application" {
			settingsImportUUID, _ := settings["import_uuid"].(string)
			if settingsImportUUID != "" {
				if resolvedAppID, found := agentImportUUIDToAppID[settingsImportUUID]; found {
					settings["application_id"] = resolvedAppID
					// Resolve version uuid too
					settingsVersionUUID, _ := settings["import_version_uuid"].(string)
					if vID, vfound := agentVersionImportUUIDToVerID[settingsVersionUUID]; vfound {
						settings["application_version_id"] = vID
					}
				} else {
					errorToolkits = append(errorToolkits, map[string]any{
						"index": tk.entityIdx,
						"name":  tkName,
						"msg":   "Unable to link toolkit_import_uuid: " + settingsImportUUID + " to any imported application",
					})
					failedToolkitImportUUIDs[tk.importUUID] = true
					continue
				}
			}
		}

		settingsJSON, settingsErr := importedJSONEncode("settings", settings)
		if settingsErr != nil {
			errorToolkits = append(errorToolkits, map[string]any{
				"index": tk.entityIdx, "name": tkName,
				"msg": "Import function has been failed: " + settingsErr.Error(),
			})
			failedToolkitImportUUIDs[tk.importUUID] = true
			continue
		}

		tkDesc, _ := tk.raw["description"].(string)
		if toolkitOwnerErr != nil {
			errorToolkits = append(errorToolkits, map[string]any{"index": tk.entityIdx, "name": tkName, "msg": "Import function has been failed: " + toolkitOwnerErr.Error()})
			failedToolkitImportUUIDs[tk.importUUID] = true
			continue
		}
		var toolID int
		err := h.pool.QueryRow(ctx, importToolkitInsertSQL(s),
			tkName, tkType, settingsJSON, toolkitOwnerID.Int64(), userID.Int64(), tkDesc).Scan(&toolID)
		if err == nil {
			if tk.importUUID != "" {
				importUUIDToToolID[tk.importUUID] = toolID
			}
			importedToolkitByID[toolID] = importedToolkitSummary{name: tkName, toolkitType: tkType}
			resultToolkits = append(resultToolkits, map[string]any{"id": strconv.Itoa(toolID), "name": tkName, "type": tkType})
		} else {
			errorToolkits = append(errorToolkits, map[string]any{"index": tk.entityIdx, "name": tkName, "msg": "Import function has been failed: " + err.Error()})
			failedToolkitImportUUIDs[tk.importUUID] = true
		}
	}

	// Phase 3: Link agent tool refs and update version_details.tools
	resultIdx := 0
	for i, ae := range agentEntries {
		info := importedAgents[i]
		if info.appID < 0 {
			continue
		}
		agentResult := resultAgents[resultIdx]
		resultIdx++

		name, _ := ae.raw["name"].(string)
		hasLinkError := false
		// #420: the insert below ran as `_, _ = h.pool.Exec(...)`. A failed link
		// was therefore silent. `hasLinkError` above cannot carry this fault. That
		// flag has one message, and the message says the toolkit was not
		// imported. A failed insert is a different fault. The toolkit IS
		// imported, and the row that joins it to the agent is missing. Each lost
		// link therefore gets its own message.
		var linkInsertFailures []string
		// The skill attachments are reported on their own channel, with the
		// AGENT's index, because that is the entity the wizard marks and the
		// channel the legacy uses (rpc/import_wizzard.py, errors['skills']).
		var skillLinkFailures []string
		var vTools []any

		// Get version IDs from the created versions
		createdVersions, _ := agentResult["versions"].([]map[string]any)

		for vIdx, toolRefs := range info.versions {
			if vIdx >= len(createdVersions) {
				break
			}
			vIDStr, _ := createdVersions[vIdx]["id"].(string)
			var vID int
			// This reads back a value phase 1 wrote with fmt.Sprintf("%d", vID)
			// on the line that created the entry, so it cannot fail. It is not
			// the class of substitution #505 repairs, which read a value that
			// came from the request.
			_, _ = fmt.Sscanf(vIDStr, "%d", &vID)

			for _, toolRef := range toolRefs {
				refUUID, _ := toolRef["import_uuid"].(string)
				if refUUID == "" {
					continue
				}
				if failedToolkitImportUUIDs[refUUID] {
					hasLinkError = true
					continue
				}
				if toolID, found := importUUIDToToolID[refUUID]; found {
					// THE DATA LOSS (#505). This read `selected_tools` as
					// `map[string]any` and wrote `{}` when the assertion
					// failed. The column holds a JSON ARRAY: `[]` is its own
					// default (001_initial.sql), the route the tool menu drives
					// writes an array of tool names (internal/api/v2/toolkits,
					// selectedToolsPayload), and the chat read only counts an
					// array (internal/db/queries/agent_chat.sql). The export
					// writes the column out as the database holds it. So every
					// selection a user had actually made failed the assertion,
					// became `{}` on import, and an export followed by an import
					// silently unchecked every tool of every toolkit.
					//
					// The value is now kept whatever its JSON type, so the
					// import reproduces the file it was given.
					selToolsJSON, selErr := importedJSONValue(toolRef, "selected_tools", "[]")
					if selErr != nil {
						linkInsertFailures = append(linkInsertFailures,
							"Import function has been failed: unable to link toolkit "+strconv.Itoa(toolID)+": "+selErr.Error())
						continue
					}
					if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
						INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
						VALUES ($1, $2, 'application', $3, $4::jsonb)`, s),
						vID, info.appID, toolID, selToolsJSON); err != nil {
						slog.ErrorContext(ctx, "import: tool link insert failed",
							"schema", s, "application_id", info.appID, "version_id", vID, "tool_id", toolID, "error", err)
						linkInsertFailures = append(linkInsertFailures,
							"Import function has been failed: unable to link toolkit "+strconv.Itoa(toolID)+": "+err.Error())
						// The response must not name a link that has no row.
						continue
					}
					// The toolkit this link points at, named as it was
					// imported. See `importedToolkitByID` in phase 2 for what
					// the two literals that stood here reported instead.
					attached := importedToolkitByID[toolID]
					vTools = append(vTools, map[string]any{
						"id":   strconv.Itoa(toolID),
						"type": attached.toolkitType,
						"name": attached.name,
					})
				} else {
					hasLinkError = true
				}
			}

			if vIdx < len(info.skills) {
				skillLinkFailures = append(skillLinkFailures,
					h.attachImportedSkills(ctx, s, vID, info.skills[vIdx], importedSkills)...)
			}
		}

		// Update version_details.tools with resolved tools.
		//
		// `vd != nil` is load-bearing. It is not defensive. `versionDetails` in
		// phase 1 is a `var` of map type. Only a successful version insert fills
		// it. An agent whose every version insert fails therefore stores a NIL
		// map in this key. The type assertion below still succeeds, because the
		// interface holds a type. The write then panics with "assignment to entry
		// in nil map". A request can reach that panic today.
		if vd, ok := agentResult["version_details"].(map[string]any); ok && vd != nil {
			if vTools == nil {
				vTools = []any{}
			}
			vd["tools"] = vTools
		}

		if hasLinkError {
			errorAgents = append(errorAgents, map[string]any{
				"index": ae.entityIdx,
				"name":  name,
				"msg":   "Import function has been failed: unable to link tools cause the later was not imported",
			})
		}
		for _, message := range linkInsertFailures {
			errorAgents = append(errorAgents, map[string]any{
				"index": ae.entityIdx,
				"name":  name,
				"msg":   message,
			})
		}
		for _, message := range skillLinkFailures {
			errorSkills = append(errorSkills, map[string]any{
				"index": ae.entityIdx,
				"name":  name,
				"msg":   "Import function has been failed: " + message,
			})
		}
	}

	// Determine status code: 400 if all failed, 207 if mixed, 201 if all succeeded
	totalErrors := len(errorAgents) + len(errorToolkits) + len(errorSkills)
	totalSuccess := len(resultAgents) + len(resultToolkits) + len(resultSkills)
	importStatus := http.StatusCreated
	if totalErrors > 0 {
		if totalSuccess == 0 {
			importStatus = http.StatusBadRequest
		} else {
			importStatus = 207
		}
	}

	writeJSON(w, importStatus, map[string]any{
		"result": importChannels(resultAgents, resultToolkits, resultSkills),
		"errors": importChannels(errorAgents, errorToolkits, errorSkills),
	})
}

// Fork copies one or more published applications into the caller's project.
//
// # It now has a route
//
// It had none. `POST /elitea_core/fork/prompt_lib/{projectID}` was registered
// on `ExportImportPost`, so this function was code with no caller and the fork
// button ran the import instead (#505). The two are not interchangeable. The
// export the fork button reads is fetched with `?fork=true`, which adds
// `owner_id`, `original_exported` and the shared-origin keys, and only this
// function reads them. Serving the route with the import meant that a forked
// agent:
//
//   - kept `llm_settings.model_project_id` pointing at the SOURCE project, so
//     the model the copy runs on belongs to a project the caller may not be a
//     member of. Only this function rewrites it to the destination;
//   - carried no `meta.parent_entity_id`, so nothing recorded where it came
//     from and the read path reported `is_forked: false` on a fork;
//   - lost every tag and every variable. That one is no longer a difference:
//     the import writes `application_variables`, and it writes the
//     `tags` / `application_version_tag_association` pair as well, because
//     those are the stores every reader of an agent looks in and an import
//     that skipped them dropped both out of the round trip entirely. The two
//     differences above remain, and are why the route needs this function.
//
// The registration is the repair. This function keeps the errors channel and
// the status rule the import uses, because the same wizard reads both.
//
// # Why every error entry carries an index
//
// `getErrorImportUUID` in `apps/elitea-ui` reads `selectedData[item.index]`
// with no guard. The entries this function used to build carried `name` and
// `error` and no index, so the first fork failure would have thrown a
// TypeError inside the wizard and stopped it. They now carry `index` and `msg`,
// which is the shape the import already writes and the wizard already maps.
func (h *Handler) Fork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The pool is the handler's own precondition, so it is checked before the
	// project id: a handler that cannot write must report that, not blame the
	// caller for the path segment.
	if h.pool == nil {
		slog.ErrorContext(ctx, "fork: "+importWriteFailed)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}

	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// A body with no `applications` array named nothing to fork and answered
	// 201 with an empty result, which the wizard reads as a completed fork.
	// An `applications` key that IS an array and IS empty keeps that answer:
	// it asks for nothing and gets nothing.
	apps, hasApps := body["applications"].([]any)
	if !hasApps {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body must carry an applications array"})
		return
	}
	if len(apps) == 0 {
		writeJSON(w, http.StatusCreated, map[string]any{
			"result": importChannels[any](nil, nil, nil),
			"errors": importChannels[any](nil, nil, nil),
		})
		return
	}

	userID, ok := importPrincipalUserID(ctx)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "an authenticated principal is required to fork"})
		return
	}

	resultAgents := make([]map[string]any, 0)
	errorAgents := make([]any, 0)
	resultSkills := make([]map[string]any, 0)
	errorSkills := make([]any, 0)

	// The skills the forked versions are attached to, copied into the caller's
	// project before the agents are written. The fork used to copy
	// `applications`, `application_versions`, `application_variables` and the
	// tags, and no row of `entity_skill_mapping`, so a forked agent came back
	// with every skill gone (#611).
	//
	// A `skills` array in the body, and not a copy out of the source schema: the
	// skill ids in an export belong to the project it came from, and
	// `p_<id>.entity_skill_mapping.skill_id` has a foreign key into
	// `p_<id>.skills`, so no cross-schema copy of those rows can hold. The
	// legacy fork reads the same key and hands it to the same import
	// (legacy/plugins/elitea_core/api/v2/fork.py).
	//
	// # The index space of every error entry
	//
	// ONE space for both arrays: the applications, then the skills. That is the
	// legacy's own numbering. `fork.py` concatenates
	// `(applications or []) + (skills or [])` into one entities list and hands
	// that list to `import_wizard`, which numbers every error and every result by
	// the position in it (`_wrap_import_error`, `_wrap_import_result`).
	//
	// Applications come first, so an agent-attributed error keeps the index it
	// already had. A skill entity carries `len(apps)` plus its own position, so
	// the entry names the skill that failed and not an unrelated agent.
	//
	// WHO READS THIS INDEX. `apps/elitea-web` is the UI, and it builds no import
	// wizard and no fork flow: no `import-wizard` slice exists under `src/`,
	// `AppShell.tsx` records the modal as "Not built", and the generated
	// `forkAgent` and `importWizard` clients have no caller — the endpoint
	// census carries `"usedBy": []` for both (shared/api/endpoints.manifest.json).
	// Only the EXPORT half is built (shared/lib/download.ts), so no shipped
	// screen reads this field and the index is free to say the true thing.
	// The old wizard in the `apps/elitea-ui` SUBMODULE does read it, through
	// `getErrorImportUUID`, which indexes the applications alone and dereferences
	// the result with no guard, so an index above `len(apps)-1` throws there.
	// That wizard is not the product and this repository does not change it. A
	// port into `apps/elitea-web` must pass the whole concatenation.
	//
	// # A body that names no skills at all
	//
	// The route used to answer 201 for `{applications: [...]}` alone, and a
	// current export's version entries now carry skill references. A client that
	// forwards the applications and drops the document's top-level `skills`
	// array therefore asks for skills it did not send. That fork IS incomplete,
	// so it still answers 207 and still reports — the whole point of #611 is that
	// a lost attachment is never silent. What it must not do is describe the
	// cause as though the file were broken, so `bodyNamesSkills` below separates
	// "you sent no skills" from "the skill you sent could not be linked".
	importedSkills := map[string]importedSkill{}
	_, bodyNamesSkills := body["skills"]
	// The destination project, resolved ONCE for the skills and for the agents.
	// `owner_id` on `skills` and on `applications` is the OWNING PROJECT (#533),
	// and both come from the same immutable path segment.
	destinationOwnerID, destinationOwnerErr := tenantOwnerID(projectID)
	for skillPosition, raw := range toAnySlice(body["skills"]) {
		// The position in the concatenation the wizard resolves against: the
		// applications it sent, followed by the skills it sent.
		skillErrorIndex := len(apps) + skillPosition
		skill, isMap := raw.(map[string]any)
		if !isMap {
			errorSkills = append(errorSkills, map[string]any{
				"index": skillErrorIndex, "name": "",
				"msg": fmt.Sprintf(
					"Fork function has been failed: skills entry %d is not a JSON object", skillPosition),
			})
			continue
		}
		skillName, _ := skill["name"].(string)
		if skillName == "" {
			skillName = fmt.Sprintf("skills entry %d", skillPosition)
		}
		if destinationOwnerErr != nil {
			errorSkills = append(errorSkills, map[string]any{
				"index": skillErrorIndex, "name": skillName,
				"msg": "Fork function has been failed: " + destinationOwnerErr.Error(),
			})
			continue
		}
		created, err := h.importSkill(ctx, s, destinationOwnerID, userID, skill)
		// A skill can be written and still fail, because the row is inserted
		// before its versions are. It is then reported and registered rather
		// than dropped, for the reason the import states at phase 0.
		if created.id != 0 {
			if importUUID, _ := skill["import_uuid"].(string); importUUID != "" {
				importedSkills[importUUID] = created
			}
			resultSkills = append(resultSkills, map[string]any{
				"id": strconv.Itoa(created.id), "name": skillName,
			})
		}
		if err != nil {
			slog.ErrorContext(ctx, "fork: skill import failed",
				"schema", s, "name", skillName, "skill_id", created.id, "error", err)
			message := "Fork function has been failed: unable to fork skill " + skillName + ": " + err.Error()
			if created.id != 0 {
				message = fmt.Sprintf(
					"Fork function has been failed: skill %d was written and is incomplete: %s",
					created.id, err.Error())
			}
			errorSkills = append(errorSkills, map[string]any{
				"index": skillErrorIndex, "name": skillName, "msg": message,
			})
		}
	}

	for entityIdx, appRaw := range apps {
		app, ok := appRaw.(map[string]any)
		if !ok {
			errorAgents = append(errorAgents, map[string]any{
				"index": entityIdx, "name": "",
				"msg": "Fork function has been failed: the entry is not a JSON object",
			})
			continue
		}
		name, _ := app["name"].(string)
		desc, _ := app["description"].(string)

		if destinationOwnerErr != nil {
			errorAgents = append(errorAgents, map[string]any{
				"index": entityIdx, "name": name,
				"msg": "Fork function has been failed: " + destinationOwnerErr.Error(),
			})
			continue
		}
		// `applications.owner_id` is the DESTINATION PROJECT (#533). This
		// statement wrote the caller user id, and the same route reads the
		// SOURCE row's owner_id back as `parent_project_id` below. One column
		// held both kinds of number in one request.
		var appID int
		err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.applications (name, description, owner_id)
			VALUES ($1, $2, $3) RETURNING id`, s),
			name, desc, destinationOwnerID.Int64()).Scan(&appID)
		if err != nil {
			slog.ErrorContext(ctx, "fork: application insert failed", "schema", s, "name", name, "error", err)
			errorAgents = append(errorAgents, map[string]any{
				"index": entityIdx, "name": name,
				"msg": "Fork function has been failed: " + err.Error(),
			})
			continue
		}

		versions, _ := app["versions"].([]any)
		var createdVersionID int
		var versionDetails map[string]any
		createdVersions := make([]map[string]any, 0)

		for _, vRaw := range versions {
			v, ok := vRaw.(map[string]any)
			if !ok {
				errorAgents = append(errorAgents, map[string]any{
					"index": entityIdx, "name": name,
					"msg": "Fork function has been failed: a version entry is not a JSON object",
				})
				continue
			}
			vName, _ := v["name"].(string)
			if vName == "" {
				vName = "latest"
			}
			agentType, _ := v["agent_type"].(string)
			if agentType == "" {
				agentType = "openai"
			}
			instructions, _ := v["instructions"].(string)
			welcomeMsg, _ := v["welcome_message"].(string)
			llmSettings, hasLLM := v["llm_settings"].(map[string]any)
			if !hasLLM {
				if raw, present := v["llm_settings"]; present && raw != nil {
					errorAgents = append(errorAgents, map[string]any{
						"index": entityIdx, "name": name,
						"msg": "Fork function has been failed: unable to fork version " + vName + ": llm_settings must be a JSON object",
					})
					continue
				}
				llmSettings = map[string]any{}
			}
			// Override model_project_id to target project
			llmSettings["model_project_id"] = projectID

			llmJSON, err := importedJSONEncode("llm_settings", llmSettings)
			if err != nil {
				errorAgents = append(errorAgents, map[string]any{
					"index": entityIdx, "name": name,
					"msg": "Fork function has been failed: unable to fork version " + vName + ": " + err.Error(),
				})
				continue
			}
			startersJSON, err := importedJSONArray(v, "conversation_starters")
			if err != nil {
				errorAgents = append(errorAgents, map[string]any{
					"index": entityIdx, "name": name,
					"msg": "Fork function has been failed: unable to fork version " + vName + ": " + err.Error(),
				})
				continue
			}

			// Build meta with fork info
			metaIn, hasMeta := v["meta"].(map[string]any)
			if !hasMeta {
				if raw, present := v["meta"]; present && raw != nil {
					errorAgents = append(errorAgents, map[string]any{
						"index": entityIdx, "name": name,
						"msg": "Fork function has been failed: unable to fork version " + vName + ": meta must be a JSON object",
					})
					continue
				}
				metaIn = map[string]any{}
			}
			forkMeta := map[string]any{}
			for k, mv := range metaIn {
				forkMeta[k] = mv
			}
			// Set parent info
			sourceAppID, _ := app["id"].(string)
			sourceOwnerID, _ := app["owner_id"].(string)
			forkMeta["parent_entity_id"] = sourceAppID
			forkMeta["parent_project_id"] = sourceOwnerID
			forkMeta["parent_author_id"] = fmt.Sprintf("%d", userID)
			if _, hasIcon := forkMeta["icon_meta"]; !hasIcon {
				forkMeta["icon_meta"] = map[string]any{}
			}
			if _, hasStep := forkMeta["step_limit"]; !hasStep {
				forkMeta["step_limit"] = nil
			}

			metaJSON, err := importedJSONEncode("meta", forkMeta)
			if err != nil {
				errorAgents = append(errorAgents, map[string]any{
					"index": entityIdx, "name": name,
					"msg": "Fork function has been failed: unable to fork version " + vName + ": " + err.Error(),
				})
				continue
			}

			var vID int
			// The bare `continue` this replaces dropped the version and told
			// nobody. The application row was already written, so a fork whose
			// every version failed answered 201 and produced an agent with no
			// version at all.
			if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
				INSERT INTO %s.application_versions (application_id, name, status, agent_type, instructions, welcome_message, llm_settings, conversation_starters, author_id, meta, pipeline_settings)
				VALUES ($1, $2, 'draft', $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9::jsonb, '{}'::jsonb) RETURNING id`, s),
				appID, vName, agentType, instructions, welcomeMsg, llmJSON, startersJSON, userID.Int64(), metaJSON).Scan(&vID); err != nil {
				slog.ErrorContext(ctx, "fork: application version insert failed",
					"schema", s, "application_id", appID, "version_name", vName, "error", err)
				errorAgents = append(errorAgents, map[string]any{
					"index": entityIdx, "name": name,
					"msg": "Fork function has been failed: unable to fork version " + vName + ": " + err.Error(),
				})
				continue
			}
			createdVersionID = vID

			// Insert variables. A variable is a value the agent needs to run,
			// and these two statements were written `_, _ = h.pool.Exec(...)`
			// under the words "best-effort insert". A lost variable left a fork
			// that answers 201 and an agent that fails on its first turn.
			if vars, ok := v["variables"].([]any); ok {
				for _, varRaw := range vars {
					varMap, _ := varRaw.(map[string]any)
					if varMap == nil {
						continue
					}
					varName, _ := varMap["name"].(string)
					varValue, _ := varMap["value"].(string)
					if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
						INSERT INTO %s.application_variables (application_version_id, name, value) VALUES ($1, $2, $3)`, s),
						vID, varName, varValue); err != nil {
						slog.ErrorContext(ctx, "fork: application variable insert failed",
							"schema", s, "version_id", vID, "variable", varName, "error", err)
						errorAgents = append(errorAgents, map[string]any{
							"index": entityIdx, "name": name,
							"msg": "Fork function has been failed: unable to fork variable " + varName + ": " + err.Error(),
						})
					}
				}
			}

			// Insert tags
			if tagsList, ok := v["tags"].([]any); ok {
				for _, tagRaw := range tagsList {
					tagMap, _ := tagRaw.(map[string]any)
					if tagMap == nil {
						continue
					}
					tagName, _ := tagMap["name"].(string)
					tagDataJSON, err := importedJSONValue(tagMap, "data", "{}")
					if err != nil {
						errorAgents = append(errorAgents, map[string]any{
							"index": entityIdx, "name": name,
							"msg": "Fork function has been failed: unable to fork tag " + tagName + ": " + err.Error(),
						})
						continue
					}
					var tagID int
					// Upsert tag
					if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
						INSERT INTO %s.tags (name, data) VALUES ($1, $2::jsonb)
						ON CONFLICT (name) DO UPDATE SET data = EXCLUDED.data
						RETURNING id`, s), tagName, tagDataJSON).Scan(&tagID); err != nil {
						slog.ErrorContext(ctx, "fork: tag upsert failed",
							"schema", s, "version_id", vID, "tag", tagName, "error", err)
						errorAgents = append(errorAgents, map[string]any{
							"index": entityIdx, "name": name,
							"msg": "Fork function has been failed: unable to fork tag " + tagName + ": " + err.Error(),
						})
						continue
					}
					if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
						INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)
						ON CONFLICT DO NOTHING`, s), vID, tagID); err != nil {
						slog.ErrorContext(ctx, "fork: tag association insert failed",
							"schema", s, "version_id", vID, "tag", tagName, "error", err)
						errorAgents = append(errorAgents, map[string]any{
							"index": entityIdx, "name": name,
							"msg": "Fork function has been failed: unable to fork tag " + tagName + ": " + err.Error(),
						})
					}
				}
			}

			// Attach the skills this version references. Every reference that
			// cannot be attached is reported, on the agent's own index, so a
			// fork that came back with fewer skills than the file says so.
			skillReferences := versionSkillReferences(v)
			switch {
			case len(skillReferences) > 0 && !bodyNamesSkills:
				// One message for the version, naming the cause, rather than one
				// per reference saying each skill "is not among the imported
				// skills" — which reads as a broken file and is not.
				errorSkills = append(errorSkills, map[string]any{
					"index": entityIdx, "name": name,
					"msg": fmt.Sprintf("Fork function has been failed: the request carries no skills array, "+
						"so the %d skills of version %s were not forked", len(skillReferences), vName),
				})
			default:
				for _, message := range h.attachImportedSkills(ctx, s, vID, skillReferences, importedSkills) {
					errorSkills = append(errorSkills, map[string]any{
						"index": entityIdx, "name": name,
						"msg": "Fork function has been failed: " + message,
					})
				}
			}

			createdVersions = append(createdVersions, map[string]any{
				"id":             fmt.Sprintf("%d", vID),
				"application_id": fmt.Sprintf("%d", appID),
				"name":           vName,
				"status":         "draft",
			})

			// Build version_details response
			var starters any
			_ = json.Unmarshal([]byte(startersJSON), &starters) // already marshaled above; can't fail

			// Rebuild variables list for response
			respVars := make([]map[string]any, 0)
			if vars, ok := v["variables"].([]any); ok {
				for _, varRaw := range vars {
					if varMap, ok := varRaw.(map[string]any); ok {
						respVars = append(respVars, map[string]any{"name": varMap["name"], "value": varMap["value"]})
					}
				}
			}

			// Rebuild tags list for response
			respTags := make([]map[string]any, 0)
			if tagsList, ok := v["tags"].([]any); ok {
				for _, tagRaw := range tagsList {
					if tagMap, ok := tagRaw.(map[string]any); ok {
						respTags = append(respTags, map[string]any{"name": tagMap["name"], "data": tagMap["data"]})
					}
				}
			}

			versionDetails = map[string]any{
				"id":                    fmt.Sprintf("%d", vID),
				"application_id":        fmt.Sprintf("%d", appID),
				"name":                  vName,
				"status":                "draft",
				"author_id":             fmt.Sprintf("%d", userID),
				"agent_type":            agentType,
				"instructions":          instructions,
				"welcome_message":       welcomeMsg,
				"llm_settings":          llmSettings,
				"conversation_starters": starters,
				"meta":                  forkMeta,
				"is_forked":             true,
				"variables":             respVars,
				"tags":                  respTags,
				"tools":                 []any{},
			}
		}

		agentResult := map[string]any{
			"id":              fmt.Sprintf("%d", appID),
			"name":            name,
			"description":     desc,
			"owner_id":        projectID,
			"webhook_secret":  nil,
			"versions":        createdVersions,
			"version_details": versionDetails,
		}
		if createdVersionID > 0 {
			agentResult["version_id"] = fmt.Sprintf("%d", createdVersionID)
		}
		resultAgents = append(resultAgents, agentResult)
	}

	// The status rule the import already uses, for the same reason and for the
	// same reader: 400 when nothing was forked, 207 when part of it was, 201
	// when all of it was. The wizard reads a 2xx body and a 400 body through
	// the same branch, so an error entry reaches the user either way.
	forkStatus := http.StatusCreated
	if len(errorAgents)+len(errorSkills) > 0 {
		if len(resultAgents)+len(resultSkills) == 0 {
			forkStatus = http.StatusBadRequest
		} else {
			forkStatus = http.StatusMultiStatus
		}
	}

	writeJSON(w, forkStatus, map[string]any{
		// A fork writes no toolkit, so its toolkits channel is always empty.
		// It is still carried: see importChannels.
		"result": importChannels(resultAgents, nil, resultSkills),
		"errors": importChannels(errorAgents, nil, errorSkills),
	})
}

func (h *Handler) ExportImportGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// A handler with no pool cannot read anything. It answered
	// 200 {"ok": true}, which is a document with no `applications` key at all,
	// and the export button saved it as the agent's backup file (#505).
	if h.pool == nil {
		slog.ErrorContext(ctx, "export: "+exportReadFailed, "reason", "the handler holds no database pool")
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}

	projectID := chi.URLParam(r, "projectID")
	entityID := chi.URLParam(r, "entityID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	// entityID is a row id, and it leaves this handler again in the
	// Content-Disposition filename of the `as_file` and markdown branches.
	// Refuse anything that is not a plain decimal id HERE, so caller text
	// never reaches a response header at all. The answer is the 404 this
	// handler already gave such an id: the query binds entityID against an
	// integer column, so a non-numeric one raised a driver error that the
	// read below reported as "application not found". The status is
	// unchanged; only the point of refusal moves.
	if !tenantschema.Valid(entityID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "application not found"})
		return
	}

	var name, desc, appUUID string
	var ownerID int
	var sharedID, sharedOwnerID *int
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT name, COALESCE(description, ''), uuid::text, owner_id, shared_id, shared_owner_id
		FROM %s.applications WHERE id = $1`, s), entityID).Scan(&name, &desc, &appUUID, &ownerID, &sharedID, &sharedOwnerID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "application not found"})
		return
	}

	// Determine app type from its versions. A lost read used to leave
	// hasPipeline false under a comment that called that safe. It is not: the
	// file then says a pipeline is an agent, and the import that reads the file
	// builds the wrong kind of entity.
	appType := "agent"
	var hasPipeline bool
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND agent_type = 'pipeline')`, s),
		entityID).Scan(&hasPipeline); err != nil {
		slog.ErrorContext(ctx, "export: "+exportReadFailed,
			"schema", s, "application_id", entityID, "read", "application type", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}
	if hasPipeline {
		appType = "pipeline"
	}

	// The toolkits, and the versions with their tools, variables and tags. Each
	// read used to run under `if err == nil`, drop a row it could not scan, and
	// never read rows.Err() — the three-part pattern #439 repaired elsewhere.
	// An export that lost a version answered 200 and served the rest, so the
	// operator kept a backup file with a version missing from it.
	toolkits, toolkitMap, err := h.exportedToolkits(ctx, s, entityID)
	if err != nil {
		slog.ErrorContext(ctx, "export: "+exportReadFailed,
			"schema", s, "application_id", entityID, "read", "toolkits", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}
	versions, err := h.exportedVersions(ctx, s, entityID, toolkitMap)
	if err != nil {
		slog.ErrorContext(ctx, "export: "+exportReadFailed,
			"schema", s, "application_id", entityID, "read", "versions", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}

	isFork := strings.EqualFold(r.URL.Query().Get("fork"), "true")
	if isFork && len(versions) > 0 {
		versions = versions[len(versions)-1:]
	}

	// AFTER the fork branch, so the skills the document carries are the skills
	// the versions the document carries are attached to. It also rewrites each
	// version reference to name its skill by `import_uuid` — see the skill
	// section of export_import.go.
	skills, err := h.exportedSkills(ctx, s, versions)
	if err != nil {
		slog.ErrorContext(ctx, "export: "+exportReadFailed,
			"schema", s, "application_id", entityID, "read", "skills", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": exportReadFailed})
		return
	}

	appExport := map[string]any{
		"id":          entityID,
		"name":        name,
		"description": desc,
		"type":        appType,
		"import_uuid": appUUID,
		"versions":    versions,
	}
	if isFork {
		appExport["owner_id"] = fmt.Sprintf("%d", ownerID)
		appExport["original_exported"] = true
		if sharedID != nil {
			appExport["shared_id"] = fmt.Sprintf("%d", *sharedID)
		} else {
			appExport["shared_id"] = nil
		}
		if sharedOwnerID != nil {
			appExport["shared_owner_id"] = fmt.Sprintf("%d", *sharedOwnerID)
		} else {
			appExport["shared_owner_id"] = nil
		}
	}

	result := map[string]any{
		"ok":           true,
		"applications": []map[string]any{appExport},
		"toolkits":     toolkits,
	}
	// Omitted when the agent has no skills, the way the legacy omits it
	// (export_import.py:210-211). The import wizard turns EVERY top-level array
	// into a row the user selects, so an empty one would show a "skills" group
	// with nothing in it.
	if len(skills) > 0 {
		result["skills"] = skills
	}

	// `format=md` renders the SAME document as markdown — one file per version,
	// zipped when there is more than one. See export_markdown.go, including the
	// two frontmatter keys this service has no producer for
	// (nested_agents/nested_pipelines, pipeline_settings).
	//
	// Checked before `as_file`, which the markdown branch does not honour: it
	// always sends a Content-Disposition, because a markdown export is a FILE
	// by construction — that is what the client asks for and what the legacy
	// sent.
	if strings.EqualFold(r.URL.Query().Get("format"), "md") {
		writeMarkdownExport(w, entityID, result)
		return
	}

	if strings.EqualFold(r.URL.Query().Get("as_file"), "true") {
		w.Header().Set("Content-Type", "application/json")
		// entityID reaches this header from the URL, so it is caller text. Send
		// it through the same helper the markdown export uses: it replaces the
		// quote, the backslash and every control character, so the value cannot
		// leave the quoted filename. `path.Base` drops a path the caller built
		// out of separators. The markdown branch above did both and this one
		// did neither (CodeQL go/reflected-xss, alerts 100 and 101, which name
		// this parameter as the source).
		w.Header().Set("Content-Disposition", contentDispositionAttachment(
			fmt.Sprintf("elitea_export_%s.json", entityID)))
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
		_ = json.NewEncoder(w).Encode(result) // response writer; connection already committed
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) ExportConverter(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional; ignore decode errors
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "converted": body})
}

func (h *Handler) UpdateNotification(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	notificationID := chi.URLParam(r, "notificationID")
	ctx := r.Context()

	user, ok := auth.UserFromContext(ctx)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	switch r.Method {
	case http.MethodPut:
		if notificationID != "" {
			_, _ = h.pool.Exec(ctx, `UPDATE centry.notifications SET is_seen = true WHERE id = $1 AND user_id = $2`, notificationID, user.ID)
		} else {
			_, _ = h.pool.Exec(ctx, `UPDATE centry.notifications SET is_seen = true WHERE project_id = $1 AND user_id = $2`, projectID, user.ID)
		}
	case http.MethodDelete:
		if notificationID != "" {
			_, _ = h.pool.Exec(ctx, `DELETE FROM centry.notifications WHERE id = $1 AND user_id = $2`, notificationID, user.ID)
		} else {
			_, _ = h.pool.Exec(ctx, `DELETE FROM centry.notifications WHERE project_id = $1 AND user_id = $2`, projectID, user.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// projectIconKeyPrefix separates project icons from the application icons
// UploadIcon writes, inside the one `icons` bucket both share.
//
// A shared bucket, not a bucket of their own, because the ONLY route that
// serves stored icons back to a browser is
// `GET /icons/{projectID}/{filename}` (DownloadIcon, mounted unauthenticated
// at router.go:806 — a browser <img src> carries no Authorization header), and
// that route reads `iconBucket`. An icon written anywhere else would be stored
// and then unreachable, which is the exact failure S20b was written to fix.
// The `{filename}` parameter is one path segment, so the separation cannot be
// a key directory either — it is a filename prefix.
const projectIconKeyPrefix = "pi_"

// iconStorageUnavailable answers the one condition all three project-icon
// routes share: no object store is composed, so there is nowhere to put a
// file, nothing to list and nothing to delete.
//
// 501 rather than 500 for the analytics reason
// (internal/api/v2/analytics/handler.go:64-78): a store that was never
// configured is a property of the deployment, identical for every retry.
func iconStorageUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": "project icon storage is not available on this deployment",
		"code":  "icon_storage_not_configured",
		"detail": "uploaded project icons need an object store " +
			"(STORAGE_BACKEND / STORAGE_CONTAINER)",
	})
}

// ListProjectIcons returns the icons this project has actually uploaded.
//
// It used to return a hardcoded empty page, so the icon picker's "uploaded"
// grid was permanently empty however many icons had been uploaded — and the
// grid gave no sign that it had never looked.
//
// The response key is `rows`, not the `items` the stub used: `rows`/`total` is
// the shape the caller unwraps
// (apps/elitea-web/src/entities/project/api/projectContextApi.ts,
// fetchProjectIcons via unwrapListPage) and the shape pylon's project_icon
// listing answers.
func (h *Handler) ListProjectIcons(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	if h.store == nil {
		iconStorageUnavailable(w)
		return
	}

	bucket, err := storage.NewBucketRef(projectID, iconBucket)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	page, err := h.store.List(r.Context(), storage.ListQuery{
		Bucket:    bucket,
		KeyPrefix: projectIconKeyPrefix,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "list project icons", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "failed to list project icons",
			"code":  "icon_list_failed",
		})
		return
	}

	names := make([]string, 0, len(page.Objects))
	for _, object := range page.Objects {
		names = append(names, object.Key)
	}
	// social/rpc/icons.py:get_icons_list sorts by name BEFORE it slices, so the
	// page a caller asks for is stable between two calls. An unsorted listing
	// makes `skip` meaningless: the second page may repeat or drop a row
	// depending on the order the backend happened to answer in.
	sort.Strings(names)

	rows := make([]map[string]any, 0, len(names))
	for _, name := range names {
		rows = append(rows, map[string]any{
			"name": name,
			"url":  fmt.Sprintf("/icons/%s/%s", projectID, name),
		})
	}
	// `skip` and `limit` were accepted and DISCARDED. The picker sends both
	// (apps/elitea-web fetchProjectIcons sends limit=200&skip=0) and pylon
	// honours both, so a project with more icons than one page could never
	// reach the rest of them: every request answered page one and called it the
	// whole set. `total` stays the count of every icon, not of the page —
	// get_icons_list computes it before the slice, and a client paging on it
	// needs the full figure.
	skip := safeIntOr(r.URL.Query().Get("skip"), 0)
	limit := safeIntOr(r.URL.Query().Get("limit"), defaultProjectIconPageSize)
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":  pageOfIconRows(rows, skip, limit),
		"total": len(rows),
	})
}

// CreateProjectIcon stores the uploaded file.
//
// It used to parse the multipart form, DISCARD it, and answer 200 with a
// fabricated `/app/project_icon/{projectID}/{name}` URL that nothing served
// and nothing had ever written to — the upload reported success, the picker
// then listed nothing, and no error appeared anywhere.
//
// The stored name is generated rather than taken from the upload, exactly as
// UploadIcon does: the name becomes a path segment of a public,
// unauthenticated URL and the key of a stored object, and the extension
// allowlist in safeIconExtension is what keeps DownloadIcon from serving
// attacker-chosen content types from the app's own origin (see the comment on
// allowedIconExtensions).
func (h *Handler) CreateProjectIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	_ = r.ParseMultipartForm(512 * 1024) // ignore parse errors; FormFile reports them
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "a file part is required",
			"code":  "missing_file",
		})
		return
	}
	defer func() { _ = file.Close() }()

	if h.store == nil {
		iconStorageUnavailable(w)
		return
	}

	// project_icon.py measures the upload and refuses it over MAX_FILE_SIZE_KB.
	// This route had NO cap: ParseMultipartForm's argument is a memory budget,
	// not a limit, and its error was discarded, so an arbitrarily large file was
	// streamed straight into the store under a permission a project editor
	// holds.
	if header.Size > maxProjectIconBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("File size exceeds %d KB", maxProjectIconBytes/1024),
			"code":  "icon_too_large",
		})
		return
	}

	name := projectIconKeyPrefix + generateID() + safeIconExtension(header.Filename)
	ref, err := storage.NewObjectRef(projectID, iconBucket, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}

	contentType := mime.TypeByExtension(safeIconExtension(header.Filename))
	// LimitReader, not the declared header.Size: that number is what the
	// multipart part CLAIMS, and the cap has to hold against a part that lies.
	// The extra byte is what makes an over-cap body detectable instead of
	// silently truncated into storage.
	limited := io.LimitReader(file, maxProjectIconBytes+1)
	info, err := h.store.Put(r.Context(), ref, limited, storage.PutOptions{
		ContentType:   contentType,
		ContentLength: -1,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "create project icon", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "failed to save project icon",
			"code":  "icon_write_failed",
		})
		return
	}
	if info.Size > maxProjectIconBytes {
		if deleteErr := h.store.Delete(r.Context(), ref); deleteErr != nil {
			slog.ErrorContext(r.Context(), "remove oversized project icon", "error", deleteErr)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("File size exceeds %d KB", maxProjectIconBytes/1024),
			"code":  "icon_too_large",
		})
		return
	}

	// The full icon_meta object, not the two keys this route used to answer.
	// social_save_image returns five, the project-info PUT stores whatever the
	// picker hands back, and the settings dialog shows the two sizes — so a
	// two-key response made the dialog render blanks and stored an icon_meta
	// that no longer described the file.
	width := clampProjectIconDimension(safeIntOr(r.FormValue("width"), defaultIconDimension))
	height := clampProjectIconDimension(safeIntOr(r.FormValue("height"), defaultIconDimension))
	writeJSON(w, http.StatusOK, map[string]any{
		"name":                name,
		"url":                 fmt.Sprintf("/icons/%s/%s", projectID, name),
		"size":                fmt.Sprintf("%dx%d", width, height),
		"initial_file_size":   sizeofFmt(header.Size),
		"resulting_file_size": sizeofFmt(info.Size),
	})
}

// maxProjectIconBytes is project_icon.py's MAX_FILE_SIZE_KB (512), in bytes.
const maxProjectIconBytes = 512 * 1024

// maxProjectIconDimension is project_icon.py's MAX_DIMENSION. It is 512, NOT
// the 64 the skill and agent icons use — a project icon is rendered larger.
const maxProjectIconDimension = 512

// defaultIconDimension is the box every icon route falls back to when the form
// names none, in pylon and here.
const defaultIconDimension = 64

// defaultProjectIconPageSize is project_icon.py's `limit` default.
const defaultProjectIconPageSize = 200

// clampProjectIconDimension is project_icon.py's min(int(...), MAX_DIMENSION)
// with the lower bound the skill route already applies: a zero or negative box
// is not a box.
func clampProjectIconDimension(value int) int {
	if value < 1 {
		return 1
	}
	if value > maxProjectIconDimension {
		return maxProjectIconDimension
	}
	return value
}

// DeleteProjectIcon removes the stored object.
//
// It used to answer 204 and do nothing at all, so a deleted icon reappeared on
// the next listing.
//
// A 204 for an object that was already absent is deliberate — Delete is
// documented idempotent — but a store that could not be reached is reported,
// because "gone" and "we could not tell" are different answers.
func (h *Handler) DeleteProjectIcon(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")

	if h.store == nil {
		iconStorageUnavailable(w)
		return
	}
	if !validIconPathSegment(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid icon name"})
		return
	}

	ref, err := storage.NewObjectRef(projectID, iconBucket, name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project"})
		return
	}
	if err := h.store.Delete(r.Context(), ref); err != nil {
		slog.ErrorContext(r.Context(), "delete project icon", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "failed to delete project icon",
			"code":  "icon_delete_failed",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateMCPProxyURL(rawURL string) (*url.URL, error) {
	endpoint, err := url.ParseRequestURI(rawURL)
	if err != nil ||
		!endpoint.IsAbs() ||
		endpoint.Host == "" ||
		endpoint.User != nil ||
		endpoint.Fragment != "" ||
		endpoint.Opaque != "" {
		return nil, fmt.Errorf("invalid MCP endpoint")
	}

	scheme := strings.ToLower(endpoint.Scheme)
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	if host == "" {
		return nil, fmt.Errorf("invalid MCP endpoint host")
	}
	if hasMCPProxyDotPathSegment(endpoint.Path) {
		return nil, fmt.Errorf("invalid MCP endpoint path")
	}

	switch scheme {
	case "https":
		// Custom HTTPS provider hosts are intentionally supported.
	case "http":
		if !isLoopbackMCPHost(host) {
			return nil, fmt.Errorf("plain HTTP MCP endpoints must use a loopback host")
		}
	default:
		return nil, fmt.Errorf("unsupported MCP endpoint scheme")
	}

	endpoint.Scheme = scheme
	return endpoint, nil
}

func hasMCPProxyDotPathSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func isLoopbackMCPHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameMCPProxyOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(first.Hostname(), "."), strings.TrimSuffix(second.Hostname(), ".")) &&
		effectiveURLPort(first) == effectiveURLPort(second)
}

func effectiveURLPort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return "443"
	}
	return "80"
}

func (h *Handler) doMCPProxyRequest(req *http.Request) (*http.Response, error) {
	client := h.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	origin := req.URL
	safeClient := *client
	inheritedRedirectPolicy := client.CheckRedirect
	safeClient.CheckRedirect = func(redirected *http.Request, via []*http.Request) error {
		validated, err := validateMCPProxyURL(redirected.URL.String())
		if err != nil || !sameMCPProxyOrigin(origin, validated) {
			return fmt.Errorf("MCP endpoint redirect is not allowed")
		}
		if inheritedRedirectPolicy != nil {
			return inheritedRedirectPolicy(redirected, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return safeClient.Do(req)
}

func (h *Handler) MCPOAuthProxy(w http.ResponseWriter, r *http.Request) {
	h.mcpOAuthProxy(w, r)
}

func (h *Handler) MCPDCRProxy(w http.ResponseWriter, r *http.Request) {
	h.mcpDCRProxy(w, r)
}

// MCPSyncTools discovers the tools of a remote MCP server and records them.
//
// NOTE(#126): it used to forward to an injected MCPToolSyncer, whose only
// implementation was the prototype indexersvc Redis RPC client. That client was
// never assigned in any composition root, so this endpoint always answered 503
// — the injection seam was decoration on an unconditional failure.
//
// Issue 335 gives it a real backend. The discovery is an outbound HTTP exchange
// with the MCP server named in the body, over the streamable-HTTP transport,
// through the same validated sender the OAuth and DCR proxies in this file
// already use. pylon reaches the same servers through a task node that imports
// the Python SDK; the protocol on the wire is the same.
//
// The result is also WRITTEN to `elitea_mcp.registered_servers`, which is what
// `GET /elitea_core/tools_list/{projectID}` reads. That write is the reason the
// listing endpoint can stop answering 501: this is the registration path, so
// there is no second source of the same data.
//
// The response shape is pylon's `McpSyncToolsResponseModel`, flat rather than
// wrapped in `{"result": …}`. The web client accepts either
// (`useGetRemoteMcpTools.ts:192` reads `response.result ?? response`), and flat
// is the truthful one here: pylon nests because the value arrives from an
// asynchronous task, and this path has no task.
func (h *Handler) MCPSyncTools(w http.ResponseWriter, r *http.Request) {
	// Gated BEFORE the body is decoded, so a disabled deployment answers the
	// same 403 to a well-formed request and a malformed one. Deciding after the
	// decode would let a caller distinguish the two and learn that the route
	// exists and parses MCP payloads.
	if !h.requireMCPEnabled(w, r) {
		return
	}

	var body struct {
		URL         string                       `json:"url"`
		Headers     map[string]string            `json:"headers"`
		Timeout     int                          `json:"timeout"`
		ToolkitType string                       `json:"toolkit_type"`
		MCPTokens   map[string]mcpDiscoveryToken `json:"mcp_tokens"`
	}
	if !decodeMCPProxyRequest(w, r, &body) {
		return
	}

	// `ssl_verify` is read from the body by pylon and honoured. It is ignored
	// here, and TLS verification always applies: AGENTS.md makes verification
	// mandatory outside an explicitly isolated test. A caller that needs a
	// private certificate authority configures the service trust bundle
	// (WithHTTPClient), which is auditable, rather than switching verification
	// off per request, which is not.
	// A pre-built MCP toolkit leaves the platform to supply the endpoint and the
	// credentials, which is what pylon's `resolve_mcp_prebuilt_settings` does
	// from its plugin configuration. The catalogue is a table here (shared
	// migration 0094); the resolution fills only fields the caller left empty,
	// so a toolkit that carries its own URL still wins.
	if mcpregistry.IsPrebuiltToolkitType(body.ToolkitType) {
		resolved, err := h.resolvePrebuiltSettings(r.Context(), map[string]any{
			"url":     body.URL,
			"headers": headersAsAny(body.Headers),
			"timeout": body.Timeout,
		}, body.ToolkitType)
		if err != nil {
			// An unreadable catalogue is not an uncatalogued toolkit. Proceeding
			// would open the connection without the credentials the operator
			// configured, and the remote server's 401 would name none of this.
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"success": false,
				"error":   "the pre-built MCP catalogue could not be read",
			})
			return
		}
		body.URL = stringSetting(resolved, "url", body.URL)
		body.Headers = headerSettings(resolved, body.Headers)
		body.Timeout = intSetting(resolved, "timeout", body.Timeout)
	}

	if strings.TrimSpace(body.URL) == "" {
		// Still empty: either the toolkit is not a pre-built one, or no
		// catalogue entry defines it. The URL must be supplied rather than
		// invented, and the message names the catalogue so an operator who
		// expected it to be filled knows where to look.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error": "url is required: no pre-built MCP catalogue entry supplies one for this " +
				"toolkit type",
		})
		return
	}
	endpoint, err := validateMCPProxyURL(body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid_mcp_url"})
		return
	}

	timeout := time.Duration(body.Timeout) * time.Second
	if timeout <= 0 || timeout > mcpSyncMaxTimeout {
		timeout = mcpSyncDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	tools, err := mcpregistry.NewDiscoverer(h.doMCPProxyRequest).
		Discover(ctx, endpoint.String(), mcpDiscoveryHeaders(body.Headers, body.MCPTokens, endpoint.String(), body.ToolkitType))
	if err != nil {
		if metadata, authErr := h.mcpDiscoveryAuthorization(ctx, endpoint, err); authErr == nil {
			writeJSON(w, http.StatusOK, map[string]any{"success": false, "requires_authorization": true, "response_metadata": metadata})
			return
		}
		// The stated cause is the remote server's, not this service's, so it is
		// reported as a failed discovery rather than a fault here. The web
		// client renders `error` directly.
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    false,
			"error":      "MCP tool discovery failed",
			"server_url": endpoint.String(),
		})
		return
	}

	// The registration is stored under the toolkit type, because that is the
	// name a worker matches a toolkit against: the SDK looks the toolkit's
	// `type` up among the server names that `tools_list` returns
	// (`elitea_sdk/runtime/toolkits/tools.py:_mcp_tools`). A discovery with no
	// toolkit type is a preview for the toolkit editor — nothing would ever
	// match it — so it is answered but not stored.
	projectID, validProject := parsePositiveID(chi.URLParam(r, "projectID"))
	if serverName := strings.TrimSpace(body.ToolkitType); serverName != "" && validProject && h.pool != nil {
		registration := mcpregistry.Registration{
			ProjectID: projectID,
			Name:      serverName,
			ServerURL: endpoint.String(),
			Tools:     tools,
		}
		if err := mcpregistry.NewStore(h.pool).Save(ctx, registration); err != nil {
			// The discovery succeeded and the caller asked for the tools. A
			// failed write must not withhold them; it costs the caller the
			// listing, not this answer.
			slog.ErrorContext(ctx, "mcp_sync_tools: store registration failed",
				"project_id", projectID, "server", serverName, "err", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"tools":      tools,
		"count":      len(tools),
		"server_url": endpoint.String(),
	})
}

// mcpSyncDefaultTimeout and mcpSyncMaxTimeout bound the outbound discovery.
// pylon defaults the synchronous form to 120 seconds; the ceiling stops a
// caller-supplied value from holding a request open indefinitely.
const (
	mcpSyncDefaultTimeout = 120 * time.Second
	mcpSyncMaxTimeout     = 300 * time.Second
)

// parsePositiveID accepts only a plain positive integer, which is what pylon's
// `<int:project_id>` converter accepts.
func parsePositiveID(raw string) (int64, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func (h *Handler) SupportConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

var defaultAgentCategories = []string{
	"Business Analyst",
	"Quality Assurance",
	"Development",
	"DevOps",
	"Project Management",
	"Knowledge & Documentation",
	"Elitea",
	"Epam",
	"Other",
}

func (h *Handler) AgentCategories(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	// Merge defaults with admin-configured extras from guardrails config
	seen := make(map[string]bool)
	categories := make([]map[string]any, 0, len(defaultAgentCategories))
	for _, name := range defaultAgentCategories {
		seen[name] = true
		categories = append(categories, map[string]any{"name": name, "is_default": true})
	}

	// Extras from the admin Features page, which is where an operator can
	// actually author them.
	//
	// The pre-existing read below looks for a `publishing_guardrail` row in the
	// PROJECT's own `configuration` table. That row is per project, and the
	// setting is not: the reference authors it once, globally, in the pylon
	// plugin's config, and no surface in this platform writes a per-project
	// `publishing_guardrail` row at all. So the project read was a lookup that
	// could only ever miss, and every deployment got the nine hardcoded
	// defaults no matter what an administrator configured.
	//
	// Both are consulted, global first. The project row is kept because it is
	// the shape a hybrid deployment's data would already be in, and dropping it
	// would silently discard categories some environment may be carrying.
	for _, name := range h.extraAgentCategories(ctx) {
		if !seen[name] {
			seen[name] = true
			categories = append(categories, map[string]any{"name": name, "is_default": false})
		}
	}

	q := fmt.Sprintf(`SELECT data FROM %s.configuration WHERE section = 'publishing_guardrail' LIMIT 1`, s)
	var data []byte
	if err := h.pool.QueryRow(ctx, q).Scan(&data); err == nil && len(data) > 0 {
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) == nil {
			if extras, ok := cfg["agent_categories"].([]any); ok {
				for _, e := range extras {
					if name, ok := e.(string); ok && name != "" && !seen[name] {
						seen[name] = true
						categories = append(categories, map[string]any{"name": name, "is_default": false})
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"categories": categories, "total": len(categories)})
}

func (h *Handler) Permissions(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok || h.permissionResolver == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	resolution, err := h.permissionResolver.ResolvePermissions(
		r.Context(),
		user,
		auth.PermissionModeDefault,
		chi.URLParam(r, "projectID"),
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	result := make([]map[string]any, 0, len(resolution.Permissions))
	for _, permission := range resolution.Permissions {
		result = append(result, map[string]any{"name": permission, "enabled": true})
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	entityType := chi.URLParam(r, "entityType")
	entityID := chi.URLParam(r, "entityID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	user, _ := auth.UserFromContext(ctx)

	q := fmt.Sprintf(`
		INSERT INTO %s.social_pins (entity_name, entity_id, user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (entity_name, entity_id, user_id) DO NOTHING`, s)
	_, _ = h.pool.Exec(ctx, q, entityType, entityID, user.ID) // best-effort upsert
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	entityType := chi.URLParam(r, "entityType")
	entityID := chi.URLParam(r, "entityID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	user, _ := auth.UserFromContext(ctx)

	q := fmt.Sprintf(`DELETE FROM %s.social_pins WHERE entity_name = $1 AND entity_id = $2 AND user_id = $3`, s)
	_, _ = h.pool.Exec(ctx, q, entityType, entityID, user.ID) // best-effort delete
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// AuditTraces and AuditTraceHeatmap used to live here as stubs — empty arrays,
// request discarded, database untouched. Unit A14 implemented them for real,
// together with the two audit reads that had no route at all; see audit.go.

// ProjectUserActivity was a third stub of the same shape — `{"rows":[],"total":0}`
// with `_ *http.Request` and no route — and is now implemented for real in
// project_activity.go.

// `RegisterDescriptor` and `ServiceDescriptors` — the admin Service Descriptors
// page's three endpoints — moved to service_descriptors.go when unit A14 ported
// that page. Both were stubs: the listing answered 200 with three hardcoded rows
// naming Pylon plugins in a shape the client does not read, and the registration
// answered `{"ok": true}` to a body it discarded, from no route at all.

// --- Collections ---

func (h *Handler) CreateCollection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	user, _ := auth.UserFromContext(r.Context())
	userID := 1
	if user.ID != "" {
		userID, _ = strconv.Atoi(user.ID)
	}

	// `prompt_collections.owner_id` is the PROJECT and `author_id` is the USER
	// (#533). The legacy runtime set both in one statement:
	// `data["owner_id"], data["author_id"] = project_id, author_id`
	// (elitea_core/api/v2/collections.py:105). This statement bound both
	// columns to the SAME placeholder — `VALUES ($1, $2, $3, $3, ...)` — so
	// every collection claimed that a user was its owning project.
	ownerID, ownerErr := tenantOwnerID(projectID)
	if ownerErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	authorID, authorErr := ownership.NewUserID(int64(userID))
	if authorErr != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "an authenticated principal is required"})
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	name, _ := body["name"].(string)
	desc, _ := body["description"].(string)

	var id int
	err := h.pool.QueryRow(ctx, createCollectionInsertSQL(s),
		name, desc, ownerID.Int64(), authorID.Int64()).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          id,
		"name":        name,
		"description": desc,
	})
}

func (h *Handler) ListCollections(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	rows, err := h.pool.Query(ctx, fmt.Sprintf(
		`SELECT id, name, COALESCE(description, '') FROM %s.prompt_collections ORDER BY name`, s))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}, "total": 0})
		return
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var id int
		var name, desc string
		if rows.Scan(&id, &name, &desc) != nil {
			continue
		}
		items = append(items, map[string]any{"id": id, "name": name, "description": desc})
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": items, "total": len(items)})
}

func (h *Handler) GetCollection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	collectionID := chi.URLParam(r, "collectionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var id int
	var name, desc string
	var appsJSON, datasourcesJSON []byte
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id, name, COALESCE(description, ''),
			   COALESCE(applications::text, '[]')::bytea,
			   COALESCE(datasources::text, '[]')::bytea
		FROM %s.prompt_collections WHERE id = $1`, s), collectionID).Scan(
		&id, &name, &desc, &appsJSON, &datasourcesJSON)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "collection not found"})
		return
	}

	var appEntities, dsEntities []any
	_ = json.Unmarshal(appsJSON, &appEntities)       // DB jsonb column; malformed means empty list
	_ = json.Unmarshal(datasourcesJSON, &dsEntities) // DB jsonb column; malformed means empty list

	// Separate applications vs pipelines
	var apps, pipelines []map[string]any
	for _, e := range appEntities {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		entityID := em["id"]
		if entityID == nil {
			entityID = em["entity_id"]
		}
		var eidInt int
		switch v := entityID.(type) {
		case float64:
			eidInt = int(v)
		case string:
			eidInt, _ = strconv.Atoi(v)
		}
		if eidInt == 0 {
			continue
		}
		var isPipeline bool
		_ = h.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT EXISTS(SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND agent_type = 'pipeline')`, s), eidInt).Scan(&isPipeline) // failure leaves isPipeline=false
		item := map[string]any{"id": strconv.Itoa(eidInt)}
		if isPipeline {
			pipelines = append(pipelines, item)
		} else {
			apps = append(apps, item)
		}
	}
	if apps == nil {
		apps = []map[string]any{}
	}
	if pipelines == nil {
		pipelines = []map[string]any{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"name":         name,
		"description":  desc,
		"applications": map[string]any{"rows": apps, "total": len(apps)},
		"pipelines":    map[string]any{"rows": pipelines, "total": len(pipelines)},
	})
}

func (h *Handler) PatchCollection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	collectionID := chi.URLParam(r, "collectionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	operation, _ := body["operation"].(string)

	// Find the entity payload — keys like "application", "pipeline", "toolkit", etc.
	var entityRef map[string]any
	for k, v := range body {
		if k == "operation" {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			entityRef = m
			break
		}
	}

	if entityRef == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "entity reference required"})
		return
	}

	// Get entity ID
	var entityID int
	switch eid := entityRef["id"].(type) {
	case float64:
		entityID = int(eid)
	case string:
		entityID, _ = strconv.Atoi(eid)
	}

	// Read current applications JSON
	var appsJSON []byte
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(applications::text, '[]')::bytea FROM %s.prompt_collections WHERE id = $1`, s), collectionID).Scan(&appsJSON)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "collection not found"})
		return
	}

	var apps []map[string]any
	_ = json.Unmarshal(appsJSON, &apps) // DB jsonb column; malformed means empty list

	switch operation {
	case "add":
		// Add entity to list (avoiding duplicates)
		found := false
		for _, a := range apps {
			if aid, ok := a["id"].(float64); ok && int(aid) == entityID {
				found = true
				break
			}
		}
		if !found {
			apps = append(apps, map[string]any{"id": entityID, "owner_id": entityRef["owner_id"]})
		}
	case "remove":
		kept := apps[:0]
		for _, a := range apps {
			if aid, ok := a["id"].(float64); ok && int(aid) == entityID {
				continue
			}
			kept = append(kept, a)
		}
		apps = kept
	}

	updatedJSON, _ := json.Marshal(apps)
	_, _ = h.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.prompt_collections SET applications = $1::jsonb WHERE id = $2`, s), string(updatedJSON), collectionID) // best-effort update

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           collectionID,
		"applications": apps,
	})
}

func (h *Handler) DeleteCollection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	collectionID := chi.URLParam(r, "collectionID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	tag, err := h.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.prompt_collections WHERE id = $1`, s), collectionID)
	if err != nil || tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "collection not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // response writer; connection already committed
}
