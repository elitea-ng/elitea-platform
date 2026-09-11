// Package mcp is Main's streamable-HTTP MCP server surface — issue 252. It
// serves two deliberately separate products:
//
//   - external project capabilities that a client such as Claude Desktop,
//     Cursor, the MCP Inspector, or an SDK client can discover; and
//   - fixed internal builder categories that an Elitea chat can use to manage
//     project entities. Each internal operation has an explicit schema,
//     permission, project clamp, and in-process executor.
//
// What already existed here is the CLIENT side and its plumbing —
// `mcp_oauth_proxy`, `mcp_dcr_proxy`, `mcp_sync_tools` in
// `internal/api/v2/eliteacore/handler.go`, the `mcp_enabled` platform flag, the
// `?mcp=true` toolkit-type filter. None of those answer an MCP protocol
// message. A repo-wide grep for `tools/list`, `tools/call` or `/mcp` server
// handling found nothing before this package.
//
// # Legacy module → Go route
//
// Transport (pylon `routes/mcp_sse.py`, served under the app blueprint):
//
//	GET|POST /<pid>/mcp                    → GET|POST /app/{projectID}/mcp
//	GET|POST /<pid>/mcp/<openapi_tag>      → GET|POST /app/{projectID}/mcp/{category}
//	GET|POST /<pid>/mcp/<entity>/<ver_id>  → GET|POST /app/{projectID}/mcp/{entity}/{entityVersionID}
//	GET      /<pid>/sse + POST /<pid>/messages → NOT PORTED, see "The SSE pair" below
//
// REST (pylon `api/v2/`), under /api/v2/elitea_core:
//
//	tools_list              → GET  /tools_list/{projectID}
//	                          GET  /tools_list/default/{projectID}
//	tools_call              → POST /tools_call/default/{projectID}
//	internal_mcp_pat_status → GET  /internal_mcp_pat_status/prompt_lib/{projectID}/{toolkitType}
//
// # What serves real data and what refuses
//
// The answers this package can give are kept strictly apart, because the
// failure this repo keeps rediscovering (issue 128) is a route that answers 200
// while nothing behind it is wired:
//
//   - REAL. `initialize`, `tools/list` in all three scopes,
//     `internal_mcp_pat_status`, and the REST `tools_list`. The MCP protocol
//     listing is assembled from this project's own rows — agents whose version
//     carries the `mcp` tag, and toolkits flagged
//     `meta.mcp_options.available_by_mcp` — by catalog.go. The REST `tools_list`
//     reads the durable MCP server store (registry.go, issue 335). Nothing
//     about either is hardcoded or invented.
//   - REAL. `tools/list` and `tools/call` for the fixed applications, skills,
//     toolkits, configurations, notifications, project-context, and secrets
//     internal categories. Every
//     operation matches a current-platform `mcp_tool=True` opt-in and has an
//     explicit permission and schema. Main reuses its current handlers where
//     their contracts match and owns the transactional application safe-update
//     path where a full backup and an optimistic instruction patch must be
//     atomic.
//   - REAL, ON A DEPLOYMENT WITH THE RUNTIME. `tools/call` on an AGENT tool.
//     It admits an ordinary agent turn through the same use case the chat
//     start route drives, waits for it, bounded, and answers with the text the
//     projection settled on (execute.go). It is authorized per call on
//     `models.chat.messages.create`, the permission chat itself requires, so
//     this endpoint is not a way around it.
//   - REAL, ON A DEPLOYMENT WITH THE DIRECT-TOOL RUNTIME. `tools/call` on an
//     opted-in, selected, read-only TOOLKIT operation. Main re-reads and freezes
//     the exact current tenant row through the generated repository, rechecks
//     its opt-in, selection and guardrail policy, admits one
//     signed `toolkit.execute.read.v1` command, and returns only its fenced
//     terminal result. Rust independently requires the materialized operation
//     to declare itself read-only before invocation. A list/call race therefore
//     fails closed rather than running an operation that is no longer exposed.
//   - HONEST PROTOCOL ERROR when the required execution seam is not composed.
//     A toolkit-only deployment can still run toolkit tools; an agent-only
//     deployment can still run agents. A deployment with neither runtime seam
//     (`runtime.enabled` off) answers ToolExecutionUnavailableReason. Every
//     refusal is a CallToolResult with `isError: true` naming what is missing,
//     and NEVER an empty successful result — which is what an agent host would
//     read as "the tool ran and produced nothing". That rule also governs a run
//     that finishes with no text, fails, or pauses: see execute.go.
//   - HONEST 501. `tools_call`, the one remaining REST refusal. It dispatches a
//     tool invocation to the server that publishes the tool. This service does
//     not store that server's credentials, and it runs no socket.io server to
//     reach a client-hosted one. See registry.go for why a remote MCP toolkit
//     loses nothing by this.
//
// # Internal API categories are explicit, not inferred
//
// pylon's listing has a third source besides agents and toolkits:
// `openapi_registry.get_mcp_api_tools()`, which republishes pylon's own REST
// endpoints as MCP tools — 90 endpoints across seven plugins opt in with
// `mcp_tool=True`, and `McpApiToolExecutor` runs them by re-entering the Flask
// WSGI app with the caller's cookies and headers.
//
// That source is not reproduced generically here, for two reasons that are not
// "no time":
//
//  1. The opt-in list does not exist in this stack. Which REST operations an
//     external MCP client may drive is a security decision made one endpoint at
//     a time; `api/openapi/v2.yaml` carries no such marker, and inferring one
//     from "every operation in the document" would publish the admin surface to
//     any PAT holder. Several of the pylon opt-ins (the configurations and
//     projects plugins) have no Go REST counterpart to mark in the first place.
//  2. Execution would be a self-dispatch bridge — a request re-entered into
//     this service's own router with forwarded credentials — with its own
//     recursion, auth-forwarding and middleware-re-entry design. That is a
//     surface to specify, not to improvise inside a parity port.
//
// Instead, each internal category is added only after its operations have
// explicit schemas, permissions, project clamping, and an in-process executor.
// Applications, skills, toolkits, configurations, notifications, project
// context, and secrets currently meet that bar. Anything else is a 400 that
// names the valid set, which is how pylon answers an unknown tag too. See
// scope.go.
//
// # The SSE pair is dropped, not deferred
//
// pylon also serves the pre-2025 MCP transport: `GET /<pid>/sse` opens an event
// stream that emits an `endpoint` event, and the client then POSTs each message
// to `/<pid>/messages?session_id=…`. That pair is deprecated in the MCP
// specification in favour of streamable HTTP, which is what every current
// client speaks and what this package implements. Issue 252 P4 asks for a
// decision rather than code: it is DROPPED, superseded by streamable HTTP. It
// gets ported only if a named client turns up that cannot speak the current
// transport, and that client's name is what would justify the session store the
// pair needs (pylon keeps one `SseSession` per connection in process memory,
// which is also why the pair cannot survive more than one replica).
//
// # Statelessness
//
// The MCP specification lets a server operate without sessions, and this one
// does: no `Mcp-Session-Id` is issued, so no client is ever required to send
// one back, and no request depends on a predecessor. pylon's streamable-HTTP
// path is effectively the same — `create_http_session` builds a throwaway
// `HttpSession` per request — but it reaches that state by accident rather than
// by contract. Being stateless by contract is what lets this run behind more
// than one replica.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	draftsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	eliteacoreapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	toolkitsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// errNoPool is returned instead of dereferencing a nil pool. A handler
// constructed without a database is a composition fault, and the callers turn
// it into a 503 rather than a panic that takes the process down.
var errNoPool = errors.New("mcp: no database pool configured")

// Handler serves both the REST endpoints and the MCP protocol endpoints.
//
// One handler rather than two because both halves answer to the same master
// switch and the same tenant scoping, and a second type would only make it
// possible for the two to disagree about them.
type Handler struct {
	pool *pgxpool.Pool
	// source is the tool catalog. Swappable so the protocol handling can be
	// tested without a database; production always gets postgresToolSource.
	source toolSource
	// registry is the durable store of MCP servers registered into a project.
	// It is what `tools_list` reads (registry.go, issue 335).
	registry *mcpregistry.Store
	// personal resolves the caller's personal project, whose registered servers
	// pylon unions into every listing.
	personal PersonalProjectResolver
	// start admits one agent turn. NIL on a deployment whose runtime plane is
	// off (`runtime.enabled`), which is why every use of it is guarded — see
	// callTool, which then answers the untouched ToolExecutionUnavailableReason.
	start AgentStartUseCase
	// toolkitExecute admits one exact selected, MCP-exposed read operation to
	// the durable Rust worker. It is optional for the same deployment reason as
	// start: runtime-disabled Main must keep listing tools while refusing calls.
	toolkitExecute ToolkitExecuteReadUseCase
	// toolkitArgumentSchemas supplies the digest-pinned SDK argument schemas
	// used by external Elitea-as-MCP toolkit tools. Dynamic toolkit families
	// use the shared saved-instance discovery service when it is available.
	// Runtime-disabled deployments retain the explicit open-object fallback.
	toolkitArgumentSchemas ToolkitArgumentSchemaSource
	toolkitDiscovery       discovery.UseCase
	// permissions authorizes an EXECUTION, and only an execution.
	//
	// The endpoint as a whole is gated at the MEMBERSHIP tier
	// (apimw.RequireProjectAccess), which is the right tier for reading a tool
	// listing and is what pylon applies to the whole surface. RUNNING an agent
	// is a different act — it spends the project's model budget and can drive
	// the agent's toolkits — and the chat surface requires
	// `models.chat.messages.create` for exactly that. Executing through MCP at
	// a weaker tier than through chat would make this endpoint a way around
	// that permission, so the check lives HERE, per call, rather than on the
	// route: a route-level gate would also refuse `tools/list` to a viewer,
	// which is a read they are entitled to.
	permissions auth.PermissionResolver
	// toolRuns runs one TOOLKIT tool (#616). NIL on a deployment whose runtime
	// plane is off, exactly like `start`, and guarded the same way — callTool
	// then answers the untouched ToolkitExecutionUnavailableReason.
	toolRuns ToolkitRunUseCase
	// internalApplications executes the fixed internal application-builder
	// category in process. It is nil only in protocol unit tests or when Main
	// has no database composition.
	internalApplications internalApplicationExecutor
	// internalSkills executes the fixed internal skill-builder category.
	internalSkills internalSkillExecutor
	internalDrafts *draftsapi.Handler
	internalChat   internalChatExecutor
	// internalToolkits executes the fixed internal toolkit-builder category.
	internalToolkits internalToolkitExecutor
	// internalConfigurations executes the fixed internal configurations
	// category through the same policy-complete handler as REST.
	internalConfigurations      internalConfigurationExecutor
	internalTypedConfigurations *configurationsapi.CurrentConfigurationToolHandler
	// internalNotifications executes the actor-scoped fixed notifications
	// category through Main's current notification store.
	internalNotifications internalNotificationExecutor
	// internalProjectContext executes the fixed project-context builder through
	// the same handler instance as the current REST surface.
	internalProjectContext internalProjectContextExecutor
	// internalSecrets executes the fixed project-secret builder through the
	// same encrypted-vault handler instance as the current REST surface.
	internalSecrets internalSecretExecutor
}

// Option configures an MCP handler without weakening the required constructor
// arguments. Internal builder executors are optional because protocol-only unit
// tests and runtime-disabled deployments may deliberately omit them.
type Option func(*Handler)

// ToolkitExecuteReadUseCase is the narrow durable direct-tool seam used by
// external Elitea-as-MCP tools. Main owns admission and result settlement; the
// MCP handler owns only authorization and protocol projection.
type ToolkitExecuteReadUseCase interface {
	Execute(
		context.Context,
		toolkitexecutionapp.ExecuteCurrentReadToolRequest,
	) (toolkitexecutionapp.CurrentReadToolExecutionOutcome, error)
}

func WithToolkitExecuteRead(executor ToolkitExecuteReadUseCase) Option {
	return func(handler *Handler) {
		handler.toolkitExecute = executor
	}
}

// ToolkitArgumentSchemaSource supplies one built-in toolkit type's per-tool
// argument JSON Schemas, keyed by the SDK operation name. found=false means
// the type is not in the pinned built-in catalogue; an empty map is a valid
// answer for a dynamic family such as MCP or OpenAPI.
//
// The narrow interface lives here because the implementation is owned by
// internal/runtimecomposition, which imports the API layer to compose routes.
type ToolkitArgumentSchemaSource interface {
	ToolkitArgumentSchemas(toolkitType string) (map[string]map[string]any, bool, error)
}

// WithToolkitArgumentSchemas gives external Elitea-as-MCP the same immutable
// SDK schema snapshot used by the toolkit editor. The source is actor-neutral;
// project and selected-tool admission remain row-backed in catalog.go.
func WithToolkitArgumentSchemas(source ToolkitArgumentSchemaSource) Option {
	return func(handler *Handler) {
		handler.toolkitArgumentSchemas = source
	}
}

// WithInternalToolkitHandler reuses the fully composed Main toolkit handler for
// internal MCP calls. The caller must pass the same instance used by the REST
// routes so guardrails, dynamic schemas, secret sealing and credential
// validation cannot diverge between the two surfaces.
func WithInternalToolkitHandler(handler *toolkitsapi.Handler) Option {
	return func(mcpHandler *Handler) {
		mcpHandler.internalToolkits = newHandlerInternalToolkitExecutor(handler)
	}
}

// WithInternalConfigurationHandler reuses the fully composed Main
// configuration handler for internal MCP calls. Sharing it with REST keeps
// registry lookup, provider admission, vault sealing and shared-project
// behavior on one implementation path.
func WithInternalConfigurationHandler(handler *configurationsapi.Handler, typed *configurationsapi.CurrentConfigurationToolHandler) Option {
	return func(mcpHandler *Handler) {
		mcpHandler.internalConfigurations = newHandlerInternalConfigurationExecutor(handler, typed)
		mcpHandler.internalTypedConfigurations = typed
	}
}

// WithInternalNotificationStore composes the fixed notification tools over
// the same user-scoped store used by the current REST route.
func WithInternalNotificationStore(store notificationapp.Store) Option {
	return func(mcpHandler *Handler) {
		toolHandler := notificationsapi.NewCurrentNotificationToolHandler(store)
		mcpHandler.internalNotifications = newHandlerInternalNotificationExecutor(toolHandler)
	}
}

// WithInternalProjectContextHandler reuses Main's project-context handler so
// REST and internal MCP share validation, storage, and response semantics.
func WithInternalProjectContextHandler(handler *eliteacoreapi.Handler) Option {
	return func(mcpHandler *Handler) {
		mcpHandler.internalProjectContext = newHandlerInternalProjectContextExecutor(handler)
	}
}

// WithInternalSecretHandler reuses Main's encrypted project-vault handler.
// The fixed MCP catalogue excludes plaintext reads, deletes, hidden-secret
// operations, and the global administration vault.
func WithInternalSecretHandler(handler *secretsapi.Handler) Option {
	return func(mcpHandler *Handler) {
		mcpHandler.internalSecrets = newHandlerInternalSecretExecutor(handler)
	}
}

// AgentStartUseCase is the narrow slice of
// `internal/api/v2/agentexecution.StartUseCase` this package needs: admitting
// the first turn of a configured application.
//
// It is declared here rather than imported so the dependency points one way,
// the same reason PersonalProjectResolver is. The production implementation is
// `runtimecomposition.PublicRoutes.AgentStart`, the SAME use case the chat
// start route and the support assistant drive — not a second pipeline. Giving
// an MCP run its own executor is how the two would drift apart on tracing,
// budgets and cancellation.
type AgentStartUseCase interface {
	StartCurrentApplication(
		context.Context,
		agentexecutionapp.CurrentApplicationStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

// PersonalProjectResolver reports a user's personal project id.
//
// Declared here as the narrow interface this package needs, rather than
// importing the middleware package, so the dependency points one way. The
// production implementation is `middleware.DBPersonalProjectResolver`, which
// mirrors pylon's `projects_get_personal_project_id`.
type PersonalProjectResolver interface {
	PersonalProjectID(ctx context.Context, userID string) (int, error)
}

// NewHandler builds the MCP handler.
//
// personal and start are required ARGUMENTS rather than optional setters. A
// listing built without personal silently drops the caller's own registered
// servers, and a handler built without start silently refuses every execution;
// this repository has shipped exactly that failure before, as a builder method
// that no composition root called (issue 128). An argument cannot be forgotten.
//
// start is required AND NILABLE, which is not a contradiction: it comes from
// `publicRoutes.AgentStart`, which is nil whenever `runtime.enabled` is off, so
// nil is a real deployment state rather than a mistake. Passing it explicitly
// is what makes the composition root state which one it is. When it is nil,
// callTool answers the same ToolExecutionUnavailableReason it always has.
//
// permissions is required for the opposite reason: it FAILS CLOSED. A handler
// built without it refuses every execution, so forgetting it cannot widen what
// a caller may run.
func NewHandler(
	pool *pgxpool.Pool,
	personal PersonalProjectResolver,
	start AgentStartUseCase,
	permissions auth.PermissionResolver,
	opts ...Option,
) *Handler {
	handler := &Handler{pool: pool, personal: personal, start: start, permissions: permissions}
	handler.source = postgresToolSource{handler: handler}
	if pool != nil {
		handler.registry = mcpregistry.NewStore(pool)
		handler.internalApplications = newPostgresInternalApplicationExecutor(pool)
		handler.internalSkills = newPostgresInternalSkillExecutor(pool)
	}
	for _, opt := range opts {
		opt(handler)
	}
	return handler
}

// NewHandlerWithToolkitRuns adds the toolkit half of `tools/call` (#616).
//
// It is a second constructor rather than a sixth positional parameter because
// the agent half and the toolkit half are composed from different halves of the
// runtime and either may be absent: a deployment can serve agents and refuse
// toolkits, and one that composes neither keeps both original sentences.
//
// `runs` is taken as an INTERFACE value and assigned only when non-nil, so a
// caller that hands in a nil concrete pointer cannot box it into a non-nil
// interface — the typed-nil trap this service has been bitten by before.
func NewHandlerWithToolkitRuns(
	pool *pgxpool.Pool,
	personal PersonalProjectResolver,
	start AgentStartUseCase,
	runs ToolkitRunUseCase,
	permissions auth.PermissionResolver,
	opts ...Option,
) *Handler {
	handler := NewHandler(pool, personal, start, permissions, opts...)
	if runs != nil {
		handler.toolRuns = runs
	}
	return handler
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// mcpEnabled resolves the deployment-wide MCP master switch.
//
// This is the same read `internal/api/v2/eliteacore/platform_flags.go` performs
// for the OAuth/DCR/sync routes, deliberately duplicated as a call to the same
// `platformconfig` section and key rather than re-exported: the flag's own
// description promises it "removes all MCP-related functionality across the
// entire application including API endpoints", and this package publishes the
// largest MCP surface in the service, so it must obey the switch or the promise
// is false.
//
// The failure mode is permissive for the same reason it is there: an
// unreadable configuration store is an operational fault, and resolving it to
// "MCP is disabled" would turn a database hiccup into a platform-wide outage of
// a subsystem nobody switched off.
func (h *Handler) mcpEnabled(ctx context.Context) bool {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionMCPConfiguration)
	if err != nil {
		return true
	}
	return values.Bool(platformconfig.KeyMCPEnabled, true)
}

// mcpDisabledMessage is pylon's wording, byte for byte
// (`legacy/plugins/elitea_core/routes/mcp_sse.py:_check_mcp_enabled`). Existing
// clients of these paths were written against that sentence.
const mcpDisabledMessage = "MCP exposure is disabled on this deployment"

// requireMCPEnabled gates the REST half. It returns true when the request may
// proceed.
func (h *Handler) requireMCPEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.mcpEnabled(r.Context()) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"error": mcpDisabledMessage})
	return false
}

// projectSchema turns a URL segment into the tenant schema, QUOTED as a
// PostgreSQL identifier and ready to interpolate with %s.
//
// Every query in this package interpolates the schema into its statement text,
// because a schema name cannot be bound as a parameter. A segment that is not
// a plain decimal project id is refused here, and the name that does pass is
// quoted with SQL rules rather than with %q, which quotes with Go rules and
// lets an embedded quote close the identifier (issue #543).
func projectSchema(raw string) (string, bool) {
	quoted, err := tenantschema.Quote(raw)
	if err != nil {
		return "", false
	}
	return quoted, true
}
