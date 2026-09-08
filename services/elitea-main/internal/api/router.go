package api

import (
	"context"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/emailsettings"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/adminui"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/gateway"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/health"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	scimapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/scim"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2apps "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	v2applicationskills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	v2artifacts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	v2budgets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/budgets"
	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	v2configs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2contextmgr "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/contextmgr"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	v2drafts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	v2core "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	v2events "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/events"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2indextypes "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	v2inventory "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	v2mcp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	v2messagetraces "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/messagetraces"
	v2moderation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/moderation"
	v2openapidocs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/openapidocs"
	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	v2predict "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
	v2projectinfo "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	v2promptcontextreads "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	v2scheduling "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/scheduling"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
	v2skillpublish "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skillpublish"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	v2support "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/supportassistant"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	v2toolkits "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	v2tracing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tracing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/artifactbootstrap"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	platformauth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// AuthDeps preserves the current-main grouped dependency contract while the
// parity composition continues to accept the established flat fields.
type AuthDeps struct {
	Validator                 apimw.TokenValidator
	PrincipalValidator        apimw.PrincipalValidator
	ForwardedIdentityVerifier apimw.ForwardedIdentityPeerVerifier
	SessionHandler            *v2auth.SessionHandler
	OIDCHandler               *v2auth.OIDCHandler
	SAMLHandler               *v2auth.SAMLHandler
	SessionSecret             string
	// SessionStore validates the server-side browser session an
	// `elitea_session` cookie names (migrations/shared/0117). It reaches every
	// apimw.AuthConfig this file builds, and it must: a group that dropped it
	// would read a server-side identifier with the legacy HMAC reader and
	// refuse every browser holding one.
	SessionStore apimw.BrowserSessionValidator
	// RejectLegacySessionCookies ends the pre-0117 signed-cookie window.
	RejectLegacySessionCookies bool
}

// NOTE(#126): IndexerDeps and its six fields — Predictor, LLMService,
// ChatService, PipelineRunner, ToolTester, MCPSyncer — used to live here. They
// were projections onto internal/infra/indexersvc, a prototype Redis RPC client
// that published raw JSON to the `elitea_rpc` channel that pylon-indexer serves
// through arbiter's gzip+pickle codec, so every call was silently dropped.
// Nothing ever assigned them outside tests, so the twelve routes they gated
// 404'd in every deployment.
//
// They are gone rather than fixed because the replacement transport already
// ships: runtimecomposition + the Redis command stream + services/
// elitea-worker-python, deployed in deploy/centry-hybrid/pov-compose.yml, and
// elitea-docs' spec-transport-implementation.mdx lists indexersvc/rpc.go under
// "Delete after bounded dispatch/control/output adapters land". They landed.
//
// The capabilities those routes were the last visible trace of are recorded so
// they are not lost with the code: #192 (inbound webhook pipeline trigger),
// #193 (scheduled pipeline execution), #93 (chat dispatch/streaming migration),
// #194 (AI draft generation, tool testing and MCP tool sync have no backend).

type RouterConfig struct {
	Auth               AuthDeps
	AuthValidator      apimw.TokenValidator
	PrincipalValidator apimw.PrincipalValidator
	SessionHandler     *v2auth.SessionHandler
	OIDCHandler        *v2auth.OIDCHandler
	SAMLHandler        *v2auth.SAMLHandler
	HealthDeps         health.Deps
	Pool               *pgxpool.Pool
	// BrandPackPath is the brand pack's file layer (BRAND_PACK_PATH, read in
	// cmd/elitea-main/brand_pack_config.go). Empty means no file layer. Used
	// only when Branding is nil.
	BrandPackPath string
	// Branding is the resolver main.go builds once and shares with the login
	// page (ADR-0024 WP5), so the bootstrap route, the admin surface and the
	// login page can never disagree about the brand. Nil builds one here
	// from BrandPackPath and Pool — the shape every test uses.
	Branding *v2branding.Resolver
	// Mailer is outbound e-mail (ADR-0024 WP7): invitations, moderation
	// notices, the Branding page's test message. Nil means none is sent and
	// every invite reports invitation_delivered: false.
	Mailer *appmailer.Composer
	// EmailSettings resolves the mail transport per send: the admin E-mail
	// page's rows laid over the environment defaults (gap G7). main.go builds
	// one and shares it with the Mailer above, so the page that says a relay
	// is configured and the composer that dials it cannot disagree. Nil builds
	// one here over Pool with an EMPTY environment layer — the shape every
	// test uses, where there is no process environment to inherit.
	EmailSettings *emailsettings.Resolver
	// BrandingPackages exports and imports the branding package (ADR-0024
	// decision 9). Nil means the package routes answer 503; the router does
	// not build one because the previews it renders belong to main.go's
	// composition (the login template and the mail composer).
	BrandingPackages admin.BrandingPackages
	// AuditRecorder overrides the Pool-backed `centry.audit_events` writer
	// mounted on the /api/v2 group. Tests inject one to assert WHICH events a
	// request produces without a live database; production leaves it unset and
	// gets audit.NewPostgresRecorder(Pool). A nil Pool and a nil field together
	// mean the audit middleware is not mounted at all.
	AuditRecorder audit.Recorder
	// ArtifactPermissionResolver overrides the legacyrbac.NewPostgresResolver
	// built from Pool for the artifact routes only (S11) — tests inject a
	// resolver here to control RBAC outcomes without a live database. Every
	// other route keeps using the Pool-backed resolver regardless of this
	// field.
	ArtifactPermissionResolver platformauth.PermissionResolver
	// ProjectAccessQuerier overrides the Pool-backed membership query that
	// apimw.RequireProjectAccess runs for the /elitea_core project-scoped
	// routes (#302) — tests inject one here to control the membership answer
	// without a live database. With a nil Pool the middleware can only answer
	// 503, which proves nothing about cross-tenant refusal. Production leaves
	// this unset and keeps the Pool-backed query.
	ProjectAccessQuerier apimw.ProjectAccessQuerier
	// ProjectPermissionResolver overrides the legacyrbac.NewPostgresResolver
	// built from Pool for the /elitea_core per-route permission gates (#302,
	// #313) — tests inject one here to control which permissions a caller
	// resolves without a live database. With a nil Pool the resolver can only
	// answer denied, so every route would 403 and a test could not tell a
	// working gate from a broken one. Production leaves this unset and keeps
	// the Pool-backed resolver.
	ProjectPermissionResolver platformauth.PermissionResolver
	// ArtifactHandler overrides newArtifactHandler's Pool/ObjectStore-backed
	// construction (S11) — newArtifactHandler always builds real
	// Postgres-backed repositories from Pool with no injection seam, so a
	// router-level test proving a genuine 2xx through the full auth/RBAC/
	// handler chain (as opposed to just "reached past the stub") has no
	// other way to supply a working fake Repository/ObjectStore pair.
	ArtifactHandler *v2artifacts.Handler
	AppsRepo        applications.Repository
	// ToolkitArgumentSchemas supplies GET /elitea_core/toolkits/prompt_lib/
	// {projectID} with the per-tool argument schemas from the digest-pinned SDK
	// snapshot. It is injected rather than constructed here because the snapshot
	// is owned by internal/runtimecomposition, which imports this layer.
	// Unassigned, the endpoint serves settings schemas with no argument schemas
	// and every tool form in the web client renders empty.
	ToolkitArgumentSchemas v2toolkits.ToolkitArgumentSchemaSource
	// ToolkitCatalogue supplies the same endpoint with the SETTINGS schema and
	// the METADATA of every built-in SDK toolkit type. Unassigned, the endpoint
	// serves only the eight hand-written types, and the create page's chooser
	// offers seven tiles against the reference deployment's sixty.
	ToolkitCatalogue v2toolkits.ToolkitCatalogueSource
	// ToolkitWorkerCapability decides which of those types this deployment can
	// run. Unassigned, every catalogued type is offered — the honest answer
	// when nothing has said which worker image is deployed.
	ToolkitWorkerCapability v2toolkits.ToolkitCapabilitySource
	// ToolkitSettingsDefinitions supplies the same endpoint with the "$defs"
	// block each type's settings properties reference. It is injected for the
	// same reason as ToolkitArgumentSchemas: the implementation joins two
	// digest-pinned snapshots owned by internal/runtimecomposition.
	// Unassigned, the endpoint serves no "$defs", and the web client's toolkit
	// credential picker and index schedule credential select stay unreachable.
	ToolkitSettingsDefinitions v2toolkits.ToolkitSettingsDefinitionSource
	// ToolkitSettingsValidator resolves a toolkit's credential references before
	// POST /elitea_core/tools/prompt_lib/{projectID} and PUT/PATCH
	// /elitea_core/tool/prompt_lib/{projectID}/{toolkitID} persist them. It is
	// injected for the same reason as the two sources above: the resolver is
	// composed in internal/runtimecomposition, which imports this layer.
	//
	// LEFT NIL, BOTH ROUTES BEHAVE EXACTLY AS THEY DID BEFORE — a toolkit can be
	// saved naming a credential that lives in another project or nowhere at all,
	// and the reference is first read at chat time, where it kills the turn. The
	// composition root supplies it only where the Configurations runtime exists
	// (ELITEA_CONFIGURATIONS_ENABLED), so a default Helm install still takes the
	// nil path; see cmd/elitea-main/main.go.
	ToolkitSettingsValidator v2toolkits.ToolkitSettingsValidator
	// ToolkitRegistry enumerates built-in toolkit types and their tools for
	// `GET /admin/plugin_config_suggestions/administration/{key}`, which is what
	// populates the guardrail fields' pickers on the admin Configuration page.
	// It is the same digest-pinned snapshot as ToolkitArgumentSchemas, declared
	// separately because it answers a different question and because a handler
	// should not be handed a schema source in order to ask for a name list.
	// Unassigned, the two toolkit suggestion sources answer 501 with a reason
	// rather than an empty list.
	ToolkitRegistry admin.ToolkitRegistrySource
	SkillsRepo      v2skills.Repository
	FoldersRepo     v2folders.Repository
	TagsRepo        v2tags.Repository
	AnalyticsRepo   v2analytics.Repository
	ConvsRepo       v2convs.Repository
	// SharedChatStore and SharedChatTranscript back "share a conversation by
	// link" (internal/api/v2/sharedchat). Two fields for one feature because
	// they have different tenancies — one central link table, one per-project
	// transcript read — and because the anonymous routes must be registrable
	// with the transcript reader absent, so a deployment that has not wired it
	// refuses rather than serving a half-built view.
	SharedChatStore      sharedchat.Store
	SharedChatTranscript sharedchat.TranscriptStore
	WebhookRepo          webhook.Repository
	RedisClient          *goredis.Client
	EventSource          v2events.EventSource
	// EvalDimensionsRepo backs the Agent Evaluation DIMENSION LIBRARY — the
	// first and, for now, only slice of that feature. Unassigned, the four
	// routes are not registered at all, which answers 404: a stubbed 200 with
	// an empty list would be indistinguishable, in the browser, from a working
	// library that happens to be empty.
	EvalDimensionsRepo v2evaluation.Repository
	// EvalDatasetsRepo and EvalRunsRepo back Agent Evaluation SLICE 2 — the
	// dataset, the run and the read-only scorecard. Unassigned, the thirteen
	// routes below are not registered at all, for the reason the dimension
	// four give: a stubbed 200 with an empty list is indistinguishable, in the
	// browser, from a working feature with no data.
	EvalDatasetsRepo v2evaluation.DatasetRepository
	EvalRunsRepo     v2evaluation.RunRepository
	// EvalOrchestrator executes runs. It may be nil while the repositories are
	// not: a deployment with no LLM plane still stores and lists runs, and a
	// `created` row that nothing executes is visibly stuck rather than
	// invisibly absent. The run START route reports the row it wrote either
	// way, so a queued run is never claimed to be running.
	EvalOrchestrator v2evaluation.Enqueuer
	// ProjectVectorStore provisions a new project's PgVector credentials and
	// its `vectorstorage` configuration row (#371). It is injected because the
	// composition needs the Configurations runtime's finder, unsecreter and
	// vault writer, and that runtime is owned by internal/runtimecomposition,
	// which imports this layer. Unassigned, a created project has no vector
	// store and cannot index — see createProjectVectorStore.
	ProjectVectorStore projectprovisioning.ProjectVectorStore
	AdminUI            *adminui.Config
	// ObjectStore is the new S3/Azure/GCS-compatible backend (see
	// docs/plans/storage-migration-plan.md). S8 reads it for the bucket-plane
	// DELETE cascade, but only inside newProductionRouter — it is
	// not on any production request path until S11 mounts the new artifact
	// routes there.
	ObjectStore      storage.ObjectStore
	BudgetAlertStore *gateway.BudgetAlertStore
	// GatewayStatus reads elitea-llm-gateway's `GET /governance/status` for the
	// admin LLM Proxy section — the only surface that can tell an operator that
	// a saved governance row was REJECTED, is INERT, or that the enforced
	// snapshot is STALE.
	//
	// Typed as the interface so tests substitute a fake. Leave it nil on a
	// deployment with no gateway address: the handler reports "not configured"
	// as a state, and assigning a nil *gateway.GatewayStatusClient into it
	// instead would produce a non-nil interface holding a nil pointer, which
	// defeats that check and calls a method on a nil receiver (#86).
	GatewayStatus gateway.StatusReader
	SessionSecret string
	// PATSigner signs the personal access tokens the /api/v2/auth/token route
	// returns. Set it whenever the deployment validates a token with a key
	// that is NOT SessionSecret. A form authentication graph does exactly
	// that: it reads a token back with credentials.pat_signing_key_file.
	//
	// Leave it nil for the OIDC-only shape, where APPLICATION_SECRET_KEY both
	// signs the token and reads it back. Never box a nil pointer into it.
	PATSigner v2auth.TokenSigner
	RuntimeRoutes                 RuntimeRoutes
	ProductionAuth                *ProductionAuthRoutes
	ProductionRuntime             *ProductionRuntimeRoutes
	CurrentProjectInfo            *v2projectinfo.CurrentProjectInfoRoute
	CurrentIndexTypes             *v2indextypes.CurrentIndexTypesRoute
	CurrentApplicationSkills      *v2applicationskills.CurrentApplicationSkillsRoute
	CurrentPromptContextReads     *v2promptcontextreads.CurrentRoutes
	CurrentProjectList            *v2projects.CurrentProjectListRoute
	CurrentSocialAuthors          *v2social.CurrentAuthorsRoute
	CurrentSocialAvatar           *v2social.CurrentAvatarRoute
	CurrentConfigurationAvailable http.Handler
	CurrentConfigurationRead      http.Handler
	CurrentConfigurationTypes     http.Handler
	CurrentConfigurationMutation  http.Handler
	CurrentIndexStart             http.Handler
	// DeepWiki is the facade in front of the DeepWiki provider service
	// (ADR-0022 P2). Nil whenever ELITEA_DEEPWIKI_ENABLED is off, which is
	// every deployment that does not run the provider — the paths are then not
	// registered at all rather than registered and refusing.
	DeepWiki *v2deepwiki.Route

	// Inventory is the facade in front of the Inventory provider service —
	// the second provider, mounted through the same shared packages.
	Inventory         *v2inventory.Route
	CurrentAgentStart http.Handler
	// SupportAssistantStart is the agent-execution use case the support
	// assistant's predict route delegates to — the SAME use case
	// CurrentAgentStart's route drives, not a second one. Left nil, the predict
	// route answers 503 and the rest of the support surface still works
	// (history stays readable), which is the honest degrade for a deployment
	// that runs no agent execution.
	SupportAssistantStart v2support.StartUseCase
	// MCPAgentStart is the agent-execution use case the MCP server's
	// `tools/call` drives — again the SAME use case CurrentAgentStart's route
	// and the support assistant drive, for the same reason. Left nil (which is
	// what `runtime.enabled` off produces), `tools/call` answers the unchanged
	// v2mcp.ToolExecutionUnavailableReason and `tools/list` is unaffected.
	MCPAgentStart v2mcp.AgentStartUseCase
	// MCPToolkitRun is the toolkit half of `tools/call` (#616). Nil keeps the
	// refusal ToolkitExecutionUnavailableReason, which is the honest answer on
	// a deployment with no worker.
	MCPToolkitRun v2mcp.ToolkitRunUseCase
	// ToolkitToolRun runs one toolkit tool synchronously (#340). Nil keeps the
	// `503 indexer service not available` both test routes have always given.
	ToolkitToolRun toolkitrun.UseCase
	// PipelineTriggers serves the two UNATTENDED ways to start a pipeline —
	// the inbound signed trigger (issue 192) and the cron schedule (issue 193).
	//
	// It is built at the composition root rather than here, because the SAME
	// handler is also registered as the platform scheduler's
	// `pipeline.schedule.scan.v1` job. Two handlers over one table would be two
	// chances for the HTTP half and the tick to disagree on a dependency — a
	// tick wired without the permission resolver would refuse every scheduled
	// run while the settings tab kept working, and nothing would report it.
	//
	// Left nil, none of the seven routes is registered at all. That is
	// deliberate and is not a silent 200: an unregistered route answers the
	// group's own 404, while a registered route with no runtime behind it is
	// the "answers 200, nothing is wired" defect this repository keeps
	// rediscovering.
	PipelineTriggers   *v2pipelinetriggers.Handler
	CurrentAgentCancel http.Handler
	// CurrentApplicationTask serves the legacy application_task path (issue
	// 254 P2): GET polls the run bound to a response message, DELETE stops
	// it through the SAME use case CurrentAgentCancel runs.
	CurrentApplicationTask     http.Handler
	CurrentIndexCancel         http.Handler
	CurrentIndexMeta           http.Handler
	CurrentIndexMetaDelete     http.Handler
	CurrentIndexScheduleUpdate http.Handler
	CurrentIndexScheduleDelete http.Handler
	CurrentNotifications       http.Handler
	CurrentNotificationEvents  http.Handler
	CurrentModelCatalog        http.Handler
	CurrentModelDefault        http.Handler
	LLMProxy                   http.Handler
	LLMProjectResolver         apimw.PersonalProjectResolver
	// GatewayProxy is the mTLS streaming reverse proxy to elitea-llm-gateway-svc
	// (BF0.9c). When non-nil, it is mounted at /llm with Auth+Project middleware
	// in the production router.
	GatewayProxy           http.Handler
	GatewayProjectResolver apimw.PersonalProjectResolver
	// ConfigConnectionChecker performs the real, minimal provider round trip
	// backing /configurations/check_connection(s) (#319). nil leaves those
	// routes reporting an honest "not available" failure rather than the
	// previous unconditional-success stub.
	ConfigConnectionChecker v2configs.ConnectionChecker
	// ConfigProviderAdmission decides configuration.status_ok for a row the
	// compatibility write routes store (#457). nil leaves every written row at
	// the column default, false, which the LLM gateway refuses.
	ConfigProviderAdmission v2configs.ProviderAdmission
	// ConfigStoredResolver resolves a STORED row's references and hidden
	// secrets, so /check_stored_connection(s) can test a saved credential
	// without the client resending its api_key. nil leaves those two routes
	// reporting an honest "not available" failure; it never checks the stored
	// {{secret.NAME}} reference as though it were the key.
	ConfigStoredResolver v2configs.StoredConfigurationResolver
	// PredictCompleter performs the blocking LLM turn behind
	// POST /elitea_core/predict_llm/prompt_lib/{projectID} (#194).
	//
	// It does NOT gate the route: the route is registered unconditionally and
	// a nil completer answers 503 naming LLM_GATEWAY_URL. That is the whole
	// lesson of #126 — a capability gated on a dependency the composition root
	// never assigns is a 404 nobody can tell from a typo.
	PredictCompleter v2predict.Completer
}

type RuntimeRoutes struct {
	Validation      http.Handler
	ExecutionEvents http.Handler
}

const (
	runtimeValidationPath = "/configurations/validation/{projectID}/{configurationRevisionID}"
	runtimeEventsPath     = "/executions/{projectID}/{executionID}/events"
)

// notImplementedArtifact is the S7 placeholder for every new artifact route
// this stage registers but does not yet implement — S8, S9, and S15 each
// replace it at their own paths. It returns the same typed error envelope
// (components/schemas/Error in api/openapi/v2.yaml) real artifact handlers
// use for every other error response, rather than a bare 501.
func notImplementedArtifact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"error":{"code":"NotImplemented","message":"pending S8/S9"}}`))
}

// adminEvalRunCanceller adapts the evaluation repository to
// admin.EvalRunCanceller: the same CancelRun, with the updated row dropped.
//
// The adapter exists so the admin package holds no dependency on the
// evaluation model for a value it never reads — and so the admin Tasks page's
// cancel and the evaluation page's own cancel remain ONE implementation.
type adminEvalRunCanceller struct {
	runs *dbrepos.EvalRunsRepo
}

func (c adminEvalRunCanceller) CancelRun(ctx context.Context, projectID, runID string) error {
	_, err := c.runs.CancelRun(ctx, projectID, runID)
	return err
}

// artifactRepoAdapter satisfies v2artifacts.Repository by embedding both S6
// repositories — they share no method names, so Go's method promotion does
// the rest. Neither constructor accepts a nil pool without erroring, unlike
// most other prototype-router dependencies, so newArtifactHandler below
// builds this only when cfg.Pool and cfg.ObjectStore are both set.
type artifactRepoAdapter struct {
	*dbrepos.ArtifactBucketsRepository
	*dbrepos.ArtifactObjectsRepository
	*dbrepos.ArtifactTransferGrantsRepository
	*dbrepos.ArtifactBucketPermissionsRepository
}

// newArtifactHandler builds the S6/S1-backed artifacts.Handler when
// cfg.Pool and cfg.ObjectStore are both available. ok is false when either
// is unset or repository construction fails, in which case every artifact
// route stays on notImplementedArtifact — the same placeholder S7
// registered for all of them, so a router built without database/storage
// config (as most tests do) keeps behaving exactly as it did before S8/S9.
func newArtifactHandler(cfg RouterConfig) (h *v2artifacts.Handler, ok bool) {
	if cfg.Pool == nil || cfg.ObjectStore == nil {
		return nil, false
	}
	bucketsRepo, err := dbrepos.NewArtifactBucketsRepository(cfg.Pool)
	if err != nil {
		return nil, false
	}
	objectsRepo, err := dbrepos.NewArtifactObjectsRepository(cfg.Pool)
	if err != nil {
		return nil, false
	}
	grantsRepo, err := dbrepos.NewArtifactTransferGrantsRepository(cfg.Pool)
	if err != nil {
		return nil, false
	}
	permissionsRepo, err := dbrepos.NewArtifactBucketPermissionsRepository(cfg.Pool)
	if err != nil {
		return nil, false
	}
	return v2artifacts.NewHandler(
		artifactRepoAdapter{bucketsRepo, objectsRepo, grantsRepo, permissionsRepo},
		cfg.ObjectStore,
	), true
}

// bucketBootstrapRepoAdapter satisfies artifactbootstrap.Repository the same
// way artifactRepoAdapter satisfies the artifacts one: the two S6 repositories
// share no method names, so promotion supplies the union.
type bucketBootstrapRepoAdapter struct {
	*dbrepos.ArtifactBucketsRepository
	*dbrepos.ArtifactObjectsRepository
}

// supportProjectProvisioner adapts the project-create pipeline to the narrow
// interface internal/api/v2/supportassistant declares.
//
// The narrowing is the point: the support assistant may create ONE project,
// named by a constant, owned by the platform system identity, with default
// limits. It cannot choose plugins, quotas or admin roles, because nothing about
// bootstrapping a support project should be able to.
type supportProjectProvisioner struct {
	provisioner *projectprovisioning.Provisioner
}

func (p supportProjectProvisioner) Provision(
	ctx context.Context, request v2support.ProvisionRequest,
) (int64, error) {
	result, err := p.provisioner.Provision(ctx, projectprovisioning.Request{
		Name:    request.Name,
		OwnerID: request.OwnerID,
		// `elitea_core` is what the reference asks for
		// (`projects_create_project(plugins=['elitea_core'])`), and it is the
		// plugin list every chat-bearing project carries.
		Plugins:     []string{"elitea_core"},
		AdminEmails: []string{request.AdminEmail},
		AdminRoles:  []string{"admin"},
		Limits:      projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		return 0, err
	}
	return result.ProjectID, nil
}

// newProjectProvisioner builds the project-create pipeline (#333).
//
// ok is false without a pool, in which case the create route answers 503 rather
// than provisioning half a tenant. The bucket bootstrapper is separate and
// optional: a deployment with no object store still creates projects, it just
// creates them without artifact buckets — which is the state EVERY project is
// in today, so its absence cannot be a regression.
//
// The vector store is optional in the same way and for the same reason (#371),
// with one difference worth stating: a project created without it CAN be
// created and CANNOT index. Its absence is not a regression either, because no
// Go-created project could index before, but it is not a working project.
func newProjectProvisioner(cfg RouterConfig) (*projectprovisioning.Provisioner, bool) {
	if cfg.Pool == nil {
		return nil, false
	}
	// The secrets vault (#373). Not conditional: a project provisioned without
	// one cannot show its model catalogue, so the provisioner refuses to run
	// without this dependency rather than creating projects that look complete
	// and have no model picker. The handler is constructed here rather than
	// shared with the one mountSecretsRoutes builds because that one is built
	// inside the route tree; both read the same tables under the same
	// SECRETS_MASTER_KEY rule.
	options := []projectprovisioning.Option{
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(cfg.Pool)),
	}
	// The vector store (#371) IS conditional — see this function's doc comment.
	if cfg.ProjectVectorStore != nil {
		options = append(options, projectprovisioning.WithVectorStore(cfg.ProjectVectorStore))
	}
	if cfg.ObjectStore != nil {
		bucketsRepo, bucketsErr := dbrepos.NewArtifactBucketsRepository(cfg.Pool)
		objectsRepo, objectsErr := dbrepos.NewArtifactObjectsRepository(cfg.Pool)
		if bucketsErr == nil && objectsErr == nil {
			options = append(options, projectprovisioning.WithArtifactBuckets(
				artifactbootstrap.NewBootstrapper(
					bucketBootstrapRepoAdapter{bucketsRepo, objectsRepo},
					cfg.ObjectStore,
				),
			))
		}
	}
	return projectprovisioning.New(
		cfg.Pool,
		migrate.New(cfg.Pool, platformmigrations.Files),
		nil,
		options...,
	), true
}

// Permission strings from the existing configuration.artifacts.artifacts
// catalog (S11). edit is easy to miss — legacy uses it for retention and pin
// changes (PATCH), distinct from create (uploads, bucket/grant creation) and
// delete (DELETE, :batchDelete).
const (
	artifactPermissionView   = "configuration.artifacts.artifacts.view"
	artifactPermissionCreate = "configuration.artifacts.artifacts.create"
	artifactPermissionEdit   = "configuration.artifacts.artifacts.edit"
	artifactPermissionDelete = "configuration.artifacts.artifacts.delete"
	// The per-bucket ACL routes. legacy/plugins/artifacts declares these two
	// on api/v2/bucket_permissions.py (:36 for the GET, :69 and :123 for the
	// writes) — DISTINCT strings from the four above, because in pylon the
	// access list lives inside an s3_api_credentials row. Shared migration
	// 0118 grants them.
	artifactPermissionACLView = "configuration.artifacts.s3_credentials.view"
	artifactPermissionACLEdit = "configuration.artifacts.s3_credentials.edit"
)

// ArtifactDeps is mountArtifactRoutes' dependency bundle. Handler nil means
// every route falls back to notImplementedArtifact — the same degrade S7-S10
// already rely on — while Authenticate/Resolver still gate every request, so
// auth/RBAC enforcement never depends on the storage backend being wired.
type ArtifactDeps struct {
	Handler      *v2artifacts.Handler
	Authenticate func(http.Handler) http.Handler
	Resolver     platformauth.PermissionResolver
}

// mountArtifactRoutes registers all 24 artifact routes (13 from S7, plus
// S16's 3 native-multipart continuation routes, plus the 5 S3-shaped ones
// the Python SDK speaks: list, download, upload, delete, stat) on r, wrapped in
// deps.Authenticate and per-route RBAC (S11). Called once, from
// newProductionRouter, so the oapiserver conformance suite and production
// see an identical route shape. Deliberately NOT nested inside the /api/v2
// group: that group compresses JSON responses, which would buffer and encode
// every downloaded object rather than streaming it (S12).
func mountArtifactRoutes(r chi.Router, deps ArtifactDeps) {
	view := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionView)
	aclView := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionACLView)
	aclEdit := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionACLEdit)
	create := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionCreate)
	edit := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionEdit)
	del := apimw.RequireResolvedPermissions(deps.Resolver, platformauth.PermissionModeDefault, artifactPermissionDelete)

	// The S3-shaped routes below name their project in a QUERY parameter
	// rather than a path segment, so RequireResolvedPermissions (whose
	// extractor reads the {projectID} PATH param, and which fails closed —
	// gating nothing — when there is none) cannot express their gate. Each
	// tier below is deliberately the same resolver, mode and permission
	// string as its path-based counterpart above; only the extractor differs,
	// so the S3 representation cannot be a softer way in than the native
	// route it mirrors.
	byQueryProject := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireResolvedPermissionsForProject(
			deps.Resolver,
			platformauth.PermissionModeDefault,
			apimw.ProjectIDFromQuery("project_id"),
			permission,
		)
	}
	viewByQueryProject := byQueryProject(artifactPermissionView)
	// PUT is gated on `create`, the same tier as the native POST upload it
	// mirrors — not `edit`. An S3 PUT both creates and replaces, but so does
	// the native upload (?overwrite=true), which S11 maps to create; adding
	// edit here would let an edit-only principal create objects it cannot
	// create natively, because RequireResolvedPermissionsForProject takes the
	// INTERSECTION of the required set (any-of, see hasIntersection in
	// middleware/rbac.go) — listing two permissions widens the gate, it does
	// not narrow it.
	createByQueryProject := byQueryProject(artifactPermissionCreate)
	deleteByQueryProject := byQueryProject(artifactPermissionDelete)

	listBuckets, createBucket, getBucket, updateBucket, deleteBucket := notImplementedArtifact, notImplementedArtifact, notImplementedArtifact, notImplementedArtifact, notImplementedArtifact
	listObjects, uploadObject, batchDeleteObjects, downloadObject, statObject, deleteObject := notImplementedArtifact, notImplementedArtifact, notImplementedArtifact, notImplementedArtifact, notImplementedArtifact, notImplementedArtifact
	createTransferGrant, commitTransferGrant := notImplementedArtifact, notImplementedArtifact
	presignUploadPart, completeMultipartUpload, abortMultipartUpload := notImplementedArtifact, notImplementedArtifact, notImplementedArtifact
	listObjectsS3, downloadObjectS3 := notImplementedArtifact, notImplementedArtifact
	uploadObjectS3, deleteObjectS3, statObjectS3 := notImplementedArtifact, notImplementedArtifact, notImplementedArtifact
	listBucketPermissions, setBucketPermissions, deleteBucketPermission := notImplementedArtifact, notImplementedArtifact, notImplementedArtifact
	if deps.Handler != nil {
		listBuckets, createBucket, getBucket, updateBucket, deleteBucket =
			deps.Handler.ListBuckets, deps.Handler.CreateBucket, deps.Handler.GetBucket, deps.Handler.UpdateBucket, deps.Handler.DeleteBucket
		listObjects, uploadObject, batchDeleteObjects, downloadObject, statObject, deleteObject =
			deps.Handler.ListObjects, deps.Handler.UploadObject, deps.Handler.BatchDeleteObjects, deps.Handler.DownloadObject, deps.Handler.StatObject, deps.Handler.DeleteObject
		createTransferGrant, commitTransferGrant = deps.Handler.CreateTransferGrant, deps.Handler.CommitTransferGrant
		presignUploadPart, completeMultipartUpload, abortMultipartUpload =
			deps.Handler.PresignUploadPart, deps.Handler.CompleteMultipartUpload, deps.Handler.AbortMultipartUpload
		listObjectsS3, downloadObjectS3 = deps.Handler.ListObjectsS3, deps.Handler.DownloadObjectS3
		uploadObjectS3, deleteObjectS3, statObjectS3 =
			deps.Handler.UploadObjectS3, deps.Handler.DeleteObjectS3, deps.Handler.StatObjectS3
		listBucketPermissions, setBucketPermissions, deleteBucketPermission =
			deps.Handler.ListBucketPermissions, deps.Handler.SetBucketPermissions, deps.Handler.DeleteBucketPermission
	}

	r.Group(func(r chi.Router) {
		r.Use(deps.Authenticate)
		r.Route("/api/v2/artifacts", func(r chi.Router) {
			// Bucket plane — S8.
			r.With(view).Get("/buckets/{projectID}", listBuckets)
			r.With(create).Post("/buckets/{projectID}", createBucket)
			r.With(view).Get("/buckets/{projectID}/{bucket}", getBucket)
			r.With(edit).Patch("/buckets/{projectID}/{bucket}", updateBucket)
			r.With(del).Delete("/buckets/{projectID}/{bucket}", deleteBucket)

			// Object plane — S9. The three key-bearing routes use a trailing
			// chi wildcard, not the spec's literal {key} — see S7/S9 for why.
			r.With(view).Get("/objects/{projectID}/{bucket}", listObjects)
			r.With(create).Post("/objects/{projectID}/{bucket}", uploadObject)
			r.With(del).Post("/objects/{projectID}/{bucket}:batchDelete", batchDeleteObjects)
			r.With(view).Get("/objects/{projectID}/{bucket}/*", downloadObject)
			r.With(view).Head("/objects/{projectID}/{bucket}/*", statObject)
			r.With(del).Delete("/objects/{projectID}/{bucket}/*", deleteObject)

			// Transfer grants — S15. create for both: grant creation is
			// explicitly create per S11; commit is the write half of the same
			// grant lifecycle and the plan does not distinguish it.
			r.With(create).Post("/grants/{projectID}/{bucket}", createTransferGrant)
			r.With(create).Post("/grants/{projectID}/{grantID}:commit", commitTransferGrant)

			// Native multipart upload continuation — S16. Same permission
			// tier as the grant routes above: a part presign, a complete, and
			// an abort are all steps of the same create-time write lifecycle
			// CreateTransferGrant started, not a distinct RBAC category the
			// plan introduces.
			r.With(create).Post("/grants/{projectID}/{grantID}/parts/{partNumber}", presignUploadPart)
			r.With(create).Post("/grants/{projectID}/{grantID}:completeMultipart", completeMultipartUpload)
			r.With(create).Post("/grants/{projectID}/{grantID}:abortMultipart", abortMultipartUpload)

			// Per-bucket access lists — the legacy `bucket_permissions`
			// resource module. The MODE segment legacy carries
			// (`/bucket_permissions/default/{pid}`) is dropped here, as it is
			// on every other artifact route above: this surface resolves
			// PermissionModeDefault unconditionally, so a mode in the path
			// would name something the router does not read.
			r.With(aclView).Get("/bucket_permissions/{projectID}", listBucketPermissions)
			r.With(aclEdit).Put("/bucket_permissions/{projectID}", setBucketPermissions)
			r.With(aclEdit).Delete("/bucket_permissions/{projectID}", deleteBucketPermission)
		})

		// S3-shaped bucket listing and object read — the two calls the SDK's
		// artifact toolkit actually performs, and therefore the ones an index
		// run depends on: it lists the bucket, then downloads every listed
		// key (elitea-sdk runtime/tools/artifact.py, _base_loader then
		// _extend_data).
		//
		// It is mounted at the ROOT, not under /api/v2, because that is
		// where the request arrives: the worker's platform_origin is
		// validated to carry no path (elitea-worker-python
		// config.py:187-199), the SDK appends "/artifacts/s3" to it with no
		// api_v2_path (elitea-sdk client.py:115 — note every sibling URL on
		// the lines above it DOES include /api/v2, so the omission is the
		// contract, not an oversight), and the platform edge forwards the
		// path verbatim. Mounting this under /api/v2 would reproduce the
		// original defect: a 404 the SDK swallows into an empty listing,
		// leaving an index run green having indexed nothing.
		//
		// The project is a query parameter here, so this is the one
		// artifact route RequireResolvedPermissions (which reads the
		// {projectID} PATH param) cannot gate; viewByQueryProject applies
		// the identical resolver/mode/permission through the query
		// extractor instead. Nothing else about the authorization differs.
		r.With(viewByQueryProject).Get("/artifacts/s3/{bucket}", listObjectsS3)
		// The object read. A trailing wildcard, not a single {key} segment:
		// the SDK quotes the key with safe='/' (client.py:1176), so a nested
		// key arrives as literal path segments. Same `view` tier as the
		// listing and as the native download route — reading an object's
		// bytes is the same act as reading its metadata.
		r.With(viewByQueryProject).Get("/artifacts/s3/{bucket}/*", downloadObjectS3)

		// The object write verbs, on the same wildcard-key path as the read.
		// They complete the surface the SDK speaks: upload_artifact_s3 PUTs
		// raw bytes, delete_artifact_s3 DELETEs, head_artifact_s3 HEADs for
		// existence (elitea-sdk client.py:1123, :1186, :1206).
		//
		// The permission tiers mirror the native object plane exactly —
		// upload is `create`, delete is `delete`, and an existence check is
		// `view` because it reveals only what the native HEAD (statObject)
		// already reveals at the same tier. A write authorized by `view`
		// would make this surface strictly weaker than /api/v2.
		r.With(createByQueryProject).Put("/artifacts/s3/{bucket}/*", uploadObjectS3)
		r.With(deleteByQueryProject).Delete("/artifacts/s3/{bucket}/*", deleteObjectS3)
		r.With(viewByQueryProject).Head("/artifacts/s3/{bucket}/*", statObjectS3)
	})
}

// mountMCPServerRoutes registers the MCP server endpoints (issue 252 P2/P3) —
// the streamable-HTTP surface external MCP clients speak to.
//
//	GET|POST /app/{projectID}/mcp
//	GET|POST /app/{projectID}/mcp/{category}
//	GET|POST /app/{projectID}/mcp/{entity}/{entityVersionID}
//
// Mounted on its own Auth-wrapped subrouter for the same reason the artifact
// routes are: these paths live outside /api/v2 (pylon serves them from the app
// blueprint, and the internal-toolkit URLs written into existing projects
// hardcode `/app/{project_id}/mcp/...`), so they cannot inherit the /api/v2
// group's middleware, and they must not inherit its JSON compression.
//
// RequireProjectAccess is unconditional here. The endpoint reads one tenant's
// agents and toolkits by name, and authentication alone would let any
// authenticated PAT holder enumerate the tool inventory of a project they have
// nothing to do with. There is no client to regress, because these routes are
// new in this change — the same argument the skill-publishing block makes above.
//
// The two path variants are registered separately rather than as one wildcard
// because chi will not match a bare `/app/{projectID}/mcp` against a `/*`
// pattern; the handler reads the tail with chi.URLParam(r, "*"), which is empty
// for the first pair.
func mountMCPServerRoutes(
	r chi.Router,
	pool *pgxpool.Pool,
	authenticate func(http.Handler) http.Handler,
	agentStart v2mcp.AgentStartUseCase,
	toolkitRun v2mcp.ToolkitRunUseCase,
	personalProjects personalproject.AsyncEnsurer,
) {
	// The resolver ASKS for the personal project it could not find, for the
	// reason stated at `withPersonalProjects`: an MCP client authenticates
	// with a personal access token and never calls `/social/author`, so this
	// was one more reader that answered "no personal project" forever.
	resolver := apimw.NewDBPersonalProjectResolver(pool)
	if personalProjects != nil {
		resolver = resolver.WithPersonalProjectEnsurer(personalProjects)
	}
	handler := v2mcp.NewHandlerWithToolkitRuns(
		pool, resolver, agentStart, toolkitRun,
		legacyrbac.NewPostgresResolver(pool),
	)
	r.Group(func(r chi.Router) {
		r.Use(authenticate)
		r.Use(apimw.RequireProjectAccess(pool))
		r.Get("/app/{projectID}/mcp", handler.Endpoint)
		r.Post("/app/{projectID}/mcp", handler.Endpoint)
		r.Get("/app/{projectID}/mcp/*", handler.Endpoint)
		r.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	})
}

// compressJSONResponses gzips a JSON API response when the caller asks for it.
//
// DEFECT this fixes: the router had no compression middleware at all, so every
// JSON answer went over the wire verbatim. `GET /api/v2/configurations/available/`
// alone is a 136 KB catalogue that compresses to about 17 KB, and the credential
// form waits for the whole of it.
//
// The allow-list holds `application/json` only. Two response kinds must never be
// compressed, and both are excluded by it:
//
//   - Server-sent events. `text/event-stream` is not in the list, so chi writes
//     those through raw. `Flush` and `Unwrap` still reach the real writer
//     (chi v5.1.0 middleware/compress.go), which is what `pkg/ssewriter` and
//     `http.NewResponseController` need.
//   - A byte-range answer. chi compresses by content type without reading the
//     status code, so a 206 would be gzipped while `Content-Range` still
//     described the raw object. This middleware skips any request that carries a
//     `Range` header. The artifact object routes, the only 206 in the service
//     (internal/api/v2/artifacts/objects.go), also sit outside this group.
func compressJSONResponses() func(http.Handler) http.Handler {
	compress := chimw.Compress(5, "application/json")
	return func(next http.Handler) http.Handler {
		compressed := compress(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				next.ServeHTTP(w, r)
				return
			}
			compressed.ServeHTTP(w, r)
		})
	}
}

// newProductionRouter is the single route composition NewRouter builds. It
// carries the broad legacy-parity registration map alongside
// mountReviewedProductionRoutes (called at the end, below) because that is
// what cmd/elitea-main/main.go's composition root has always assembled: most
// of the routes registered here have not been assigned an exact legacy route
// policy, but real deployments need them anyway (#243).
func newProductionRouter(cfg RouterConfig) chi.Router {
	if cfg.AuthValidator == nil {
		cfg.AuthValidator = cfg.Auth.Validator
	}
	if cfg.PrincipalValidator == nil {
		cfg.PrincipalValidator = cfg.Auth.PrincipalValidator
	}
	if cfg.SessionHandler == nil {
		cfg.SessionHandler = cfg.Auth.SessionHandler
	}
	if cfg.OIDCHandler == nil {
		cfg.OIDCHandler = cfg.Auth.OIDCHandler
	}
	if cfg.SAMLHandler == nil {
		cfg.SAMLHandler = cfg.Auth.SAMLHandler
	}

	// The project-create pipeline, built ONCE and shared by every mount that
	// needs it: the projects route, the support assistant, and the two readers
	// of "the caller's personal project" below.
	//
	// It used to be built per call site. Three Provisioner values over one
	// pool, each with its own vault handler and bootstrappers, is the shape the
	// configurations handler in this file already warns about — "Building two
	// would let the write path and the read path disagree about which pool they
	// are on" — and an option added at one site and not the others would give
	// them three different behaviours for no visible reason.
	//
	// BELOW the defaulting above, not before it. It reads cfg.Pool,
	// cfg.ObjectStore and cfg.ProjectVectorStore, none of which that block
	// fills in today — but the block is where such a line WOULD go, and a
	// provisioner built from an unresolved field answers `ok = false`, which
	// switches project creation, the support assistant and personal-project
	// provisioning off together and reports nothing.
	projectProvisioner, projectProvisionerOK := newProjectProvisioner(cfg)
	// The personal-project ensurer over that one provisioner. Nil when the
	// composition has no pool, which every consumer tolerates.
	var personalProjects *personalproject.Ensurer
	if projectProvisionerOK {
		if ensurer, err := personalproject.NewEnsurer(cfg.Pool, projectProvisioner); err == nil {
			personalProjects = ensurer
		}
	}
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = cfg.Auth.SessionSecret
	}

	// THE PERSONAL PROJECT IS ASKED FOR AT SIGN-IN, not only when a screen
	// happens to read it.
	//
	// `GET /social/author` and the `/llm` project resolver already ask, and
	// both are lazy: a new user whose first screen calls neither is left with
	// no personal project and parks on `/onboarding`. A login is the one
	// moment every account passes through exactly once, so both federation
	// planes ask here as well. The call is idempotent and off the request
	// path, so a login that follows a hundred others costs two reads.
	//
	// It is attached HERE and not in cmd/elitea-main/main.go because the one
	// shared *personalproject.Ensurer is built in this file, from the one
	// project provisioner. A second ensurer composed in main.go would be a
	// second provisioner, which is the shape newProjectProvisioner warns about.
	if personalProjects != nil {
		cfg.OIDCHandler.WithPersonalProjectEnsurer(personalProjects)
		cfg.SAMLHandler.WithPersonalProjectEnsurer(personalProjects)
		cfg.Auth.OIDCHandler.WithPersonalProjectEnsurer(personalProjects)
		cfg.Auth.SAMLHandler.WithPersonalProjectEnsurer(personalProjects)
	}

	r := chi.NewRouter()
	permissionResolver := legacyrbac.NewPostgresResolver(cfg.Pool)

	// The audit-trail recorder. Built from the same pool the audit READS use,
	// for the reason stated at newProjectProvisioner: a write path and a read
	// path on two different pools can disagree about which database holds the
	// rows, and the Audit Trail would then be empty for a reason no log line
	// names. `NewPostgresRecorder(nil, …)` returns a typed nil, which
	// apimw.Audit recognises and answers by not mounting itself at all.
	auditRecorder := cfg.AuditRecorder
	if auditRecorder == nil {
		auditRecorder = audit.NewPostgresRecorder(cfg.Pool, nil)
	}

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		ExposedHeaders:   []string{"Content-Length"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	r.Use(apimw.RequestID)
	// Response headers that stop a browser reading an API body as a document.
	// Mounted here, in front of every route, because the per-handler copies
	// covered only the routes somebody remembered (see the middleware doc).
	r.Use(apimw.SecurityHeaders)
	// Keep the raw socket peer available to any trust-aware proxy resolver.
	// Generic RealIP processing before authentication would make X-Real-IP and
	// X-Forwarded-For caller-controlled trust inputs.
	r.Use(apimw.OtelMiddleware)
	r.Use(apimw.Recover)

	r.Mount("/", health.RoutesWithDeps(cfg.HealthDeps))

	// Traefik forward-auth endpoint (no auth middleware — this IS the auth check)
	forwardAuth := v2auth.NewForwardAuthHandler(cfg.AuthValidator)
	r.Get("/auth", forwardAuth.ServeHTTP)

	// Browser-facing OIDC session lifecycle. Legacy form authentication is not
	// mounted because auth_core__user has no password credential columns; the
	// previous prototype queried columns that do not exist in the legacy schema.
	if cfg.SessionHandler != nil {
		r.Route("/forward-auth", func(r chi.Router) {
			if cfg.OIDCHandler != nil {
				r.Get("/login", func(w http.ResponseWriter, req *http.Request) {
					http.Redirect(w, req, "/forward-auth/auth_oidc/login", http.StatusFound)
				})
			}
			r.Get("/logout", cfg.SessionHandler.Logout)
			r.Get("/info", cfg.SessionHandler.Info)
			r.Get("/auth_form/logout", cfg.SessionHandler.Logout)
			if cfg.OIDCHandler != nil {
				r.Get("/auth_oidc/login", cfg.OIDCHandler.Login)
				r.Get("/auth_oidc/callback", cfg.OIDCHandler.Callback)
			}
			r.Get("/auth_oidc/logout", cfg.SessionHandler.Logout)
			// SAML 2.0. Three routes, and the shapes are not
			// interchangeable: metadata and login are GET navigations, and
			// the assertion consumer service is a POST because the
			// authentication request asks for the HTTP-POST binding.
			//
			// `/auth_saml/logout` clears the local session, as the OIDC one
			// above does. Federated single logout — sending a LogoutRequest to
			// the identity provider's SLO endpoint — is NOT mounted: it needs
			// the session index of the assertion that started the session, and
			// this deployment's session cookie does not carry one. Mounting a
			// route that silently only cleared the local session would tell an
			// operator their users were signed out everywhere.
			if cfg.SAMLHandler != nil {
				r.Get("/auth_saml/metadata", cfg.SAMLHandler.Metadata)
				r.Get("/auth_saml/login", cfg.SAMLHandler.Login)
				r.Post("/auth_saml/acs", cfg.SAMLHandler.ACS)
				r.Get("/auth_saml/logout", cfg.SessionHandler.Logout)
			}
		})
	}

	// Static file serving for application icons (root level like pylon)
	iconDir := os.Getenv("ICON_DATA_DIR")
	if iconDir == "" {
		iconDir = "/data/static/application_icon"
	}
	toolIconDir := os.Getenv("TOOL_ICON_DATA_DIR")
	if toolIconDir == "" {
		toolIconDir = "/data/static/elitea_static-main/tool_icons"
	}
	r.Handle("/app/application_icon/*", http.StripPrefix("/app/application_icon/", http.FileServer(http.Dir(iconDir))))
	r.Handle("/app/application_tool_icon/*", http.StripPrefix("/app/application_tool_icon/", http.FileServer(http.Dir(toolIconDir))))

	// S20b: serves what coreHandler.UploadIcon (mounted further below, S9
	// object-store-backed) writes — public/unauthenticated for the same
	// reason as the two routes above: a browser <img src="..."> carries no
	// Authorization header.
	r.Get("/icons/{projectID}/{filename}", v2core.DownloadIcon(cfg.ObjectStore))
	// Social avatar: serves what CurrentSocialAvatar's upload handler writes,
	// public/unauthenticated for the same reason as /icons above.
	r.Get(v2social.CurrentAvatarDownloadPath, v2social.DownloadAvatar(cfg.ObjectStore))
	// The UI loads branding before a browser session exists, so this exact
	// static bootstrap route must remain public in both current-main and PoV.
	//
	// The pack is resolved from three layers (ADR-0024): the product default
	// under the BRAND_PACK_PATH file under the admin-authored `branding`
	// section, read through cfg.Pool and cached briefly. The admin routes
	// below invalidate the same resolver on a save.
	brandingResolver := cfg.Branding
	if brandingResolver == nil {
		brandingResolver = v2branding.NewResolver(v2branding.ResolverConfig{
			PackPath: cfg.BrandPackPath,
			Pool:     cfg.Pool,
		})
	}
	brandingHandler := v2branding.NewHandler(v2branding.Config{Resolver: brandingResolver})
	// A nil *Composer must stay a nil INTERFACE for the With* options to
	// recognise "no mailer"; a typed nil inside the interface would pass
	// their check and panic on first use.
	var (
		adminMailer    admin.Mailer
		inviteMailer   v2core.InviteMailer
		decisionMailer v2moderation.DecisionMailer
	)
	if cfg.Mailer != nil {
		adminMailer, inviteMailer, decisionMailer = cfg.Mailer, cfg.Mailer, cfg.Mailer
	}
	brandingPackages := cfg.BrandingPackages
	r.Get("/api/v2/branding/bootstrap.js", brandingHandler.Bootstrap)
	r.Head("/api/v2/branding/bootstrap.js", brandingHandler.Bootstrap)
	// Uploaded brand assets (ADR-0024 decision 3): public for the same
	// reason as /icons — <img src>, <link rel="icon"> and @font-face fetches
	// carry no credential. Content-addressed and immutable; the route
	// applies the icon route's hardening to every object it serves.
	brandingAssets := v2branding.NewAssetStore(cfg.ObjectStore)
	r.Get(v2branding.AssetPathPrefix+"{kind}/{file}", brandingAssets.Download)
	r.Head(v2branding.AssetPathPrefix+"{kind}/{file}", brandingAssets.Download)

	// Share a conversation by link — the ANONYMOUS half.
	//
	// It is registered HERE, on the root mux, for one reason: everything below
	// `r.Group(...)`/`r.Use(apimw.Auth(...))` is authenticated, and these two
	// routes must not be. This is the same placement /icons, the social avatar
	// download and the branding bootstrap already use — a route mounted above
	// the Auth group, not a bypass threaded through it. There is deliberately
	// no "skip auth for this path" branch inside the middleware: a path-matching
	// exemption is a second, weaker copy of the routing table, and the first
	// time the two disagree the exemption wins.
	//
	// The paths carry their full /api/v2 prefix because they are siblings of
	// the /api/v2 subrouter rather than children of it. Chi allows that (the
	// branding bootstrap above does the same), and it is what keeps the URL the
	// SPA already speaks.
	mountSharedChatAnonymousRoutes(r, cfg)

	// The INBOUND pipeline trigger — issue 192.
	//
	// Registered HERE, on the root mux, for exactly the reason the two routes
	// above are: everything below `r.Group(...)`/`r.Use(apimw.Auth(...))` is
	// authenticated, and this route must not be. Its only credential is the
	// per-pipeline secret, compared in constant time against a digest stored
	// beside the row; see internal/api/v2/pipelinetriggers for the four
	// authorization rules and for why nothing the caller sends selects a
	// tenant.
	//
	// It carries its full /api/v2 prefix because it is a SIBLING of the
	// /api/v2 subrouter rather than a child of it, which is what keeps the URL
	// an external system was given stable.
	if cfg.PipelineTriggers != nil {
		r.Post(v2pipelinetriggers.InboundPath, cfg.PipelineTriggers.Trigger)
	}

	// Served API docs (S251): the legacy shared plugin's openapi/swagger-ui
	// routes had no Go counterpart at all — public/unauthenticated like the
	// branding bootstrap above, since API documentation predates any session.
	openapiDocsHandler := v2openapidocs.NewHandler()
	r.Get("/api/openapi.yaml", openapiDocsHandler.Spec)
	r.Get("/api/openapi.json", openapiDocsHandler.SpecJSON)
	r.Get("/docs", openapiDocsHandler.UI)

	// Admin UI SPA — serves the admin panel with server-side config injection
	if cfg.AdminUI != nil {
		adminUIHandler := adminui.NewHandler(*cfg.AdminUI)
		r.Mount(cfg.AdminUI.BasePath, adminUIHandler.Routes())
	}

	// Strip doubled /api/v2/api/v2/... prefix caused by admin_ui RTK Query
	// baseUrl + explicit V2_BASE prefix in projectsApi/configurationApi/serviceDescriptorsApi
	r.HandleFunc("/api/v2/api/v2/*", func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api/v2")
		req.URL.RawPath = ""
		r.ServeHTTP(w, req)
	})

	// Artifacts (S11): mounted here, outside the /api/v2 group below, on its
	// own Auth-wrapped subrouter so it never inherits that group's JSON
	// compression.
	artifactResolver := cfg.ArtifactPermissionResolver
	if artifactResolver == nil {
		artifactResolver = permissionResolver
	}

	// projectPermission gates one route on the named legacy permission,
	// resolved in DEFAULT mode against the {projectID} path segment.
	//
	// It is declared HERE, above every group that uses it, rather than inside
	// /elitea_core where #302 first wrote it. /notifications, /context_manager
	// and the /admin project listings are SIBLINGS of that group, not children,
	// so a helper local to it could not reach them — and each of those groups
	// names {projectID} in every path and then reads that project's rows.
	//
	// cfg.ProjectPermissionResolver overrides the live resolver for the route
	// tests, exactly as cfg.ArtifactPermissionResolver does above. Production
	// leaves it unset and gets permissionResolver. It is resolved through a
	// local for the reason the /elitea_core note gives:
	// TestNilGatedRouterFieldsAreWiredOrDeclared reads this file as TEXT and
	// treats a nil-comparison against a config field as a gate that decides
	// REGISTRATION. These two decide only the authorization answer; the same
	// routes register either way.
	coreResolver := cfg.ProjectPermissionResolver
	if coreResolver == nil {
		coreResolver = permissionResolver
	}
	projectPermission := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireResolvedPermissions(
			coreResolver, platformauth.PermissionModeDefault, permission)
	}
	// requireAnalyticsEnabled gates the `/analytics*` HTTP surface on the same
	// `analytics_enabled` platform_config flag the admin Features page's
	// Analytics section writes (config_schemas.go's analyticsSection) and
	// GET /elitea_core/platform_settings/… reads for the Settings > Analytics
	// tab (eliteacore/platform_flags.go's analyticsFlags).
	//
	// This is the server-side half of that switch, in the shape of
	// eliteacore's requireMCPEnabled: hiding the tab alone would leave every
	// one of these routes open to a client that kept a URL and never asked the
	// UI. A load error is read as ENABLED, the same permissive direction every
	// other flag read in platformconfig takes — a database hiccup here must
	// not silently take Analytics offline for every deployment.
	requireAnalyticsEnabled := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values, err := platformconfig.Load(r.Context(), cfg.Pool, platformconfig.SectionAnalytics)
			enabled := true
			if err == nil {
				enabled = values.Bool(platformconfig.KeyAnalyticsEnabled, true)
			}
			if !enabled {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Analytics is disabled on this deployment"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	// The configurations handler, built ONCE and mounted twice: on
	// /api/v2/configurations for the project-scoped compatibility surface, and
	// on /api/v2/admin/gateway/providers for the platform-wide provider surface
	// (internal/api/v2/configurations/global_providers.go).
	//
	// One construction, so the vault sealer, the provider admission and the
	// public project id cannot differ between the two. A second NewHandler is
	// how one surface acquires a vault the other lacks and starts storing
	// api_keys in plaintext rows every tenant can read.
	//
	// `WithPublicProjectID` had NO caller before this, and its own doc comment
	// said so: the list response's `shared` block — the one that shows a project
	// which platform credentials it may use — was empty in every default
	// install, because the option was applied only inside a feature-flagged
	// branch. It resolves from the same helper the Project middleware uses, so
	// the two cannot disagree about which schema holds the shared rows.
	configurationsHandler := v2configs.NewHandler(
		cfg.Pool,
		v2configs.WithPermissionResolver(coreResolver),
		v2configs.WithConnectionChecker(cfg.ConfigConnectionChecker),
		v2configs.WithProviderAdmission(cfg.ConfigProviderAdmission),
		v2configs.WithStoredConfigurationResolver(cfg.ConfigStoredResolver),
		v2configs.WithSecretSealer(configurationSecretSealer(cfg.Pool)),
		v2configs.WithPublicProjectID(apimw.PublicProjectID()),
	)
	artifactHandler := cfg.ArtifactHandler
	if artifactHandler == nil {
		artifactHandler, _ = newArtifactHandler(cfg)
	}
	// Declared at function scope (not inside the "Conversations" block below,
	// where it used to live) so the Support Assistant mount can reuse this
	// EXACT instance for its own attachment routes (issue #625 item 2) —
	// the same object store, the same attachment metadata tables, the same
	// AddAttachments code, not a second implementation of any of them.
	var convHandler *v2convs.Handler
	authenticate := apimw.Auth(apimw.AuthConfig{
		Validator:                  cfg.AuthValidator,
		PrincipalValidator:         cfg.PrincipalValidator,
		ForwardedIdentityVerifier:  cfg.Auth.ForwardedIdentityVerifier,
		SessionSecret:              cfg.SessionSecret,
		SessionStore:               cfg.Auth.SessionStore,
		RejectLegacySessionCookies: cfg.Auth.RejectLegacySessionCookies,
	})
	mountArtifactRoutes(r, ArtifactDeps{
		Handler:      artifactHandler,
		Authenticate: authenticate,
		Resolver:     artifactResolver,
	})

	// The MCP server (issue 252). Outside the /api/v2 group for the reasons in
	// mountMCPServerRoutes.
	mountMCPServerRoutes(r, cfg.Pool, authenticate, cfg.MCPAgentStart, cfg.MCPToolkitRun,
		personalProjects)

	// This group holds the whole JSON API — the `/api/v2` route below is its
	// only member. Compression sits at the top of it, so every handler in the
	// group writes plain JSON and only the outermost layer encodes it.
	r.Group(func(r chi.Router) {
		r.Use(compressJSONResponses())
		r.Use(apimw.Auth(apimw.AuthConfig{
				Validator:                  cfg.AuthValidator,
			PrincipalValidator:         cfg.PrincipalValidator,
			ForwardedIdentityVerifier:  cfg.Auth.ForwardedIdentityVerifier,
			SessionSecret:              cfg.SessionSecret,
			SessionStore:               cfg.Auth.SessionStore,
			RejectLegacySessionCookies: cfg.Auth.RejectLegacySessionCookies,
		}))

		// Maintenance mode, immediately AFTER authentication and before
		// anything that does work.
		//
		// The ordering is load-bearing. It has to be after Auth, because the
		// only caller it admits is one whose administration permissions can be
		// resolved, and that needs a principal on the context. It has to be
		// above everything that does work, so a refused request reaches no
		// handler.
		r.Use(apimw.Maintenance(apimw.MaintenanceConfig{
			Pool:     cfg.Pool,
			Resolver: permissionResolver,
		}))

		// The audit-trail emitter for `centry.audit_events` — the producer the
		// admin Audit Trail page never had (internal/api/middleware/audit.go
		// carries the four decisions: level, scope, failure policy, retention).
		//
		// Placed here, and this is the only correct place in this chain:
		//
		//   - BELOW Auth, so the row carries the principal. Above it, the
		//     handler's context — where the principal lives — is invisible,
		//     because a context flows down and never back up.
		//   - ABOVE the per-route RBAC gates, so a caller refused a permission
		//     (403) is recorded rather than silently disappearing — which is
		//     the question an audit trail is most often opened to answer.
		r.Use(apimw.Audit(auditRecorder))

		r.Route("/api/v2", func(r chi.Router) {
			// THE GROUP'S OWN "no such route" ANSWER (F3).
			//
			// Auth runs ABOVE this subrouter, so a path nobody registered is
			// already authenticated by the time chi looks for it. That
			// ordering is the contract: an unknown path answers 404 to a
			// caller with a credential and 401 only to one without. A retired
			// path that answers 401 to a logged-in browser is read by the SPA
			// as an expired session, and it opens a fresh OIDC round-trip on
			// every visit to the page that still calls it.
			//
			// What was missing is the BODY. chi's default fallback writes
			// `404 page not found` as text/plain, so the one answer a client
			// cannot parse is the answer it gets for a route that moved. These
			// two make the group's misses typed, like every other error it
			// returns, and chi propagates them into the subrouters mounted
			// below that declare none of their own.
			r.NotFound(func(w http.ResponseWriter, r *http.Request) {
				apierr.WriteStatus(w, http.StatusNotFound, "not found")
			})
			r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
				apierr.WriteStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			})

			mountRuntimeRoutes(r, cfg.RuntimeRoutes)

			// The PRE-BUILT MCP server catalogue (shared migration 0094) and
			// the platform vault its client secrets are sealed into.
			//
			// One store and one vault seam, built once and given to BOTH the
			// admin surface that writes the catalogue and the eliteacore
			// handlers that read it. Building two would let the write path and
			// the read path disagree about which pool they are on — the shape
			// of #128, where a route answered while its backing was never
			// composed.
			//
			// v2secrets.NewHandler is used as the vault here for the same reason
			// projectprovisioning uses it: it is the one type in this service
			// that can open and write a centry vault.
			prebuiltMCPStore := mcpregistry.NewPrebuiltStore(cfg.Pool)
			prebuiltMCPVault := v2secrets.NewHandler(cfg.Pool)

			// The TYPED identity provider definitions (shared migration 0095) —
			// the real surface behind the Configuration page's "Authentication"
			// section. It shares the vault above rather than opening a second
			// one, for the reason stated there: two vault handlers on two pools
			// is how a write path and a read path come to disagree about which
			// database holds the credential.
			identityProviderStore := identityproviders.NewStore(cfg.Pool)

			// The toolkit TYPE availability policy (shared migration 0114) —
			// the store behind `Admin › Toolkits` AND behind the served
			// catalogue's project filter. ONE store for both, so the page and
			// the catalogue cannot disagree about which types a project gets.
			toolkitTypePolicyStore := toolkitcatalogue.NewStore(cfg.Pool)

			// Outbound e-mail settings (gap G7). The SAME vault handler again,
			// for the reason stated above: the SMTP password is sealed into the
			// hidden bucket, and a second handler on a second pool is how the
			// page that seals it and the resolver that reads it come to
			// disagree about which vault holds it.
			emailResolver := cfg.EmailSettings
			if emailResolver == nil {
				emailResolver = emailsettings.NewResolver(
					emailsettings.NewStore(cfg.Pool, prebuiltMCPVault), emailsettings.Settings{}, "")
			}

			coreHandler := v2core.NewHandler(
				cfg.Pool,
				v2core.WithPermissionResolver(permissionResolver),
				v2core.WithObjectStore(cfg.ObjectStore),
				v2core.WithInviteMailer(inviteMailer),
				v2core.WithPrebuiltMCPCatalogue(prebuiltMCPStore, prebuiltMCPVault),
				// `cost_budgets_enabled`, from the one fact this process holds
				// about whether LLM cost is tracked at all: a gateway address
				// was configured. cfg.GatewayStatus is built from
				// LLM_GATEWAY_URL and is left nil when that is empty
				// (cmd/elitea-main/main.go), so the nil check IS the
				// "is the gateway composed" question — and it is a nil
				// INTERFACE check that holds, because that field is documented
				// never to receive a boxed nil pointer.
				v2core.WithCostBudgets(cfg.GatewayStatus != nil),
			)

			// === Auth endpoints ===
			//
			// Sign a personal access token with the key that THIS deployment's
			// validator reads it back with. A form authentication graph holds
			// that key; SessionSecret is a different value and signing with it
			// produced tokens that failed every later request with 401.
			authOptions := []v2auth.Option{
				v2auth.WithPermissionResolver(permissionResolver),
			}
			if cfg.PATSigner != nil {
				authOptions = append(authOptions, v2auth.WithTokenSigner(cfg.PATSigner))
			} else {
				authOptions = append(authOptions, v2auth.WithTokenSigningKey(cfg.SessionSecret))
			}
			r.Mount("/auth", v2auth.NewHandler(cfg.Pool, authOptions...).Routes())

			// === Projects endpoints ===
			//
			// The resolver is passed into the handler rather than wrapped around
			// the mount: the three group WRITES are gated per route inside
			// `Routes()`, because chi cannot carry a per-route gate across a
			// Mount boundary and the project LIST must stay open to any member.
			projectOptions := []v2projects.Option{
				v2projects.WithPermissionResolver(permissionResolver),
			}
			if projectProvisionerOK {
				projectOptions = append(projectOptions, v2projects.WithProvisioner(projectProvisioner))
			}
			r.Mount("/projects", v2projects.NewHandler(cfg.Pool, projectOptions...).Routes())

			// === Admin endpoints ===
			//
			// The Tasks page's job store and its evaluation-run cancel. Both
			// are built here rather than inside the handler for the reason
			// every other store is: a nil pool means the option is skipped and
			// the routes answer 503, instead of the handler discovering it has
			// no database at request time.
			var backgroundJobsStore *dbrepos.AdminBackgroundJobsRepository
			if cfg.Pool != nil {
				if store, err := dbrepos.NewAdminBackgroundJobsRepository(cfg.Pool); err == nil {
					backgroundJobsStore = store
				}
			}
			adminOptions := []admin.Option{}
			if backgroundJobsStore != nil {
				adminOptions = append(adminOptions,
					admin.WithBackgroundJobs(backgroundJobsStore),
					// The SAME repository method the evaluation API's own
					// cancel route calls, adapted only to drop the row this
					// page does not read — see admin.EvalRunCanceller.
					admin.WithEvalRunCancel(adminEvalRunCanceller{
						runs: dbrepos.NewEvalRunsRepo(cfg.Pool),
					}),
				)
			}
			adminHandler := admin.NewHandler(
				cfg.Pool,
				append([]admin.Option{
					admin.WithPermissionResolver(permissionResolver),
					admin.WithToolkitRegistry(cfg.ToolkitRegistry),
					admin.WithPrebuiltMCPCatalogue(prebuiltMCPStore, prebuiltMCPVault),
					admin.WithIdentityProviders(identityProviderStore, prebuiltMCPVault),
					admin.WithBranding(brandingResolver),
					admin.WithBrandingAssets(brandingAssets),
					admin.WithMailer(adminMailer),
					admin.WithEmailSettings(emailResolver.Store(), emailResolver),
					admin.WithBrandingPackages(brandingPackages),
					// The same store the SCIM tree writes through. One store, so
					// the screen and a group push can never disagree about which
					// project a binding names.
					admin.WithSCIMGroupBindings(scimdirectory.NewStore(cfg.Pool)),
					// The toolkit type policy the served catalogue also reads.
					admin.WithToolkitTypePolicy(toolkitTypePolicyStore),
				}, adminOptions...)...,
			)
			moderationHandler := v2moderation.NewHandler(cfg.Pool, v2moderation.WithMailer(decisionMailer))
			// The admin panel's surface. Every route below is gated on the same
			// pylon permission its Python counterpart declares in
			// legacy/plugins/admin/api/v2/, resolved from the database in
			// `administration` mode. The admin SPA's
			// `window.admin_ui_config.permissions` is PRESENTATION state and is
			// never consulted here.
			//
			// Unit A14 gated the auth_users WRITES only and left that listing —
			// and the whole plugin-config/runtime block — open, to avoid a
			// behaviour change with blast radius outside that unit: no
			// deployment bootstrapped by elitea-migrate had a single
			// administration-mode role, so gating a read would have turned
			// "the admin panel works" into "403 for everyone". That is fixed at
			// the source — migrations/shared/0060_admin_central_rbac.sql seeds
			// the administration roles and their grants, and
			// internal/infra/db/migrations/001_initial.sql seeds them on a fresh
			// database — so the remaining reads are gated to match pylon too.
			//
			// Read this as the parity table it is. `mode` in the URL does NOT
			// select the permission mode: pylon reaches these handlers only
			// through its `administration` AdminAPI, so the resolution mode is
			// always `administration`.
			central := func(permission string) func(http.Handler) http.Handler {
				return apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					permission,
				)
			}
			// auth_users.py, user_suspend.py
			requireAdminUsers := central("admin.auth.users")
			// system_info.py, plugin_config_*.py, maintenance.py, runtime_*.py,
			// tasks.py, active_tasks.py — all declare ["runtime.plugins"].
			requireRuntimePlugins := central("runtime.plugins")
			// moderation_status.py / moderation_statuses.py. The queue itself is
			// registered further down, next to the three other moderation routes
			// this unit added, so all four read as one surface.
			requireModeration := central("admin.moderation")
			// The admin PROJECTS surface. projects.py declares
			// `projects.projects.projects.view` and project_suspend.py declares
			// `projects.projects.projects.edit`.
			//
			// The listing is gated rather than open: a project row names the
			// project, its owner and its admins across every tenant, so the
			// listing itself is the sensitive part. The same argument applies
			// verbatim to the admin USER listing, which was left open only
			// because it predated the migration that makes any of this
			// resolvable.
			requireProjectsView := central("projects.projects.projects.view")
			requireProjectsEdit := central("projects.projects.projects.edit")

			// SCIM 2.0 user provisioning (shared migration 0096).
			//
			// Mounted UNDER /api/v2, not at the root, so it inherits the one
			// authentication middleware this service has rather than acquiring a
			// second identity check. The base URL an operator pastes into their
			// identity provider is therefore `https://<host>/api/v2/scim/v2`;
			// internal/api/scim states that in its package comment, next to the
			// paths themselves.
			//
			// The gate is `admin.auth.users` in administration mode — the SAME
			// permission the admin Users page's write routes carry above. A SCIM
			// client creating and deactivating accounts is doing what that page
			// does, so no new permission string arrives here and the grant gate
			// in router_permission_grant_gate_test.go stays untripped.
			// `/Groups` is served too, and the gate does not change with it. An
			// identity provider presents ONE credential for both resources, so
			// a second permission on the group half would stop every SCIM
			// client already configured against this deployment. What a group
			// push can do is bounded by the BINDING an administrator authored
			// under `/admin/scim_group_bindings/administration` (below, on the
			// project-membership permission), not by a second gate here: a push
			// cannot choose a project, cannot choose a role, and cannot create
			// or delete either.
			r.With(requireAdminUsers).Mount(scimapi.MountPath,
				scimapi.NewHandler(scimdirectory.NewStore(cfg.Pool)).Routes())

			r.Route("/admin", func(r chi.Router) {
				// Admin panel endpoints (administration mode, no projectID)
				//
				// The two `prompt_lib` routes stay open ON PURPOSE. They are the
				// help-center's version/resource lookup, reached in pylon through
				// `PromptLibAPI`, whose `get()` carries NO check_api decorator at
				// all (system_info.py, plugin_config_values.py) — unlike the
				// `AdminAPI` siblings one line below. chi matches a static
				// segment ahead of a `{param}`, so these keep winning over
				// `/system_info/{mode}` and `/plugin_config_values/{mode}/{plugin}`.
				r.Get("/system_info/prompt_lib", adminHandler.SystemInfo)
				// The Help Center's own read (unit A14). pylon exposes exactly
				// ONE config section to non-administrators —
				// `_PUBLIC_SECTIONS = {"resources"}` in
				// legacy/plugins/admin/api/v2/plugin_config_values.py — and this
				// route is restricted the same way, by being a route rather than
				// a `{section}` parameter a caller could substitute. It now
				// serves that section; it used to return chat and upload limits,
				// which is why every Help Center card said "No links configured".
				r.Get("/plugin_config_values/prompt_lib/resources", adminHandler.PromptLibResourcesValues)

				r.With(requireRuntimePlugins).Get("/system_info/{mode}", adminHandler.SystemInfo)
				// Before this gate any authenticated session could read the
				// global user list — id, name, email, last_login, suspended and
				// administration role for every row of auth_core__user.
				r.With(requireAdminUsers).Get("/auth_users/{mode}", adminHandler.AuthUsers)
				r.With(requireAdminUsers).Post("/auth_users/{mode}", adminHandler.AuthUsersAction)
				r.With(requireAdminUsers).Put("/user_suspend/{mode}/{userID}", adminHandler.UserSuspend)
				// The MODES registry and the GLOBAL invite (#255). Both are
				// pylon `AdminAPI`-only surfaces — `mode_handlers` names
				// `administration` and nothing else — so the segment is STATIC
				// and another mode 404s rather than reaching a handler that
				// would have to guess what it meant. Both declare
				// `["modes.users"]` in their check_api decorators.
				//
				// `/modes/administration` writes auth_core__user_role, the same
				// table `/auth_users` set_admin_role writes, so it carries the
				// same super_admin escalation guard — otherwise it would be a
				// second, unguarded route to the platform's highest role.
				requireModeUsers := central("modes.users")
				r.With(requireModeUsers).Get("/modes/administration", adminHandler.Modes)
				r.With(requireModeUsers).Post("/modes/administration", adminHandler.ModesAssign)
				r.With(requireModeUsers).Delete("/modes/administration", adminHandler.ModesRemove)
				r.With(requireModeUsers).Post("/user_invite/administration", adminHandler.UserInvite)
				// The CROSS-PROJECT bulk membership invite (issue 247,
				// internal/api/v2/admin/invites_bulk.go). pylon serves this as
				// two console pages — legacy/plugins/admin/api/v2/
				// invites_bulkusers.py and invites_bulkprojects.py — and each
				// declares its OWN permission rather than the project-membership
				// one. Both strings gate this route, ANY-of, so an operator who
				// held either page holds the one route that replaces both.
				// Shared migration 0121 grants them; without it the route would
				// answer 403 on a clean database, which is the class
				// router_permission_grant_gate_test.go exists to catch.
				//
				// The mode segment is STATIC. pylon's `mode_handlers` names
				// `administration` and nothing else on both modules, so another
				// mode 404s rather than reaching a handler that would have to
				// guess what it meant.
				r.With(apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					admin.BulkInviteUsersPermission, admin.BulkInviteProjectsPermission,
				)).Post("/invites_bulk/administration", adminHandler.BulkInviteMembers)
				// The personal/team project permission editor (#255) — the
				// matrix every ORDINARY project gets, as opposed to the named
				// scopes `/permissions/{scope}/{mode}` edits. Gated on the
				// permissions user_project_permissions.py declares, resolved
				// centrally: it writes across every personal (or every shared)
				// project at once, and the operator doing that is a member of
				// none of them.
				r.With(central(admin.UserProjectPermissionsViewPermission)).
					Get("/user_project_permissions/administration", adminHandler.UserProjectPermissions)
				r.With(central(admin.UserProjectPermissionsEditPermission)).
					Put("/user_project_permissions/administration", adminHandler.UserProjectPermissionsSave)
				// The SCIM group bindings (shared migration 0098): which
				// identity provider group grants which role on which project.
				//
				// Gated on the SAME pair as the editor above, and deliberately
				// so. Authoring a binding is authorising a directory to put
				// people into a project with a role, which is exactly what
				// `configuration.roles.user_project_permissions` governs. No new
				// permission string arrives, so the grant gate in
				// router_permission_grant_gate_test.go stays untripped.
				//
				// The mode segment is static `administration`: a binding is a
				// deployment fact with no project-scoped view, so another mode
				// 404s rather than being answered under a scope that does not
				// apply.
				r.With(central(admin.UserProjectPermissionsViewPermission)).
					Get("/scim_group_bindings/administration", adminHandler.SCIMGroupBindingList)
				// The roles a project REALLY has, for the binding editor's role
				// control. `/admin/roles/{mode}/{projectID}` answers a hardcoded
				// admin/editor/viewer for a project with no role rows, which
				// would make this control offer a role the save then refuses.
				//
				// A static segment ahead of `{id}`: chi matches the literal
				// first, so this cannot shadow the binding routes below.
				r.With(central(admin.UserProjectPermissionsViewPermission)).
					Get("/scim_group_bindings/administration/project_roles/{projectID}",
						adminHandler.SCIMGroupBindingProjectRoles)
				r.With(central(admin.UserProjectPermissionsEditPermission)).
					Post("/scim_group_bindings/administration", adminHandler.SCIMGroupBindingCreate)
				r.With(central(admin.UserProjectPermissionsEditPermission)).
					Put("/scim_group_bindings/administration/{id}", adminHandler.SCIMGroupBindingSave)
				r.With(central(admin.UserProjectPermissionsEditPermission)).
					Delete("/scim_group_bindings/administration/{id}", adminHandler.SCIMGroupBindingDelete)
				// The admin Roles page. Before it, only the GET existed —
				// ungated, ignoring {scope}, and listing only already-granted
				// permissions. See internal/api/v2/admin/roles.go.
				//
				// Gated on the permissions the pylon originals declare
				// (`configuration.roles.permissions.view` / `.edit`,
				// legacy/plugins/admin/api/v2/permissions.py), resolved from
				// auth_core__user_role per request. The read is gated too: this
				// matrix is the deployment's authorisation model, and knowing
				// which role holds which privilege is itself sensitive.
				requireRolesView := central("configuration.roles.permissions.view")
				requireRolesEdit := central("configuration.roles.permissions.edit")
				r.With(requireRolesView).Get("/permissions/{scope}/{mode}", adminHandler.AdminPermissions)
				r.With(requireRolesEdit).Put("/permissions/{scope}/{mode}", adminHandler.AdminPermissionsSave)
				r.With(requireRolesEdit).Post("/permissions/{scope}/{mode}", adminHandler.AdminPermissionsSync)
				r.With(requireProjectsView).Get("/projects/{mode}", adminHandler.Projects)
				// `ProjectSuspend` existed in the admin package before this unit
				// but was mounted on NO route — dead code with no caller. It is
				// the only project WRITE this unit implements: create and delete
				// are multi-system provisioning pipelines (tenant schema, object
				// storage, vault, RabbitMQ, InfluxDB, a system user and its
				// token — legacy/plugins/projects/utils/project_steps.py) and
				// are rendered unavailable in the UI rather than guessed at here.
				r.With(requireProjectsEdit).
					Put("/project_suspend/{mode}/{projectID}", adminHandler.ProjectSuspend)
				r.With(requireRuntimePlugins).Get("/plugin_config_schemas/{mode}", adminHandler.PluginConfigSchemas)
				// The admin Configuration page's read and write (unit A14).
				// The `{mode}` pair they replace answered 200 for EVERY section:
				// the GET with the schema's defaults for all of them at once
				// (ignoring the segment), the PUT with an empty object and a
				// success flag, without decoding its request body.
				//
				// `administration` is a STATIC segment, so chi's trie prefers it
				// and a static segment binds no `{mode}` parameter — the trap
				// #207's test caught. These handlers state their mode rather
				// than sniffing it from the URL. Sections that additionally
				// declare a `required_permission` are checked INSIDE the
				// handler, because that permission depends on the section and
				// route middleware cannot see it.
				//
				// pylon registers no handler for any other mode on this path
				// except `prompt_lib`/resources above, so there is deliberately
				// no `{mode}` fallback: another mode 404s rather than being
				// answered with something plausible.
				r.With(requireRuntimePlugins).
					Get("/plugin_config_values/administration/{plugin}", adminHandler.AdministrationPluginConfigValues)
				r.With(requireRuntimePlugins).
					Put("/plugin_config_values/administration/{plugin}", adminHandler.AdministrationPluginConfigValuesSave)
				// The PRE-BUILT MCP server catalogue — the real surface behind
				// the Configuration page's "MCP Servers" section, which stays
				// unavailable because its rows are plaintext and a catalogue
				// entry carries a client secret (config_schemas.go).
				//
				// The permission is the SAME `runtime.plugins` the section it
				// replaces already required, so an operator who could edit that
				// section can edit the catalogue, and no new permission string
				// arrives without a grant (router_permission_grant_gate_test.go).
				//
				// The mode segment is static `administration`: the catalogue is
				// platform-wide and there is no project-scoped view of it, so
				// another mode 404s rather than being answered with the same
				// rows under a scope that does not apply to them.
				r.With(requireRuntimePlugins).
					Get("/mcp_prebuilt_servers/administration", adminHandler.PrebuiltMCPList)
				r.With(requireRuntimePlugins).
					Put("/mcp_prebuilt_servers/administration/{key}", adminHandler.PrebuiltMCPSave)
				r.With(requireRuntimePlugins).
					Delete("/mcp_prebuilt_servers/administration/{key}", adminHandler.PrebuiltMCPDelete)
				// The TYPED identity provider definitions — the real surface
				// behind the Configuration page's "Authentication" section,
				// which stays unavailable because its rows are plaintext and a
				// provider carries a client secret (config_schemas.go).
				//
				// The permission is the SAME `runtime.plugins` that section
				// already required, so an operator who could edit it can author
				// a provider, and no new permission string arrives without a
				// grant (router_permission_grant_gate_test.go).
				//
				// The mode segment is static `administration`: federation is a
				// deployment fact with no project-scoped view, so another mode
				// 404s rather than being answered under a scope that does not
				// apply.
				r.With(requireRuntimePlugins).
					Get("/identity_providers/administration", adminHandler.IdentityProviderList)
				r.With(requireRuntimePlugins).
					Put("/identity_providers/administration/{key}", adminHandler.IdentityProviderSave)
				r.With(requireRuntimePlugins).
					Delete("/identity_providers/administration/{key}", adminHandler.IdentityProviderDelete)
				// The TOOLKIT TYPE catalogue policy — `Admin › Toolkits`
				// (shared migration 0114). Five routes: the listing, one
				// decision, the two per-project exception verbs, and a bulk
				// apply.
				//
				// ITS OWN PERMISSION, and not `runtime.plugins` above. That
				// grant already reaches the guardrails deny-list editor, which
				// stops a toolkit type WORKING. This surface decides what the
				// product OFFERS, and to which projects, which is a different
				// authority: an operator trusted to edit a plugin's
				// configuration values is not automatically the operator who
				// decides which clients may build a Salesforce toolkit. The two
				// must be separately grantable, and a reuse cannot be undone
				// later without breaking every deployment that relied on it.
				// Shared migration 0114 grants the string, so the grant gate in
				// router_permission_grant_gate_test.go stays green.
				//
				// The mode segment is static `administration`: the catalogue
				// decision is a platform fact and there is no project-scoped
				// view of it. The per-project EXCEPTION is a path parameter on
				// the two grant routes, which is a different thing from a
				// project-scoped copy of the whole surface.
				//
				// `bulk` is a POST on a STATIC segment, and chi's trie prefers
				// it over `{type}`. It is a POST rather than a second PUT
				// because it is not idempotent in the useful sense: it reports
				// which types landed, and a partial result is a real outcome
				// this surface returns rather than hides.
				requireToolkitCatalogue := central(admin.ToolkitTypeManagePermission)
				r.With(requireToolkitCatalogue).
					Get("/toolkit_types/administration", adminHandler.ToolkitTypeList)
				r.With(requireToolkitCatalogue).
					Post("/toolkit_types/administration/bulk", adminHandler.ToolkitTypeBulk)
				r.With(requireToolkitCatalogue).
					Put("/toolkit_types/administration/{type}", adminHandler.ToolkitTypeSave)
				r.With(requireToolkitCatalogue).
					Put("/toolkit_types/administration/{type}/projects/{projectID}",
						adminHandler.ToolkitTypeGrantSave)
				r.With(requireToolkitCatalogue).
					Delete("/toolkit_types/administration/{type}/projects/{projectID}",
						adminHandler.ToolkitTypeGrantDelete)
				// The brand pack's database layer (ADR-0024 decision 5). Its own
				// permission, granted by shared migration 0109: a rebrand
				// changes what every user sees, and `runtime.plugins` is
				// already the widest configuration grant on this page.
				requireBranding := central("configuration.branding")
				r.With(requireBranding).Get("/branding/administration", adminHandler.BrandingRead)
				r.With(requireBranding).Put("/branding/administration", adminHandler.BrandingSave)
				r.With(requireBranding).Post("/branding/assets/{kind}", adminHandler.BrandingAssetUpload)
				r.With(requireBranding).Post("/branding/test_email/administration", adminHandler.BrandingTestEmail)
				// OUTBOUND E-MAIL — the real surface behind the Configuration
				// page's "E-mail" section, which stays unavailable because its
				// rows are plaintext and the SMTP password is a credential
				// (config_schemas.go).
				//
				// The permission is the SAME `runtime.plugins` that section
				// already required, so an operator who could edit it can
				// configure the relay, and no new permission string arrives
				// without a grant (router_permission_grant_gate_test.go).
				//
				// The mode segment is static `administration`: a mail relay is
				// a deployment fact with no project-scoped view, so another
				// mode 404s rather than being answered under a scope that does
				// not apply.
				//
				// The test route is a SECOND test-mail route beside the
				// branding one above, deliberately: they differ in permission,
				// not in behaviour, and an operator who may configure the relay
				// must be able to verify it without also holding the grant that
				// lets them re-brand the product.
				r.With(requireRuntimePlugins).Get("/email/administration", adminHandler.EmailSettingsRead)
				r.With(requireRuntimePlugins).Put("/email/administration", adminHandler.EmailSettingsSave)
				r.With(requireRuntimePlugins).Post("/email/test/administration", adminHandler.EmailTestSend)
				// The branding package (ADR-0024 decision 9): export, import
				// with a dry run, and the kept versions for rollback.
				r.With(requireBranding).Get("/branding/package/administration", adminHandler.BrandingPackageExport)
				r.With(requireBranding).Post("/branding/package/administration", adminHandler.BrandingPackageImport)
				r.With(requireBranding).Get("/branding/package/administration/versions", adminHandler.BrandingPackageVersions)
				r.With(requireBranding).Post("/branding/package/administration/versions/{digest}/restore", adminHandler.BrandingPackageRestore)
				r.With(requireRuntimePlugins).Get("/plugin_config_suggestions/{mode}/{key}", adminHandler.PluginConfigSuggestions)
				r.With(requireRuntimePlugins).Post("/plugin_config_restart/{mode}/{pylonID}", adminHandler.PluginConfigRestart)
				// `/moderation_statuses/…` is NOT registered here. #209 gated the
				// stub that used to sit on this line; this unit replaced the stub
				// with a real handler, registered below on a static
				// `administration` segment. Re-adding a `{mode}` route here would
				// resurrect the stub for every other mode — and since pylon reaches
				// this surface only through its `administration` AdminAPI, the only
				// thing that would answer is a constant nobody asked for.
				r.With(requireRuntimePlugins).Get("/maintenance/{mode}", adminHandler.Maintenance)
				r.With(requireRuntimePlugins).Put("/maintenance/{mode}", adminHandler.Maintenance)
				r.With(requireRuntimePlugins).Get("/runtime_remote/{mode}", adminHandler.RuntimeRemote)
				r.With(requireRuntimePlugins).Get("/runtime_remote_config/{mode}/{pluginID}", adminHandler.RuntimeRemoteConfig)
				r.With(requireRuntimePlugins).Post("/runtime_remote_config/{mode}/{pluginID}", adminHandler.RuntimeRemoteConfig)
				r.With(requireRuntimePlugins).Get("/runtime_plugin/{mode}/{pluginName}", adminHandler.RuntimePlugin)
				r.With(requireRuntimePlugins).Put("/runtime_plugin/{mode}/{pluginName}", adminHandler.RuntimePlugin)
				r.With(requireRuntimePlugins).Post("/runtime_pylons/{mode}", adminHandler.RuntimePylonLogs)
				r.With(requireRuntimePlugins).Get("/tasks/{mode}/", adminHandler.Tasks)
				r.With(requireRuntimePlugins).Get("/tasks/{mode}", adminHandler.Tasks)
				r.With(requireRuntimePlugins).Get("/active_tasks/{mode}", adminHandler.ActiveTasks)
				// Admin › Tasks — the PLATFORM's own background jobs, not
				// pylon's Arbiter task node. The two routes above keep
				// answering 501 (arbiterTaskNodeUnavailable); these two answer
				// the question that page asked, from this platform's tables.
				//
				// Same `runtime.plugins` gate in administration mode, because
				// that is what the legacy Tasks page declares
				// (legacy/plugins/admin/api/v2/tasks.py:43). No new permission
				// string arrives, so the grant gate stays untripped and every
				// operator who could open the legacy page can open this one.
				//
				// A STATIC `administration` segment, not `{mode}`: pylon reaches
				// this surface only through its administration AdminAPI, so a
				// mode variable would name a resolution this router does not
				// perform — the same argument the moderation routes below make.
				r.With(requireRuntimePlugins).Get("/background_jobs/administration", adminHandler.BackgroundJobs)
				r.With(requireRuntimePlugins).Post(
					"/background_jobs/administration/{kind}/{jobID}:cancel",
					adminHandler.CancelBackgroundJob,
				)

				// Regular app admin endpoints (with projectID)
				//
				// #130: POST/PUT/DELETE used to be mounted onto coreHandler.Users
				// as well. That handler does no method branching — every verb ran
				// the listing SELECT and answered 200 with the member list — so
				// Invite / Edit role / Remove all reported success and wrote
				// nothing. They now reach real handlers, each gated on the same
				// pylon permission its Python counterpart declares
				// (legacy/plugins/admin/api/v2/users.py).
				// The member LISTING was the last write-free route in this block
				// with no gate at all, and it is the one that discloses the most:
				// coreHandler.Users joins auth_core__project_user_role for the
				// named project and answers with every member's EMAIL, name and
				// roles. Any authenticated principal, a PAT holder included,
				// could enumerate any project's membership by editing the
				// {projectID} segment. Its own three writes beside it have been
				// gated since #130.
				//
				// pylon declares `configuration.users.users.view` on the same
				// handler (legacy/plugins/admin/api/v2/users.py, API.get), which
				// is the string the /elitea_core fallback copy of this route
				// already carries.
				//
				// TWO REGISTRATIONS, BECAUSE THE MODE DIFFERS — the same split
				// the create and edit writes below needed, for the same reason.
				// Read them together.
				//
				//   `{mode}` resolves in DEFAULT mode. It serves the project
				//   settings Members page, which the web app calls as
				//   `/admin/users/default/{projectID}`. legacyrbac answers a
				//   default-mode gate purely from the caller's membership OF
				//   THAT PROJECT, which is what this page means.
				//
				//   `administration` is a STATIC segment resolved CENTRALLY. It
				//   serves the admin panel's project member dialog and activity
				//   drawer, which call `/admin/users/administration/{projectID}`
				//   (apps/elitea-web/src/pages/admin/api/adminProjectsApi.ts).
				//   An operator acting on projects they are not a member of
				//   scores zero in default mode, so the default-mode gate alone
				//   would answer 403 to every legitimate caller of that dialog.
				//
				// The static registration is REQUIRED, not decorative: chi falls
				// through to the `{mode}` route for a method the static node does
				// not carry, so without a static GET here the admin panel's read
				// would land on the default-mode gate and break.
				//
				// migrations/shared/0085 grants the administration-mode holders.
				// The default-mode ones are already 0068's.
				//
				// The two default-mode reads take `projectPermission`, not the
				// inline RequireResolvedPermissions the three writes below use.
				// The two are the same middleware over the same resolver in
				// production — cfg.ProjectPermissionResolver is unset there —
				// but the helper is the one a route test can inject, and a gate
				// with no injectable resolver can be proved only in the refusing
				// direction. Without the admitting direction, a gate that
				// refuses EVERY caller reads as a pass.
				r.With(projectPermission("configuration.users.users.view")).
					Get("/users/{mode}/{projectID}", coreHandler.Users)
				r.With(central("configuration.users.users.view")).
					Get("/users/administration/{projectID}", coreHandler.Users)
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"configuration.users.users.create",
				)).Post("/users/{mode}/{projectID}", coreHandler.UsersCreate)
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"configuration.users.users.edit",
				)).Put("/users/{mode}/{projectID}", coreHandler.UsersUpdate)
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"configuration.users.users.delete",
				)).Delete("/users/{mode}/{projectID}", coreHandler.UsersDelete)
				// The same invite/edit-role writes in ADMINISTRATION mode — what
				// the admin Projects page's "Manage project member" dialog calls
				// (unit A14). Registered as STATIC segments so chi's trie prefers
				// them over the `{mode}` routes above, leaving those untouched.
				//
				// They are separate registrations because the GATE differs, not
				// the handler. The `{mode}` routes resolve
				// `configuration.users.users.*` in DEFAULT mode, which
				// `legacyrbac.PostgresResolver` answers purely from the caller's
				// membership OF THAT PROJECT — so a global administrator who is
				// not a member of the project scores zero permissions and is
				// refused, and the admin panel's whole purpose is acting on
				// projects one is not in. pylon gates the same handler on the
				// same permission resolved in administration mode
				// (legacy/plugins/admin/api/v2/users.py maps BOTH modes to the
				// same body, and its `recommended_roles` names `administration`).
				//
				// THEY TAKE THE `Administration…` HANDLERS, not the `{mode}`
				// ones. A static segment binds no URL parameter, so the shared
				// handler's `chi.URLParam(r, "mode")` was empty on every one of
				// these requests and the dialog's submit answered
				// `404 {"error":"unknown mode"}`. The GET above never showed it,
				// because Handler.Users reads no mode. See the header of
				// internal/api/v2/eliteacore/users_write.go.
				r.With(apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					"configuration.users.users.create",
				)).Post("/users/administration/{projectID}", coreHandler.AdministrationUsersCreate)
				r.With(apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					"configuration.users.users.edit",
				)).Put("/users/administration/{projectID}", coreHandler.AdministrationUsersUpdate)
				// The project ROLE listing, gated the same way and for the same
				// reason as the member listing above: `coreHandler.Roles` reads
				// auth_core__project_role for the named project, pylon declares
				// `configuration.roles.roles.view` on the same handler
				// (legacy/plugins/admin/api/v2/roles.py, ProjectAPI.get), and
				// two callers reach it in two modes — the project settings page
				// as `/admin/roles/default/{projectID}` and the admin panel's
				// member dialog as `/admin/roles/administration/{projectID}`
				// (apps/elitea-web/src/pages/admin/ProjectMemberDialog.tsx).
				//
				// The two role listings do NOT get the same holders. The legacy
				// matrix gives the administration-mode viewer
				// `configuration.roles.roles.view` and withholds
				// `configuration.users.users.view` from that same role. 0085
				// transcribes that difference rather than levelling it.
				r.With(projectPermission("configuration.roles.roles.view")).
					Get("/roles/{mode}/{projectID}", coreHandler.Roles)
				r.With(central("configuration.roles.roles.view")).
					Get("/roles/administration/{projectID}", coreHandler.Roles)

				// Role DEFINITIONS — create, rename, delete (gap G9).
				//
				// The two GETs above LIST roles. Nothing could add one, rename
				// one, or take one away, while pylon's
				// legacy/plugins/admin/api/v2/roles.py is full CRUD. A
				// deployment that wanted a `security_reviewer` role had to
				// INSERT it into auth_core__role by hand.
				//
				// THE SECOND SEGMENT IS NOT A PROJECT ID HERE. These three take
				// `{scope}/{mode}` — the shape /admin/permissions already uses,
				// where the first segment picks the matrix (administration,
				// public, support) and the second the target mode. That is the
				// pair the admin Roles page's four tabs are keyed on, and the
				// role columns those tabs render come from exactly that pair.
				//
				// chi keeps the two shapes apart: `{mode}` and `{scope}` are
				// separate param nodes, and the static `administration` node
				// the GET above registers carries no POST/PUT/DELETE, so those
				// fall through to `{scope}`.
				// TestAdminRoleWriteRoutesResolve pins that resolution, because
				// it is a property of chi's trie rather than of this file.
				//
				// Gated on the three permissions roles.py declares, resolved in
				// ADMINISTRATION mode like every other admin-panel gate above.
				// They are distinct from `configuration.roles.permissions.*`:
				// moving a checkbox in the matrix and deleting the role that
				// checkbox belongs to are different privileges, and pylon
				// separates them too.
				//
				// migrations/shared/0111 grants them. No migration did before,
				// so all three would have answered 403 to every caller on a
				// clean database.
				r.With(central(admin.RolesCreatePermission)).
					Post("/roles/{scope}/{mode}", adminHandler.AdminRoleCreate)
				r.With(central(admin.RolesEditPermission)).
					Put("/roles/{scope}/{mode}", adminHandler.AdminRoleRename)
				r.With(central(admin.RolesDeletePermission)).
					Delete("/roles/{scope}/{mode}", adminHandler.AdminRoleDelete)

				// App requests / moderation (unit A14). Four routes, of which
				// three did not exist and the fourth answered from a constant:
				// `moderation_status/{mode}/{projectID}/{entityID}` returned
				// `{"status":"approved"}` to every caller for every entity while
				// its POST created nothing, so the catalogue's "Request Access"
				// button wrote to nowhere and the gate it feeds always said yes.
				// See internal/api/v2/moderation/requests.go.
				//
				// Gated on the permissions the pylon handlers declare
				// (legacy/plugins/admin/api/v2/moderation_status*.py). The split
				// between CENTRAL and RESOLVED is the same one the schedules and
				// admin-user writes needed: an operator answering requests that
				// arrive from every tenant is a member of none of those
				// projects, so a project-scoped resolver would refuse every
				// legitimate moderator — while the two project-scoped routes are
				// exactly as project-scoped as they look.
				//
				// `administration` is a STATIC segment on the queue and the
				// decision, so neither binds a `{mode}` param and both handlers
				// state their mode by existing rather than sniffing the URL.
				// `requireModeration` is #209's `central("admin.moderation")` —
				// the same middleware that gated the stub this route replaces.
				r.With(requireModeration).
					Get("/moderation_statuses/administration", moderationHandler.AdministrationRequests)
				r.With(apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					"admin.moderation.edit",
				)).Put("/moderation_status/administration", moderationHandler.AdministrationRequestUpdate)
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"admin.moderation.view",
				)).Get("/moderation_status/{mode}/{projectID}/{entityID}", moderationHandler.Requests)
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"admin.moderation.create",
				)).Post("/moderation_status/{mode}/{projectID}/{entityID}", moderationHandler.RequestCreate)
				// Withdraw (#544). The same permission as the POST, on purpose:
				// a person who may file a request may take it back, so no
				// deployment needs a new grant. The handler scopes the delete to
				// the CALLER's own rows in SQL, so an `administration` value on
				// `{mode}` does not let a moderator erase what they answered.
				r.With(apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					"admin.moderation.create",
				)).Delete("/moderation_status/{mode}/{projectID}/{entityID}", moderationHandler.RequestDelete)

				// Preserve current-main gateway administration. Server-side
				// permission enforcement is required even when the UI hides
				// these controls.
				//
				// This gate used `apimw.RequirePermissions`, which reads
				// `auth.User.Permissions` instead of asking the resolver.
				// Production never fills that field. The only source that
				// assigned it was the legacy Redis-RPC validator that #383
				// deleted, and production wires
				// `authsvc.NewPrincipalValidator` instead, which leaves the
				// field nil. So the gate refused EVERY caller by construction,
				// the operator included, and no migration could reach it. That
				// is #386.
				//
				// `RequireCentralPermissions` in the `administration` mode is
				// what every neighbour in this block uses, and it is the right
				// shape here: the path carries no `{projectID}`, so the surface
				// is platform-wide rather than project-scoped. The permission
				// string does not change.
				// shared/0082_admin_panel_permissions.sql grants it.
				// The store is built over the shared pool because the config it
				// holds is global and durable (issue #322). It used to be a
				// process-local struct, which made a PUT a per-replica opinion
				// that a restart discarded.
				if cfg.BudgetAlertStore == nil {
					cfg.BudgetAlertStore = gateway.NewBudgetAlertStore(cfg.Pool)
				}
				budgetAlertHandler := gateway.NewBudgetAlertHandler(cfg.BudgetAlertStore)
				governanceHandler := gateway.NewGovernanceHandler(cfg.Pool)
				// The LLM Proxy surface: the gateway's enforcement status and
				// the model price catalogue, joined to the usage that makes the
				// catalogue actionable. It shares this group's permission
				// deliberately — it explains and prices the same enforcement
				// the governance CRUD beside it authors, and a second
				// permission string would need its own grant migration to reach
				// anybody (the #386 shape, one block up).
				//
				// `cfg.GatewayStatus` may be nil. A deployment with no gateway
				// address is supported, and the handler reports that posture
				// rather than the composition refusing to build — but it is
				// passed as a typed field rather than assigned into the
				// interface here, because boxing a nil *GatewayStatusClient
				// would make the handler's `status == nil` check false and turn
				// "not configured" into a nil-receiver call.
				//
				// The pool is passed only when it is non-nil, for the same
				// reason: `llmProxyQuerier` is an interface, and a nil
				// *pgxpool.Pool boxed into it is NOT nil, so the handler's
				// "no database pool" branches would never run and the first
				// read would panic on a nil receiver instead.
				var llmProxyDB gateway.LLMProxyQuerier
				if cfg.Pool != nil {
					llmProxyDB = cfg.Pool
				}
				llmProxyHandler := gateway.NewLLMProxyHandler(llmProxyDB, cfg.GatewayStatus)
				r.Group(func(r chi.Router) {
					r.Use(central("configuration.governance"))
					r.Route("/gateway", func(r chi.Router) {
						r.Mount("/", budgetAlertHandler.Routes())
						governanceHandler.Register(r)
						llmProxyHandler.Register(r)
						// PLATFORM-WIDE provider credentials — the public
						// project's shared `ai_credentials` rows, which the LLM
						// gateway already resolves for every project (issue
						// #316). See global_providers.go for why this is the
						// same rows and the same handlers rather than a second
						// credential model.
						//
						// It inherits this group's `configuration.governance`
						// gate from the enclosing Group, which is correct for a
						// surface whose four verbs take one permission — and is
						// why it can be a Mount at all, unlike the project-scoped
						// Routes() that needs five different strings.
						r.Mount("/providers", configurationsHandler.GlobalProviderRoutes())
						// PLATFORM-WIDE models — the other half. A credential
						// authenticates; a model is what a caller addresses.
						// Both are the public project's shared rows, and the
						// gateway resolves a public model against public
						// credentials ONLY, so the two surfaces are mounted
						// together and global_models.go enforces that pairing.
						r.Mount("/platform_models", configurationsHandler.GlobalModelRoutes())
					})
				})
			})

			// === Scheduling ===
			//
			// The platform cron table (unit A14). The PUT had no route at all
			// until this unit, so the admin Schedules tab's active switch and
			// inline cron editor had nothing behind them; the GET had a route
			// but read pipeline-trigger metadata out of the tenant schema and
			// short-circuited on the `projectID=0` the admin page sends. See
			// internal/api/v2/scheduling/schedules.go.
			//
			// Gated on the permissions pylon declares for the same handlers
			// (legacy/plugins/scheduling/api/v2/schedules.py):
			// `configuration.scheduling.schedules.view` and `…edit`.
			//
			// Registered as two pairs because the GATE differs, not the
			// handler — the same split the admin user writes needed above.
			// `legacyrbac.PostgresResolver` answers the `{mode}` routes purely
			// from the caller's membership OF THAT PROJECT, and an operator
			// disabling a PLATFORM job (`project_id IS NULL`) is a member of no
			// project at all, so a project-scoped resolver would refuse every
			// legitimate admin caller. The `administration` segment is STATIC so
			// chi's trie prefers it, leaving the project routes untouched.
			schedulingHandler := v2scheduling.NewHandler(cfg.Pool)
			r.With(apimw.RequireCentralPermissions(
				permissionResolver, platformauth.PermissionModeAdministration,
				"configuration.scheduling.schedules.view",
			)).Get("/scheduling/schedules/administration/{projectID}", schedulingHandler.AdministrationSchedules)
			r.With(apimw.RequireCentralPermissions(
				permissionResolver, platformauth.PermissionModeAdministration,
				"configuration.scheduling.schedules.edit",
			)).Put("/scheduling/schedules/administration/{projectID}", schedulingHandler.AdministrationSchedulesUpdate)
			r.With(apimw.RequireResolvedPermissions(
				permissionResolver, platformauth.PermissionModeDefault,
				"configuration.scheduling.schedules.view",
			)).Get("/scheduling/schedules/{mode}/{projectID}", schedulingHandler.Schedules)
			r.With(apimw.RequireResolvedPermissions(
				permissionResolver, platformauth.PermissionModeDefault,
				"configuration.scheduling.schedules.edit",
			)).Put("/scheduling/schedules/{mode}/{projectID}", schedulingHandler.SchedulesUpdate)

			// === Configurations ===
			//
			// The per-project CREDENTIAL surface: 22 routes that derive the
			// tenant schema from `{projectID}` and then read, write or delete
			// that project's `configuration` rows. Until #496 the mounted
			// subrouter applied NO gate — see the Routes() header in
			// internal/api/v2/configurations/handler.go for what that let any
			// authenticated caller do to any project, and for the permission
			// each route now takes.
			//
			// `configurationsHandler` is built at the top of this function and
			// mounted twice — see its declaration for why one construction is
			// load-bearing rather than tidy.
			//
			// The gates live inside the package, not on this mount, for the two
			// reasons /secrets gives: the routes need five different strings, and
			// a mount carries one middleware. `coreResolver` is used rather than
			// `permissionResolver` so the route tests can substitute a resolver
			// through cfg.ProjectPermissionResolver, exactly as the sibling
			// groups below do; production leaves that field unset and both names
			// are the same object.
			r.Mount("/configurations", configurationsHandler.Routes())

			// === Secrets ===
			// Mounted under "/secrets" — the pylon PLUGIN name — while the
			// subrouter's own three prefixes are the plugin's RESOURCE
			// MODULES (secrets.py / secret.py / hide.py). The doubled
			// "secrets" in /api/v2/secrets/secrets/{mode}/{projectID} is the
			// real legacy shape, shared by apps/elitea-ui, elitea-sdk,
			// admin_ui and qa/elitea-api-testing. #137 read it as a
			// double-mount bug and moved the routes to the v2 root, which
			// broke every consumer outside apps/elitea-web; #151 restores it.
			// The resolver gates BOTH mode families: the `administration`
			// routes over the GLOBAL vault (unit A14) and the `default`
			// project routes, which until then had no gate at all — any
			// authenticated caller could name any {projectID} and read that
			// project's secret VALUES in plaintext. Both gates live inside
			// the package because the mode is a PATH SEGMENT: one chi route
			// serves both modes, so route-level middleware here could not
			// gate one and not the other.
			r.Mount("/secrets", v2secrets.NewHandler(
				cfg.Pool,
				v2secrets.WithPermissionResolver(permissionResolver),
			).Routes())

			// === Notifications ===
			//
			// Every route here names {projectID} and then reads or writes
			// centry.notifications for that project. They shipped with NO gate
			// of any kind: not a permission, not a membership check.
			//
			// The permissions are transcribed from the pylon notifications
			// plugin, which is the same source the REVIEWED copy of this surface
			// uses — internal/api/v2/notifications/api.go gates the identical
			// paths on the identical four strings. That copy is registered by
			// production_router.go only when cfg.CurrentNotifications is
			// composed, and chi prefers its static path over this group's mount,
			// so it SHADOWS these five registrations wherever it exists. Where it
			// does not exist, this group is what answers, and until now it
			// answered ungated. Two registrations of one surface must not carry
			// two different authorization contracts.
			//
			//   notifications.py GET    → models.notifications.notifications.list
			//   notifications.py PUT    → models.notifications.notification.update
			//   notifications.py DELETE → models.notifications.notification.delete
			//   notification.py  PUT    → models.notifications.notification.update
			//   notification.py  DELETE → models.notifications.notification.delete
			//
			// migrations/shared/0079_notification_permissions.sql grants all
			// four in DEFAULT mode, with the legacy matrix split: the list and
			// the delete go to admin, editor and viewer, and the update goes to
			// admin and editor alone.
			requireNotificationUpdate := projectPermission("models.notifications.notification.update")
			requireNotificationDelete := projectPermission("models.notifications.notification.delete")
			r.Route("/notifications", func(r chi.Router) {
				r.With(projectPermission("models.notifications.notifications.list")).
					Get("/notifications/prompt_lib/{projectID}", coreHandler.Notifications)
				r.With(requireNotificationUpdate).
					Put("/notification/prompt_lib/{projectID}/{notificationID}", coreHandler.UpdateNotification)
				r.With(requireNotificationDelete).
					Delete("/notification/prompt_lib/{projectID}/{notificationID}", coreHandler.UpdateNotification)
				r.With(requireNotificationDelete).
					Delete("/notifications/prompt_lib/{projectID}", coreHandler.UpdateNotification)
				r.With(requireNotificationUpdate).
					Put("/notifications/prompt_lib/{projectID}", coreHandler.UpdateNotification)
			})

			// === elitea_core plugin routes ===
			r.Route("/elitea_core", func(r chi.Router) {
				// projectScoped closes the cross-project hole measured in #302.
				// 116 of this group's registrations named {projectID} in the
				// path and ran on the group's Auth middleware alone. Every
				// handler behind them derives its tenant schema straight from
				// that path parameter — v2apps.Handler.Delete calls
				// tenantSchema(projectID), eliteacore's Unpublish builds
				// fmt.Sprintf("p_%s", projectID) — and none of them reads the
				// caller's membership. Any authenticated principal, including
				// any PAT holder, could therefore read and mutate any project
				// by editing one path segment.
				//
				// MEMBERSHIP was all the first pass enforced, deliberately:
				// the legacy permission NAMES were recoverable
				// (testdata/legacy/legacy-rbac-static-catalog.json) but nothing
				// granted them, and 0063's header states what that costs —
				// "gating a route on a permission nothing grants is
				// 403-for-everyone, which reads as a broken page rather than as
				// a missing grant". #313 closed that: migrations/shared/
				// 0068_elitea_core_route_permissions.sql seeds the default-mode
				// grants, transcribed per role from the exported legacy matrix,
				// so `projectPermission` below can be used at all. The two land
				// together on purpose; either alone is a defect.
				//
				// `projectScoped` therefore survives only where legacy has NO
				// permission to copy — see each remaining use for which one and
				// why. Everywhere else the named permission REPLACES it, which
				// is strictly stronger: legacyrbac's projectPermissions() joins
				// the central default-mode fallback THROUGH the caller's
				// assigned project roles, so a non-member resolves the empty
				// set and is refused before any permission is compared.
				//
				// One behaviour does narrow. RequireProjectAccess admits a
				// central `super_admin` who holds no role in the named project;
				// the resolver does not, in default mode, and never has — the
				// secrets, budgets, cost, trace, chat-config and index_meta
				// gates already in this file all behave that way. A super_admin
				// who needs a project's data joins the project, as they must
				// today for every one of those.
				//
				// Both are resolved through locals, exactly as artifactResolver
				// is above, and for a reason beyond style:
				// TestNilGatedRouterFieldsAreWiredOrDeclared scans router.go
				// for nil-comparisons against a config field and demands every
				// such field be wired in main.go or allowlisted (note: it
				// scans TEXT, so naming the pattern literally in a comment
				// trips it), because those gates normally decide
				// whether routes get REGISTERED. These do not — the same
				// routes are registered either way, only the authorization
				// decision behind them changes — so writing them as cfg gates
				// would file a false "silently unregistered, answers 404"
				// report against fields production is meant to leave unset.
				projectAccessQuerier := cfg.ProjectAccessQuerier
				projectScoped := apimw.RequireProjectAccess(cfg.Pool)
				if projectAccessQuerier != nil {
					projectScoped = apimw.RequireProjectAccessWith(projectAccessQuerier)
				}

				// `projectPermission` gates one route on the named legacy
				// permission, resolved in DEFAULT mode against the {projectID}
				// path segment. It is declared above the /notifications group,
				// because /notifications and /context_manager need the same
				// helper and are siblings of this group rather than children.
				//
				// Every name passed to it is transcribed from the pylon
				// `check_api` declaration of the matching elitea_core/api/v2/*.py
				// module and verb; router_elitea_core_permission_map_test.go
				// re-derives both directions from
				// testdata/legacy/legacy-rbac-static-catalog.json and from 0068,
				// so a name that legacy never declared — or one that nothing
				// grants — fails the build instead of shipping as a 403 nobody
				// can clear.

				// Applications
				if cfg.AppsRepo != nil {
					appHandler := v2apps.NewHandler(cfg.AppsRepo, cfg.Pool)
					r.With(projectPermission("models.applications.applications.list")).
						Get("/applications/prompt_lib/{projectID}", appHandler.List)
					r.With(projectPermission("models.applications.applications.create")).
						Post("/applications/prompt_lib/{projectID}", appHandler.Create)
					r.With(projectPermission("models.applications.application.details")).
						Get("/application/prompt_lib/{projectID}/{applicationID}", appHandler.Get)
					r.With(projectPermission("models.applications.application.update")).
						Put("/application/prompt_lib/{projectID}/{applicationID}", appHandler.Update)
					r.With(projectPermission("models.applications.application.delete")).
						Delete("/application/prompt_lib/{projectID}/{applicationID}", appHandler.Delete)
					r.With(projectPermission("models.applications.versions.get")).
						Get("/versions/prompt_lib/{projectID}/{applicationID}", appHandler.ListVersions)
					r.With(projectPermission("models.applications.versions.create")).
						Post("/versions/prompt_lib/{projectID}/{applicationID}", appHandler.CreateVersion)
					r.With(projectPermission("models.applications.version.details")).
						Get("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.GetVersion)
					r.With(projectPermission("models.applications.version.update")).
						Put("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.UpdateVersion)
					r.With(projectPermission("models.applications.version.delete")).
						Delete("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.DeleteVersion)
					// PATCH on this path is a READ, not a partial update (#336).
					// It serves the SDK's `get_app_version_details`, which sends
					// a body-less PATCH and expects the expanded, secret-resolved
					// version details.
					//
					// Pylon declares the READ permission
					// `models.applications.version.details` on its own handler
					// (legacy/plugins/elitea_core/api/v2/version.py:100-106), so
					// this route takes the same permission as the GET above it —
					// NOT `version.update`, which would refuse the viewer that
					// pylon admits.
					r.With(projectPermission("models.applications.version.details")).
						Patch("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.GetVersionExpanded)
					// pylon's default_version.py declares only `patch`; the GET
					// beside it has no legacy verb to copy, so it takes the READ
					// permission of the resource it returns — a version — rather
					// than a name invented for it.
					r.With(projectPermission("models.applications.version.details")).
						Get("/default_version/prompt_lib/{projectID}/{applicationID}", appHandler.GetDefaultVersion)
					r.With(projectPermission("models.applications.version.update")).
						Patch("/default_version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.SetDefaultVersion)
				}

				// Agent categories — deliberately NOT project-scoped.
				//
				// This is a global taxonomy, not project data: the handler
				// returns nine hardcoded defaults merged with a globally
				// authored extras row, and its own comment records that the
				// per-project `publishing_guardrail` read "could only ever
				// miss" because no surface writes one. So a project gate here
				// protects nothing.
				//
				// It also breaks a cross-project surface. The Agent HUB renders
				// this list beside the PUBLIC published-agent catalogue, which
				// is itself ungated below; gating the categories emptied the
				// hub's rail while the catalogue beside it kept working. Caught
				// by the @visual agents-hub snapshot, whose oracle is a category
				// name — no unit test covers a route that answers correctly for
				// a member and is simply unreachable from a public page.
				r.Get("/agent_categories/prompt_lib/{projectID}", coreHandler.AgentCategories)

				// Skills (UI calls /skill/ and /skills/ paths)
				if cfg.SkillsRepo != nil {
					skillHandler := v2skills.NewHandler(cfg.SkillsRepo)
					requireSkillCreate := projectPermission("models.applications.skills.create")
					requireSkillUpdate := projectPermission("models.applications.skills.update")
					requireSkillExport := projectPermission("models.applications.skills.export")
					r.With(projectPermission("models.applications.skills.list")).
						Get("/skills/{mode}/{projectID}", skillHandler.List)
					r.With(requireSkillCreate).Post("/skills/{mode}/{projectID}", skillHandler.Create)
					r.With(projectPermission("models.applications.skills.details")).
						Get("/skill/{mode}/{projectID}/{skillID}", skillHandler.Get)
					r.With(requireSkillCreate).Post("/skill/{mode}/{projectID}/{skillID}", skillHandler.Create)
					r.With(requireSkillUpdate).Put("/skill/{mode}/{projectID}/{skillID}", skillHandler.Update)
					r.With(requireSkillUpdate).Patch("/skill/{mode}/{projectID}/{skillID}", skillHandler.Update)
					r.With(projectPermission("models.applications.skills.delete")).
						Delete("/skill/{mode}/{projectID}/{skillID}", skillHandler.Delete)
					r.With(requireSkillUpdate).
						Patch("/skill_default_version/{mode}/{projectID}/{skillID}", skillHandler.Update)
					// NOTE(#395): GET
					// /application_skills/{mode}/{projectID}/{appVersionID}
					// stood here, on skillHandler.ListForApplication. It was
					// the PROTOTYPE fallback for the attached-skills read.
					//
					// It is deleted, not moved. The reviewed route,
					// internal/api/v2/applicationskills, is the only handler
					// for this path now; production_router.go registers it at
					// the exact contract path, and the chart and the compose
					// stacks turn it on wherever the install authenticates.
					// Two handlers for one path were never a fallback pair:
					// chi resolved the explicitly registered path first, so
					// this mount only ever answered where the reviewed route
					// was absent, and there it answered a different envelope
					// from a different read.
					//
					// The published contract fixes the mode segment to
					// `prompt_lib` (getListApplicationSkillsUrl in
					// apps/elitea-web/src/shared/api/generated/skills/skills.ts),
					// and projectPermission resolved PermissionModeDefault for
					// every value of {mode}, so the deleted wildcard carried no
					// authorization meaning and no client used another mode.
					r.With(requireSkillCreate).Post("/skill_import/{mode}/{projectID}", skillHandler.Import)
					r.With(requireSkillExport).Get("/skill_export/{mode}/{projectID}/{skillID}", skillHandler.Export)
					r.With(requireSkillExport).
						Get("/skill_export/{mode}/{projectID}/{skillID}/{versionID}", skillHandler.Export)
				}

				// Toolkits
				// The guardrails source is constructed here rather than injected
				// through RouterConfig: it needs only the pool this config
				// already carries, and internal/platformconfig is a leaf the api
				// layer already depends on (eliteacore reads its flags). A
				// deployment with no pool gets no source, and every toolkit
				// surface behaves as it did before guardrails existed — which is
				// the only honest answer when there is no store to read a policy
				// from.
				toolkitOptions := []v2toolkits.Option{
					v2toolkits.WithArgumentSchemas(cfg.ToolkitArgumentSchemas),
					v2toolkits.WithSettingsDefinitions(cfg.ToolkitSettingsDefinitions),
				}
				// Guarded for the reason the settings validator below is: an
				// Option that stored a typed nil would defeat the handler's own
				// nil check, and the nil check is the whole fallback.
				if cfg.ToolkitCatalogue != nil {
					toolkitOptions = append(toolkitOptions,
						v2toolkits.WithCatalogue(cfg.ToolkitCatalogue))
				}
				if cfg.ToolkitToolRun != nil {
					toolkitOptions = append(toolkitOptions,
						v2toolkits.WithToolRuns(cfg.ToolkitToolRun))
				}
				if cfg.ToolkitWorkerCapability != nil {
					toolkitOptions = append(toolkitOptions,
						v2toolkits.WithWorkerCapability(cfg.ToolkitWorkerCapability))
				}
				// Guarded rather than appended unconditionally: an Option that
				// stored a nil interface would still leave h.settingsValidator
				// nil, but a caller that later boxes a typed nil pointer here
				// would not, and the handler's own nil check is the whole
				// fallback. Keep the nil out of the option list.
				if cfg.ToolkitSettingsValidator != nil {
					toolkitOptions = append(toolkitOptions,
						v2toolkits.WithSettingsValidator(cfg.ToolkitSettingsValidator))
				}
				if guardrailPolicies, err := platformconfig.NewGuardrailPolicyAdapter(cfg.Pool); err == nil {
					toolkitOptions = append(toolkitOptions, v2toolkits.WithGuardrails(guardrailPolicies))
				}
				// The SAME store the admin page writes (shared migration 0114).
				// One store for the decision and for the served catalogue, so
				// the two cannot disagree about which types a project gets.
				toolkitOptions = append(toolkitOptions,
					v2toolkits.WithTypePolicy(toolkitTypePolicyStore))
				toolkitHandler := v2toolkits.NewHandler(cfg.Pool, toolkitOptions...)
				// /tool(s)/ and /toolkits/ paths route to toolkitHandler (toolkit instances, not skills).
				//
				// NOTE the split, which was wrong until #129: /tools/ is the
				// INSTANCE list (toolkitHandler.List) and /toolkits/ is the
				// TYPE catalogue (toolkitHandler.ListTypeSchemas — a map of
				// toolkit type name to its settings JSON Schema). That is what
				// api/openapi/v2.yaml specifies (listToolkits ->
				// ToolkitTypeSchemas, listToolkitInstances -> the array), what
				// the generated web client requests (apps/elitea-web/src/
				// shared/api/generated/toolkits/toolkits.ts:562 vs :764), and
				// what the legacy runtime served (legacy elitea_core
				// api/v2/toolkits.py -> get_toolkit_schemas, api/v2/tools.py ->
				// the instance list). Both /toolkits/ registrations previously
				// pointed at List, so ListTypeSchemas had no route at all and
				// the MCP create screen could never show a type.
				// Gate behind FEATURE_FLAG_TOOLKIT_PROJECT_ACCESS for gradual
				// rollout: when enabled, each route resolves the permission its
				// pylon module declares; setting the env var to "false"
				// registers the same routes with no gate at all — the legacy
				// vllm/bifrost compatibility path. The flag selects GATED
				// versus UNGATED, never registered versus not.
				//
				// One registration list serves both settings. It used to be
				// written out twice, once per branch, which is how twenty-two
				// routes ended up maintained in two places where only the ON
				// copy is ever read.
				//
				// The permissions replace the membership check this block
				// carried: pylon's tools.py/tool.py split `list`/`create` from
				// `details`/`update`/`patch`/`delete`, and PATCH is a different
				// string from PUT there, which is why `tool.patch` also gates
				// the index-START route and the MCP proxies below.
				toolkitGate := projectPermission
				if os.Getenv("FEATURE_FLAG_TOOLKIT_PROJECT_ACCESS") == "false" {
					toolkitGate = func(string) func(http.Handler) http.Handler {
						return func(next http.Handler) http.Handler { return next }
					}
				}
				requireToolDetails := toolkitGate("models.applications.tool.details")
				requireToolPatch := toolkitGate("models.applications.tool.patch")
				requireToolkitValidate := toolkitGate("models.applications.toolkit_validator.check")
				requireIndexMetaRead := toolkitGate("models.applications.index_meta.details")
				r.With(toolkitGate("models.applications.tools.list")).
					Get("/tools/prompt_lib/{projectID}", toolkitHandler.List)
				r.With(toolkitGate("models.applications.tools.create")).
					Post("/tools/prompt_lib/{projectID}", toolkitHandler.Create)
				r.With(requireToolDetails).
					Get("/tool/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.Get)
				r.With(toolkitGate("models.applications.tool.update")).
					Put("/tool/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.Update)
				r.With(requireToolPatch).
					Patch("/tool/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.Update)
				r.With(toolkitGate("models.applications.tool.delete")).
					Delete("/tool/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.Delete)
				r.With(toolkitGate("models.applications.toolkits.details")).
					Get("/toolkits/prompt_lib/{projectID}", toolkitHandler.ListTypeSchemas)
				r.With(toolkitGate("models.applications.tools.list")).
					Get("/toolkit_types/prompt_lib/{projectID}", toolkitHandler.ListTypes)
				r.With(requireToolDetails).
					Get("/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.AvailableTools)
				r.With(requireToolDetails).
					Post("/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}", toolkitHandler.DiscoverTools)
				// toolkit_validator.py declares only `get`; the POST beside it
				// runs the same check over a body and takes the same name.
				r.With(requireToolkitValidate).
					Get("/toolkit_validator/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.ValidateToolkit)
				r.With(requireToolkitValidate).
					Post("/toolkit_validator/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.ValidateToolkit)
				r.With(toolkitGate("models.applications.fork.post")).
					Post("/fork_toolkit/prompt_lib/{projectID}", toolkitHandler.ForkToolkit)
				// pylon has test_toolkit_tool.py and no test_tool.py; both
				// verbs here run one toolkit tool, so both take the string that
				// module declares.
				r.With(requireToolPatch).
					Post("/test_tool/prompt_lib/{projectID}/{toolID}", toolkitHandler.TestTool)
				r.With(requireToolPatch).
					Post("/test_toolkit_tool/prompt_lib/{projectID}", toolkitHandler.TestToolkitTool)
				r.With(toolkitGate("models.applications.export_toolkit.export")).
					Get("/export_toolkit/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.ExportToolkit)
				// NOTE(#394): GET /index_types/prompt_lib/{projectID} stood
				// here, on toolkitHandler.IndexTypes. It was the PROTOTYPE
				// fallback for the index-type catalogue: first a static
				// six-loader list that no data backs, then a 501 refusal.
				//
				// It is deleted. internal/api/v2/indextypes answers this path
				// from the snapshot pinned out of the SDK, production_router.go
				// registers it at the exact contract path, and the chart and
				// the compose stacks turn it on wherever the install
				// authenticates. A second registration of the same path is not
				// a fallback: chi resolved the explicit path first, so this
				// mount only ever answered where the reviewed route was absent.
				r.With(requireIndexMetaRead).
					Get("/index_meta/prompt_lib/{projectID}/{toolkitID}", toolkitHandler.IndexMeta)
				r.With(requireIndexMetaRead).
					Get("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", toolkitHandler.IndexMetaGet)
				r.With(toolkitGate("models.applications.index_meta.edit")).
					Patch("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", toolkitHandler.IndexMetaUpdate)
				r.With(toolkitGate("models.applications.index_meta.delete")).
					Delete("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", toolkitHandler.IndexMetaDelete)
				r.With(toolkitGate("models.applications.task.delete")).
					Delete("/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}", toolkitHandler.IndexCancel)

				// Folders
				if cfg.FoldersRepo != nil {
					// WithPool is what makes the grouped sidebar have any
					// content at all: every conversation the listing
					// groups is read through it, and without it the
					// endpoint answered 200 with empty folders and empty
					// date_groups for a project with nine conversations
					// (#128 defects 1 and 2).
					folderHandler := v2folders.NewHandler(cfg.FoldersRepo).WithPool(cfg.Pool)
					requireFolderRead := projectPermission("models.chat.folders.get")
					requireFolderUpdate := projectPermission("models.chat.folders.update")
					r.With(requireFolderRead).Get("/folder/prompt_lib/{projectID}", folderHandler.List)
					r.With(projectPermission("models.chat.folders.create")).
						Post("/folder/prompt_lib/{projectID}", folderHandler.Create)
					r.With(requireFolderRead).Get("/folder/prompt_lib/{projectID}/{folderID}", folderHandler.Get)
					r.With(requireFolderUpdate).Put("/folder/prompt_lib/{projectID}/{folderID}", folderHandler.Update)
					r.With(requireFolderUpdate).Patch("/folder/prompt_lib/{projectID}/{folderID}", folderHandler.Update)
					r.With(projectPermission("models.chat.folders.delete")).
						Delete("/folder/prompt_lib/{projectID}/{folderID}", folderHandler.Delete)
				}

				// Tags
				if cfg.TagsRepo != nil {
					tagHandler := v2tags.NewHandler(cfg.TagsRepo)
					r.With(projectPermission("models.promptlib_shared.tags.list")).
						Get("/tags/prompt_lib/{projectID}", tagHandler.List)
					// POST and DELETE stay at the MEMBERSHIP tier, alone in this
					// group. pylon's tags.py defines `get` and nothing else — no
					// legacy deployment has ever served a tag write on this
					// resource, so there is no `check_api` declaration to
					// transcribe. Naming one here would be a guess in one of two
					// directions with no way to tell which: reuse
					// `tags.list` and any viewer can delete a project's tags;
					// invent `models.promptlib_shared.tags.create` and nothing
					// grants it, so the button 403s for everyone. Both are worse
					// than the membership check, which is at least the
					// authorization these routes had a moment ago. #313 records
					// this as a product decision rather than a transcription.
					r.With(projectScoped).Post("/tags/prompt_lib/{projectID}", tagHandler.Create)
					r.With(projectScoped).Delete("/tags/prompt_lib/{projectID}/{tagID}", tagHandler.Delete)
				}

				// Agent Evaluation — the DIMENSION LIBRARY, and nothing else.
				//
				// The baseline UI spans 19 `eval_*` path families. Four routes
				// are registered here and the other 34 operations are NOT: no
				// suites, bindings, datasets, cases, runs, results, human
				// scores, platform catalogue or `generate_eval_dimensions`.
				// They are absent rather than stubbed, so they answer 404 —
				// a 200 with an empty body is indistinguishable, in a browser,
				// from a feature that works and has no data.
				//
				// WHY THESE FOUR GATES DO NOT USE `projectPermission`.
				//
				// They build the identical middleware — same resolver, same
				// DEFAULT mode — and differ only in that the permission comes
				// from a package constant rather than a string literal.
				// router_elitea_core_permission_map_test.go asserts that every
				// literal handed to `projectPermission` appears in
				// testdata/legacy/legacy-rbac-static-catalog.json, and that
				// assertion is exactly right for a PORTED pylon route: a name
				// the catalogue does not know is a typo, and a typo ships as a
				// permanent 403 nobody can clear. Agent Evaluation is not in
				// the pylon corpus this repository carries — there is no
				// evaluation module in legacy/plugins/elitea_core/api/v2/ — so
				// these four names have no `check_api` to be transcribed from.
				// Routing them through that helper would make a provenance
				// claim that is false; the names come from the product's own UI
				// constants instead (see the package doc in
				// internal/api/v2/evaluation).
				//
				// The check that MATTERS still binds:
				// router_permission_grant_gate_test.go resolves these constants
				// through the AST and fails unless a shared migration grants
				// each of them in `default` mode.
				// shared/0100_evaluation_dimension_permissions.sql is that
				// grant, and it lands with this registration rather than after
				// it — either alone is the #354/#359 defect.
				//
				// ONE gate constructor for every evaluation route, hoisted out
				// of the dimension block that first declared it: slice 2's
				// dataset and run routes are registered behind their own nil
				// checks, and a second copy of this closure would be a second
				// place for the resolver or the mode to drift.
				evaluationGate := func(permission string) func(http.Handler) http.Handler {
					return apimw.RequireResolvedPermissions(
						coreResolver, platformauth.PermissionModeDefault, permission)
				}
				if cfg.EvalDimensionsRepo != nil {
					evaluationHandler := v2evaluation.NewHandler(cfg.EvalDimensionsRepo)
					r.With(evaluationGate(v2evaluation.PermissionDimensionRead)).
						Get("/eval_dimensions/prompt_lib/{projectID}", evaluationHandler.List)
					r.With(evaluationGate(v2evaluation.PermissionDimensionCreate)).
						Post("/eval_dimensions/prompt_lib/{projectID}", evaluationHandler.Create)
					r.With(evaluationGate(v2evaluation.PermissionDimensionUpdate)).
						Put("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", evaluationHandler.Update)
					r.With(evaluationGate(v2evaluation.PermissionDimensionDelete)).
						Delete("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", evaluationHandler.Delete)
				}

				// Agent Evaluation SLICE 2 — the dataset, the run and the
				// read-only scorecard. Thirteen routes: five for the dataset,
				// three for its cases, four for the run and one scorecard.
				//
				// STILL NOT REGISTERED, and absent rather than stubbed: the
				// suite and binding families, the dataset import and the
				// conversation promote, the human-score writes, the platform
				// catalogue and `generate_eval_dimensions`. Twelve of the
				// reference's thirty-eight operations are here; the rest answer
				// 404, which is what an unbuilt route should answer.
				//
				// The gates use the same package constants and the same
				// `apimw.RequireResolvedPermissions` construction the dimension
				// four use, and NOT router.go's `projectPermission` helper, for
				// the reason recorded above and in
				// migrations/shared/0115_evaluation_dataset_run_permissions.sql:
				// Agent Evaluation is not in the pylon corpus this repository
				// carries, so these names have no `check_api` to be transcribed
				// from and the helper's provenance assertion would be false.
				// router_permission_grant_gate_test.go still binds them to a
				// shared migration, and 0115 is that migration.
				//
				// CANCEL is gated on `run.create` and not on a `run.cancel`
				// string, because the reference's vocabulary has no such
				// string: the right that started a run is the right that stops
				// it. Inventing a name would ship either a widened vocabulary
				// or a permanent 403 (#313).
				if cfg.EvalDatasetsRepo != nil {
					datasetHandler := v2evaluation.NewDatasetHandler(cfg.EvalDatasetsRepo)
					requireDatasetRead := evaluationGate(v2evaluation.PermissionDatasetRead)
					requireDatasetUpdate := evaluationGate(v2evaluation.PermissionDatasetUpdate)

					r.With(requireDatasetRead).
						Get("/eval_datasets/prompt_lib/{projectID}", datasetHandler.List)
					r.With(evaluationGate(v2evaluation.PermissionDatasetCreate)).
						Post("/eval_datasets/prompt_lib/{projectID}", datasetHandler.Create)
					r.With(requireDatasetRead).
						Get("/eval_dataset/prompt_lib/{projectID}/{datasetID}", datasetHandler.Get)
					r.With(requireDatasetUpdate).
						Put("/eval_dataset/prompt_lib/{projectID}/{datasetID}", datasetHandler.Update)
					r.With(evaluationGate(v2evaluation.PermissionDatasetDelete)).
						Delete("/eval_dataset/prompt_lib/{projectID}/{datasetID}", datasetHandler.Delete)

					// A CASE is part of its dataset, so its writes take the
					// dataset UPDATE permission rather than a permission of
					// their own. The reference declares none for cases, and a
					// caller who may not edit a dataset must not be able to
					// change what it asks.
					r.With(requireDatasetUpdate).
						Post("/eval_dataset_cases/prompt_lib/{projectID}/{datasetID}", datasetHandler.AddCase)
					r.With(requireDatasetUpdate).
						Put("/eval_dataset_case/prompt_lib/{projectID}/{datasetID}/{caseID}", datasetHandler.UpdateCase)
					r.With(requireDatasetUpdate).
						Delete("/eval_dataset_case/prompt_lib/{projectID}/{datasetID}/{caseID}", datasetHandler.DeleteCase)
				}

				// The run routes need BOTH repositories: the run one for the
				// rows, and the dimension one to freeze the snapshot a run is
				// scored against. Registering them on the run repository alone
				// would produce a route that starts runs whose snapshot has no
				// dimensions in it, and every case would then score nothing
				// behind a 201.
				if cfg.EvalRunsRepo != nil && cfg.EvalDimensionsRepo != nil {
					runHandler := v2evaluation.NewRunHandler(
						cfg.EvalRunsRepo, cfg.EvalDimensionsRepo, cfg.EvalOrchestrator)
					requireRunRead := evaluationGate(v2evaluation.PermissionRunRead)
					requireRunWrite := evaluationGate(v2evaluation.PermissionRunCreate)

					r.With(requireRunRead).
						Get("/eval_runs/prompt_lib/{projectID}", runHandler.List)
					r.With(requireRunWrite).
						Post("/eval_runs/prompt_lib/{projectID}", runHandler.Start)
					r.With(requireRunRead).
						Get("/eval_run/prompt_lib/{projectID}/{runID}", runHandler.Get)
					r.With(requireRunWrite).
						Post("/eval_run_cancel/prompt_lib/{projectID}/{runID}", runHandler.Cancel)
					r.With(requireRunRead).
						Get("/eval_results/prompt_lib/{projectID}/{runID}", runHandler.Results)
				}

				// Conversations
				if cfg.ConvsRepo != nil {
					// S20a: chat attachment byte path — WithPool/WithObjectStore/
					// WithAttachmentStore are no-ops (nil) when cfg.Pool/
					// cfg.ObjectStore are unset, so AddAttachments' JSON-metadata
					// branch keeps working exactly as before wherever storage isn't
					// wired (matching newArtifactHandler's own degrade convention).
					convHandler = v2convs.NewHandler(cfg.ConvsRepo).
						WithPool(cfg.Pool).
						WithObjectStore(cfg.ObjectStore).
						WithAttachmentStore(newAttachmentStore(cfg.Pool)).
						WithUserContextDefaults(newUserContextDefaults(cfg.Pool))
					requireConversationRead := projectPermission("models.chat.conversation.details")
					requireMessageDelete := projectPermission("models.chat.messages.delete")
					requireEntitySettings := projectPermission("models.chat.entity_settings.update")
					requireConversationEdit := projectPermission("models.chat.conversation.edit")
					r.With(projectPermission("models.chat.conversations.list")).
						Get("/conversations/prompt_lib/{projectID}", convHandler.List)
					r.With(projectPermission("models.chat.conversations.create")).
						Post("/conversations/prompt_lib/{projectID}", convHandler.Create)
					r.With(requireConversationRead).
						Get("/conversation/prompt_lib/{projectID}/{conversationID}", convHandler.Get)
					r.With(projectPermission("models.chat.conversation.update")).
						Put("/conversation/prompt_lib/{projectID}/{conversationID}", convHandler.Update)
					r.With(projectPermission("models.chat.conversations.delete")).
						Delete("/conversation/prompt_lib/{projectID}/{conversationID}", convHandler.Delete)
					r.With(projectPermission("models.chat.messages.list")).
						Get("/messages/prompt_lib/{projectID}/{conversationID}", convHandler.ListMessages)
					r.With(requireMessageDelete).
						Delete("/messages/prompt_lib/{projectID}/{conversationID}", convHandler.DeleteMessages)
					// message.py declares BOTH verbs on one module, so the
					// per-message read shares this DELETE's URL and its
					// {messageID} param — pylon's `message_group_uid`, a
					// message-GROUP uuid string (message.py:176-183). The read
					// declares `models.chat.messages.details`
					// (message.py:39), which is NOT a new name: it is the
					// string message_trace.py already declares and 0063 already
					// grants, so mounting this needs no migration and 403s
					// nobody who could read the transcript.
					//
					// GetMessage was implemented and routed by nothing until
					// now (#126's dead-wiring class). Its repository read is
					// the only caller of ConversationsRepo.GetMessageByUUID,
					// whose unconditional LEFT JOIN on chat_messages_canvas is
					// what the fresh-install tenant migration made safe.
					r.With(projectPermission("models.chat.messages.details")).
						Get("/message/prompt_lib/{projectID}/{messageID}", convHandler.GetMessage)
					r.With(requireMessageDelete).
						Delete("/message/prompt_lib/{projectID}/{messageID}", convHandler.DeleteMessage)
					r.With(projectPermission("models.chat.participants.create")).
						Post("/participants/prompt_lib/{projectID}/{conversationID}", convHandler.AddParticipant)
					r.With(projectPermission("models.chat.participant.delete")).
						Delete("/participant/prompt_lib/{projectID}/{conversationID}/{participantID}", convHandler.RemoveParticipant)
					r.With(requireEntitySettings).
						Put("/entity_settings/prompt_lib/{projectID}/{conversationID}/{participantID}", convHandler.UpdateEntitySettings)
					r.With(requireEntitySettings).
						Patch("/entity_settings/prompt_lib/{projectID}/{conversationID}", convHandler.BatchUpdateEntitySettings)
					// select_conversation.py declares the conversation READ for
					// both its verbs — selecting one is a per-user marker, not
					// an edit of the conversation.
					r.With(requireConversationRead).
						Post("/select_conversation/prompt_lib/{projectID}/{conversationID}", convHandler.SelectConversation)
					r.With(requireConversationRead).
						Delete("/select_conversation/prompt_lib/{projectID}", convHandler.DeselectConversation)
					r.With(projectPermission("models.chat.conversations.regenerate")).
						Post("/regenerate/prompt_lib/{projectID}/{conversationID}", convHandler.Regenerate)
					r.With(projectPermission("models.chat.canvas.create")).
						Post("/canvases/prompt_lib/{projectID}", convHandler.CreateCanvas)
					r.With(projectPermission("models.chat.canvas.details")).
						Get("/canvas/prompt_lib/{projectID}/{canvasID}", convHandler.GetCanvas)
					r.With(projectPermission("models.chat.canvas.update")).
						Put("/canvas/prompt_lib/{projectID}/{canvasID}", convHandler.UpdateCanvas)
					// Canvas editor presence (#622). A heartbeat, published onto
					// events.ProjectChannel({projectID}) — the SSE room primitive
					// mounted further down this same group — instead of the
					// socket.io server #615 decided not to rebuild.
					//
					// It takes `projectPermission`, the SAME composition root as
					// the three canvas routes above it, and the same permission
					// string as the canvas READ: announcing presence on a canvas
					// is not a wider claim than reading it, and reusing the
					// string is what keeps this route out of a new migration.
					//
					// WithRedis is called UNCONDITIONALLY on purpose. It is a
					// no-op on a nil client, and the route serves either way on
					// the package's in-process store, so this is not a
					// registration gate — see the option's own note.
					r.With(projectPermission(v2canvaspresence.Permission)).
						Post("/canvas/prompt_lib/{projectID}/{canvasID}/presence",
							v2canvaspresence.NewHandler(
								cfg.ConvsRepo,
								v2canvaspresence.WithRedis(cfg.RedisClient),
							).Heartbeat)
					// attachment_storage has no pylon module; it writes the
					// conversation's own storage setting, so it takes
					// context_strategy.py's string — the other per-conversation
					// setting write — rather than a name invented for it.
					r.With(requireConversationEdit).
						Put("/attachment_storage/prompt_lib/{projectID}/{conversationID}", convHandler.UpdateAttachmentStorage)
					r.With(projectPermission("models.chat.attachments.create")).
						Post("/attachments/prompt_lib/{projectID}/{conversationID}", convHandler.AddAttachments)
					r.With(projectPermission("models.chat.attachments.delete")).
						Delete("/attachments/prompt_lib/{projectID}/{conversationID}", convHandler.DeleteAttachments)
					r.With(requireConversationRead).
						Get("/context_analytics/prompt_lib/{projectID}/{conversationID}", convHandler.GetContextStatus)
					// The strategy READ is new here (pylon exposed only the
					// write). It takes the conversation-details permission,
					// like every other read of this conversation's own
					// settings, and it sits on the same path as the write so
					// the pair reads as one resource.
					r.With(requireConversationRead).
						Get("/context_strategy/prompt_lib/{projectID}/{conversationID}", convHandler.GetContextStrategy)
					r.With(requireConversationEdit).
						Put("/context_strategy/prompt_lib/{projectID}/{conversationID}", convHandler.UpdateContextStrategy)
				}

				// Share a conversation by link — the OWNER-FACING half. The
				// anonymous half (shared_chat_view / shared_chat_view_unlock)
				// is registered far above this group, outside every auth
				// middleware; see mountSharedChatAnonymousRoutes.
				//
				// PERMISSIONS. Listing links takes the conversation READ
				// permission: a link is metadata about a conversation the
				// caller can already read. Creating and revoking take the
				// conversation UPDATE permission, not the read one —
				// publishing a transcript to anyone holding a URL, and taking
				// that publication away, are changes to who can see the
				// conversation, and the weakest role that may merely read it
				// must not be the role that may expose it.
				//
				// They sit inside this group, so `projectScoped` has already
				// bound {projectID} to the caller's membership before the
				// permission is resolved.
				if cfg.SharedChatStore != nil {
					sharedChatHandler := sharedchat.NewHandler(
						cfg.SharedChatStore, cfg.SharedChatTranscript, []byte(cfg.SessionSecret))
					r.With(projectPermission("models.chat.conversation.details")).
						Get("/shared_chat_links/prompt_lib/{projectID}/{conversationID}", sharedChatHandler.List)
					r.With(projectPermission("models.chat.conversation.update")).
						Post("/shared_chat_links/prompt_lib/{projectID}/{conversationID}", sharedChatHandler.Create)
					r.With(projectPermission("models.chat.conversation.update")).
						Delete("/shared_chat_link/prompt_lib/{projectID}/{conversationID}/{linkID}", sharedChatHandler.Revoke)
				}

				// NOTE(#126): the Predict/LLM, Chat and Pipeline-trigger route
				// groups stood here, each behind a nil gate on RouterConfig's
				// Predictor, ChatService or PipelineRunner field. Nothing ever
				// assigned those fields, so the groups were never registered
				// and the paths 404'd in every deployment:
				//   POST   /chat/prompt_lib/{projectID}/{conversationID}/messages
				//   GET|POST|PUT /pipeline_trigger/prompt_lib/{projectID}/pipeline/{versionID}/trigger
				// See the IndexerDeps note at the top of this file for why the
				// transport behind them was retired rather than repaired, and
				// #192/#193/#93 for the capability records.
				//
				// FOUR paths have left that list and are NOT in it any more. A
				// comment that keeps claiming a route is absent after it has
				// landed is the "disclosed gap goes stale" failure this
				// repository has produced repeatedly, so each line was removed
				// rather than annotated:
				//   POST   /predict_llm/prompt_lib/{projectID} — registered
				//          immediately below (#194).
				//   GET    /chat_config/prompt_lib/{projectID} — served by
				//          internal/api/v2/promptcontextreads, mounted in
				//          production_router.go (#194).
				//   DELETE /task/prompt_lib/{projectID}/{responseMessageID} —
				//          served by internal/api/v2/agentexecution, mounted in
				//          production_router.go under the runtime plane.
				//   GET|DELETE /application_task/prompt_lib/{projectID}/
				//          {responseMessageID} — served by the same package and
				//          mounted the same way (#254 P2).

				// Predict LLM — one stateless turn, no agent, no tools, no
				// version id (#194). Registered UNCONDITIONALLY, unlike the
				// group it replaces: the handler carries its own optional
				// dependency and answers 503 naming LLM_GATEWAY_URL when no LLM
				// plane is composed, so an unconfigured deployment is
				// distinguishable from a missing route. Gating the registration
				// on the dependency instead is exactly what produced #126.
				//
				// The permission is legacy's own
				// (elitea_core/api/v2/predict_llm.py check_api), and its
				// default-mode split reaches VIEWER — see
				// migrations/shared/0105_predict_llm_permission.sql, which is
				// what grants it.
				r.With(projectPermission("models.applications.predict.post")).
					Post("/predict_llm/prompt_lib/{projectID}", v2predict.NewHandler(cfg.PredictCompleter).PredictLLM)

				// Batch version replacement
				if cfg.AppsRepo != nil {
					appHandler := v2apps.NewHandler(cfg.AppsRepo, cfg.Pool)
					// Neither this route nor the one below it has a pylon
					// module: both are Go-side additions that WRITE an
					// application version — one swaps a version reference across
					// a project's agents, the other rewrites a version's
					// attachment-storage setting — so both take version.py's
					// update string rather than a name invented for them.
					r.With(projectPermission("models.applications.version.update")).
						Post("/batch_replace_version/prompt_lib/{projectID}/{oldVersionID}/{newVersionID}", appHandler.BatchReplaceVersion)
				}

				// Application attachment storage
				r.With(projectPermission("models.applications.version.update")).
					Put("/application_attachment_storage/prompt_lib/{projectID}/{applicationID}/{versionID}", coreHandler.UpdateAttachmentStorage)

				// The three AI-draft routes (#254 P1). They stood in the
				// NOTE(#126) tombstone here with webchat, behind the same nil
				// gate on RouterConfig.Predictor, and answered 404 in every
				// deployment. They are pure LLM plays — one blocking turn each,
				// no runtime, no task — so they take the same PredictCompleter
				// predict_llm above takes, and are registered UNCONDITIONALLY
				// for the same reason it is: the handler answers 503 naming
				// LLM_GATEWAY_URL when no LLM plane is composed, so an
				// unconfigured deployment stays distinguishable from a missing
				// route. Gating the registration on the dependency is exactly
				// what produced #126.
				//
				// The permissions are legacy's own check_api declarations, with
				// ONE deliberate substitution. generate_project_context_draft
				// declares `models.project_context.generate`, a string this
				// repository's migration history does not grant and legacy's
				// own catalogue reaches only through that one module; gating on
				// it would ship a permanent 403 nobody can clear (#313).
				// `models.project_context.edit` is granted by 0068 to exactly
				// the roles legacy's recommended_roles gives .generate — admin
				// and editor, never viewer — and it is the permission the
				// caller needs anyway, since the only thing to do with a
				// generated Project Background is save it.
				//
				// NOT restored, here or anywhere: webchat. It is not a chat
				// surface. legacy/plugins/elitea_core/api/v2/webchat.py is an
				// unauthenticated Microsoft Bot Framework webhook stub that
				// echoes the caller's own text back through a connector URL
				// taken from the request body, with the literal placeholder
				// strings 'MICROSOFT-APP-ID' and 'MICROSOFT-APP-PASSWORD' as
				// its credentials and a `# FIXME: auth` where the permission
				// gate belongs. It never predicts, never reads its own
				// {version_id}, and creates no conversation. #254 carries the
				// full finding and a bounded design for a real public chat
				// surface, which is a NEW feature and not a restoration.
				draftHandler := v2drafts.NewHandler(cfg.PredictCompleter)
				r.With(projectPermission("models.applications.applications.create")).
					Post("/generate_application_draft/prompt_lib/{projectID}", draftHandler.GenerateApplicationDraft)
				r.With(projectPermission("models.applications.skills.create")).
					Post("/generate_skill_draft/prompt_lib/{projectID}", draftHandler.GenerateSkillDraft)
				r.With(projectPermission("models.project_context.edit")).
					Post("/generate_project_context_draft/prompt_lib/{projectID}", draftHandler.GenerateProjectContextDraft)

				// Fork, and the application publish plane. These are the
				// routes #302 names explicitly: publish writes a catalogue row
				// for {projectID}, unpublish reverts that project's version to
				// draft with a bare `UPDATE p_<projectID>.application_versions`,
				// and fork imports an entity INTO the named project. The only
				// caller-related read any of them performed was
				// publishGuardrail, which is a deployment-wide kill switch plus
				// a project whitelist — not a membership check.
				//
				// #302 asks explicitly for publish and unpublish to be gated on
				// the publish permission rather than on membership; these are
				// the strings publish.py, unpublish.py, publish_validate.py and
				// fork.py declare. publish and unpublish are DIFFERENT names in
				// pylon, and the legacy matrix withholds both from a viewer —
				// which is the whole point of the tier, since a viewer that
				// could publish would push a project's agent into the public
				// catalogue.
				requirePublish := projectPermission("models.applications.publish.post")
				requireVersionValidate := projectPermission("models.applications.version_validator.check")
				// The fork route runs the FORK handler (#505). It was registered
				// on ExportImportPost, so `Handler.Fork` had no caller and a fork
				// ran the import. The import does not read the keys `?fork=true`
				// adds to the export the wizard sends it: it leaves
				// llm_settings.model_project_id pointing at the SOURCE project,
				// writes no meta.parent_entity_id, and drops every version variable
				// and tag. api/openapi/v2.yaml already describes this path as the
				// Fork handler.
				r.With(projectPermission("models.applications.fork.post")).
					Post("/fork/prompt_lib/{projectID}", coreHandler.Fork)
				r.With(requirePublish).Post("/publish/prompt_lib/{projectID}/{versionID}", coreHandler.Publish)
				r.With(projectPermission("models.applications.unpublish.post")).
					Post("/unpublish/prompt_lib/{projectID}/{versionID}", coreHandler.Unpublish)
				r.With(requirePublish).
					Get("/publish_validate/prompt_lib/{projectID}/{versionID}", coreHandler.PublishValidate)
				r.With(requirePublish).
					Post("/publish_validate/prompt_lib/{projectID}/{versionID}", coreHandler.PublishValidate)
				r.With(requireVersionValidate).
					Post("/version_validator/prompt_lib/{projectID}/{applicationID}/{versionID}", coreHandler.VersionValidator)
				r.With(requireVersionValidate).
					Get("/version_validator/prompt_lib/{projectID}/{applicationID}/{versionID}", coreHandler.VersionValidator)

				// Public applications
				r.Get("/public_applications/prompt_lib", coreHandler.PublicApplications)
				r.Get("/public_applications/prompt_lib/", coreHandler.PublicApplications)
				r.Get("/public_application/prompt_lib/{applicationID}", coreHandler.PublicApplications)
				r.Get("/public_application/prompt_lib/{applicationID}/{versionName}", coreHandler.PublicApplications)

				// Skill publishing (#249) — the skill-level counterpart of the
				// application publish block above, plus the skills-parity
				// extras that live in the same domain. Registered here rather
				// than in the skills block because none of it goes through
				// SkillsRepo: publishing is cross-schema SQL, the way
				// application publishing is.
				skillPublishHandler := v2skillpublish.NewHandler(cfg.Pool)

				// The catalog reads are project-independent: they serve the
				// public project's schema and take no {projectID}, so there is
				// no membership to check.
				r.Get("/public_skills/prompt_lib", skillPublishHandler.PublicSkills)
				r.Get("/public_skills/prompt_lib/", skillPublishHandler.PublicSkills)
				r.Get("/public_skill/prompt_lib/{skillID}", skillPublishHandler.PublicSkill)
				r.Get("/public_skill/prompt_lib/{skillID}/{versionName}", skillPublishHandler.PublicSkill)

				// Everything project-scoped carries the permission its pylon
				// module declares.
				//
				// These routes take {projectID} from the path and then read and
				// write THAT project's schema — unpublish deletes a catalog row
				// and reverts a source version, attach creates a skill and maps
				// it onto that project's agents. They shipped behind the
				// membership check because nothing granted a permission then;
				// #313's seeding migration is what lets them carry one now.
				// pylon's publish_skill.py, unpublish_skill.py and
				// publish_skill_validate.py all declare ONE string —
				// `skills.publish` — unlike the application plane above, where
				// publish and unpublish are separate names.
				requireSkillPublish := projectPermission("models.applications.skills.publish")
				requireSkillFork := projectPermission("models.applications.fork.post")
				r.With(requireSkillPublish).
					Post("/publish_skill/prompt_lib/{projectID}/{skillID}/{versionID}", skillPublishHandler.Publish)
				r.With(requireSkillPublish).
					Post("/unpublish_skill/prompt_lib/{projectID}/{skillID}/{versionID}", skillPublishHandler.Unpublish)
				r.With(requireSkillPublish).
					Post("/publish_skill_validate/prompt_lib/{projectID}/{skillID}/{versionID}", skillPublishHandler.PublishValidate)
				r.With(requireSkillFork).
					Post("/attach_public_skill/prompt_lib/{projectID}", skillPublishHandler.AttachPublicSkill)
				r.With(projectPermission("models.promptlib_shared.tags.list")).
					Get("/skill_categories/prompt_lib/{projectID}", skillPublishHandler.SkillCategories)
				r.With(requireSkillFork).
					Get("/skill_export_fork/prompt_lib/{projectID}/{skillID}", skillPublishHandler.ExportFork)
				r.With(requireSkillFork).
					Get("/skill_export_fork/prompt_lib/{projectID}/{skillID}/{versionID}", skillPublishHandler.ExportFork)
				r.With(projectPermission("models.applications.applications.details")).
					Get("/agents_with_skill/prompt_lib/{projectID}/{skillID}", skillPublishHandler.AgentsWithSkill)

				// Check version in use
				r.With(projectPermission("models.applications.version.details")).
					Get("/check_version_in_use/prompt_lib/{projectID}/{appID}/{versionID}", coreHandler.ApplicationRelation)

				// Authors / trending. /author names no project in its path —
				// pylon's author.py is a ProjectAPI whose id is the AUTHOR's —
				// so there is nothing for a project gate to resolve against and
				// it stays as it is; the social plugin serves the same resource
				// ungated (#161).
				r.Get("/author/prompt_lib/{authorID}", coreHandler.Author)
				r.With(projectPermission("models.applications.trending_authors.list")).
					Get("/trending_authors/prompt_lib/{projectID}", coreHandler.TrendingAuthors)

				// Moderation used to be registered here as well, on
				// `/elitea_core/moderation_status/…`. pylon serves that resource
				// under `admin` only and no client has ever called this copy; it
				// is removed with the stub it pointed at rather than re-pointed
				// at the real handler, which would publish a second URL for the
				// same rows. See internal/api/v2/moderation/requests.go.

				// Application relations. application_relation.py declares only
				// `patch`; the GET beside it is the same handler answering the
				// same rows as check_version_in_use above, so it takes that
				// module's read string.
				r.With(projectPermission("models.applications.version.details")).
					Get("/application_relation/prompt_lib/{projectID}/{appID}/{versionID}", coreHandler.ApplicationRelation)
				// PATCH used to be bound to ApplicationRelation — the READ
				// handler — so UpdateApplicationRelation was unreachable and
				// every agent-as-tool attach answered 200 while writing
				// nothing. Found in a live browser: the + Agent picker's PATCH
				// returned the relation LIST and no row appeared anywhere.
				r.With(projectPermission("models.applications.application_relation.patch")).
					Patch("/application_relation/prompt_lib/{projectID}/{appID}/{versionID}", coreHandler.UpdateApplicationRelation)

				// Recommendations — recommendations.py declares the agent LIST
				// permission, since that is what it returns a slice of.
				r.With(projectPermission("models.applications.applications.list")).
					Get("/recommendations/prompt_lib/{projectID}", coreHandler.Recommendations)

				// Feedbacks — left ungated on purpose. #302's acceptance notes
				// name feedbacks.py among the modules legacy does not gate, and
				// pylon serves this resource from the SOCIAL plugin, whose
				// feedback.py guard is a different resource (one feedback row by
				// id, not the project listing this path returns).
				r.Get("/feedbacks/default/{projectID}", coreHandler.Feedbacks)

				// Analytics (flat paths matching UI expectations). All seven
				// pylon analytics_*.py modules declare the SAME string as
				// analytics_costs.py below — `models.monitoring.tracing.view` —
				// so the eighth endpoint is no longer the only gated one. The
				// grant that makes it resolvable on a Go-bootstrapped database
				// is 0063's, already in the history.
				if cfg.AnalyticsRepo != nil {
					analyticsHandler := v2analytics.NewHandler(cfg.AnalyticsRepo)
					requireAnalyticsView := projectPermission(v2analytics.ViewPermission)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics/prompt_lib/{projectID}", analyticsHandler.Usage)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_agents/prompt_lib/{projectID}", analyticsHandler.Agents)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_agent_detail/prompt_lib/{projectID}", analyticsHandler.Agents)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_tools/prompt_lib/{projectID}", analyticsHandler.Tools)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_tool_detail/prompt_lib/{projectID}", analyticsHandler.Tools)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_users/prompt_lib/{projectID}", analyticsHandler.Users)
					r.With(requireAnalyticsView, requireAnalyticsEnabled).Get("/analytics_user_detail/prompt_lib/{projectID}", analyticsHandler.Users)
				}

				// The eighth analytics endpoint (issue 253), and the only one
				// of the eight that reads a real table: the LLM cost
				// breakdown, straight from gateway.llm_budget_accumulators —
				// the rows elitea-scheduler's budgetwriteback consumer folds
				// the gateway's GATEWAY_BUDGET_DELTAS into. Same source as
				// /usage and /project_budget (#246), so the three cannot
				// report different money.
				//
				// Registered OUTSIDE the cfg.AnalyticsRepo block above, and
				// behind no nil gate of its own: it does not use that
				// repository, and hanging a route off a dependency it does not
				// need is how six paths ended up registered in no deployment at
				// all (#126). It takes cfg.Pool the way the budgets routes do.
				//
				// Gated, unlike its seven neighbours, on the permission
				// analytics_costs.py itself declares. The neighbours answer
				// with counts and zero-filled stubs; this one answers with
				// spend. The default-mode grant that makes the gate resolvable
				// on a Go-bootstrapped database is seeded by
				// migrations/shared/0063_trace_and_cost_read_permissions.sql.
				costsHandler := v2analytics.NewCostsHandler(cfg.Pool)
				r.With(projectPermission(v2analytics.ViewPermission), requireAnalyticsEnabled).
					Get("/analytics_costs/prompt_lib/{projectID}", costsHandler.Costs)

				// Chat execution-step traces (issue 253) — the pin strip under
				// an agent's answer, and the step behind one pin.
				//
				// The producer is this service's own agent-execution trace
				// projection (internal/infra/db/repos/agent_trace.go writes
				// <tenant>.chat_message_trace_step on every frame), so these
				// are reads over a live table rather than an API waiting for a
				// schema. The old web app already called
				// `message_traces/prompt_lib/{projectId}/{conversationId}` —
				// see apps/elitea-web/src/processes/chat/model/
				// useLoadMoreMessages.ts, which records the fetch as a
				// not-yet-ported feature — so the paths and parameters are the
				// ones a client already speaks.
				//
				// Gated on the two permissions the pylon handlers declare,
				// resolved against the project in DEFAULT mode.
				traceHandler := v2messagetraces.NewHandler(cfg.Pool)
				r.With(projectPermission(v2messagetraces.ListPermission)).
					Get("/message_traces/prompt_lib/{projectID}/{conversationID}", traceHandler.List)
				r.With(projectPermission(v2messagetraces.DetailPermission)).
					Get("/message_trace/prompt_lib/{projectID}/{stepID}", traceHandler.Get)

				// Icons. default_icons.py carries no `check_api` at all — it
				// returns the deployment's built-in icon set, the same list for
				// every project — so it stays ungated, as #302's acceptance
				// notes require for the modules legacy leaves open. upload_icon.py
				// declares all four verbs, and the legacy matrix withholds every
				// one of them from a viewer INCLUDING the GET, which is why the
				// listing is gated on `.get` rather than treated as a free read.
				r.Get("/default_icons/prompt_lib/{projectID}", coreHandler.DefaultIcons)
				requireIconUpload := projectPermission("models.applications.upload_icon.post")
				r.With(projectPermission("models.applications.upload_icon.get")).
					Get("/upload_icon/prompt_lib/{projectID}", coreHandler.ListUploadedIcons)
				r.With(requireIconUpload).Post("/upload_icon/prompt_lib/{projectID}", coreHandler.UploadIcon)
				r.With(requireIconUpload).Post("/upload_icon/prompt_lib/{projectID}/{entityID}", coreHandler.UploadIcon)
				r.With(projectPermission("models.applications.upload_icon.update")).
					Put("/upload_icon/prompt_lib/{projectID}/{versionId}", coreHandler.UpdateIcon)
				r.With(projectPermission("models.applications.upload_icon.delete")).
					Delete("/upload_icon/prompt_lib/{projectID}/{name}", coreHandler.DeleteIcon)

				// Skill icons. upload_skill_icon.py declares the same four verbs
				// as upload_icon.py under its own permission family
				// (`models.applications.skills.upload_icon.*`), and the legacy
				// matrix withholds all four from a viewer — the GET included,
				// exactly as it does for the agent listing. 0100 grants them.
				//
				// The POST is registered twice for the same reason the agent one
				// is: pylon's url_params carry an OPTIONAL trailing version id,
				// and with it the upload binds the icon to that skill version in
				// one call.
				requireSkillIconUpload := projectPermission("models.applications.skills.upload_icon.post")
				r.With(projectPermission("models.applications.skills.upload_icon.get")).
					Get("/upload_skill_icon/prompt_lib/{projectID}", coreHandler.ListSkillIcons)
				r.With(requireSkillIconUpload).
					Post("/upload_skill_icon/prompt_lib/{projectID}", coreHandler.UploadSkillIcon)
				r.With(requireSkillIconUpload).
					Post("/upload_skill_icon/prompt_lib/{projectID}/{versionId}", coreHandler.UploadSkillIcon)
				r.With(projectPermission("models.applications.skills.upload_icon.update")).
					Put("/upload_skill_icon/prompt_lib/{projectID}/{versionId}", coreHandler.UpdateSkillIcon)
				r.With(projectPermission("models.applications.skills.upload_icon.delete")).
					Delete("/upload_skill_icon/prompt_lib/{projectID}/{name}", coreHandler.DeleteSkillIcon)

				// Export/Import. export_import.py declares `get` — the EXPORT —
				// and nothing else; the POST beside it is an import, and
				// import_wizard.py declares exactly that string for exactly that
				// operation, so the two share it rather than the POST inheriting
				// the export's read tier. export_converter.py carries no guard in
				// pylon and names no project in its path, so it stays open.
				r.With(projectPermission("models.applications.export_import.import")).
					Post("/export_import/prompt_lib/{projectID}/{entityID}", coreHandler.ExportImportPost)
				r.With(projectPermission("models.applications.export_import.export")).
					Get("/export_import/prompt_lib/{projectID}/{entityID}", coreHandler.ExportImportGet)
				r.Post("/export_converter/prompt_lib", coreHandler.ExportConverter)

				// Pin — left ungated on purpose, as #302's acceptance notes
				// require: pylon serves it from social/api/v2/pin.py, which the
				// legacy catalogue lists among the UNGUARDED handlers. Both verbs
				// write only rows keyed by the caller's own user id.
				r.Post("/pin/prompt_lib/{projectID}/{entityType}/{entityID}", coreHandler.Pin)
				r.Delete("/pin/prompt_lib/{projectID}/{entityType}/{entityID}", coreHandler.Unpin)

				// Project info/context. project_info.py, project_icon.py and
				// project_context.py all declare the same pair —
				// `models.project_context.view` for the reads and `.edit` for the
				// writes. `.view` is 0062's grant, already in the history; `.edit`
				// arrives with 0068.
				requireProjectContextView := projectPermission("models.project_context.view")
				requireProjectContextEdit := projectPermission("models.project_context.edit")
				r.With(requireProjectContextView).
					Get("/project_info/prompt_lib/{projectID}/project-info", coreHandler.ProjectInfo)
				r.With(requireProjectContextEdit).
					Put("/project_info/prompt_lib/{projectID}/project-info", coreHandler.UpdateProjectInfo)
				r.With(requireProjectContextView).
					Get("/project_icon/prompt_lib/{projectID}", coreHandler.ListProjectIcons)
				r.With(requireProjectContextEdit).
					Post("/project_icon/prompt_lib/{projectID}", coreHandler.CreateProjectIcon)
				r.With(requireProjectContextEdit).
					Delete("/project_icon/prompt_lib/{projectID}/{name}", coreHandler.DeleteProjectIcon)
				// Registered unconditionally: this is the ONLY registration
				// source for project-context GET/PUT in the router every real
				// deployment reaches (CURRENT_PARITY_EVIDENCE.md,
				// internal/api/v2/promptcontextreads — "the compatibility
				// router mounts the chat-config path only... the production
				// router mounts both", and #243 made this the only router
				// left). CurrentPromptContextReads' own project-context
				// handler is a real, RBAC-scoped, parity-verified
				// implementation, but wiring it here too would double-register
				// the same method+path (chi panics on that) — see
				// mountReviewedProductionRoutes (production_router.go), which
				// deliberately does not mount it for the same reason.
				//
				// The relative suffix is derived from
				// v2promptcontextreads.CurrentProjectContextPath (the "/api/v2/elitea_core"
				// prefix comes from this route's enclosing r.Route groups)
				// rather than a second hardcoded literal, purely so the two
				// files can't drift on what path they're both talking about.
				r.With(requireProjectContextView).
					Get(strings.TrimPrefix(v2promptcontextreads.CurrentProjectContextPath, "/api/v2/elitea_core"), coreHandler.ProjectContext)
				r.With(requireProjectContextEdit).
					Put(strings.TrimPrefix(v2promptcontextreads.CurrentProjectContextPath, "/api/v2/elitea_core"), coreHandler.UpdateProjectContext)

				// Platform settings — left ungated on purpose, and the reason is
				// recorded here because a project in the path makes the route
				// look gateable. platform_settings.py is one of the thirty-seven
				// handlers testdata/legacy/legacy-rbac-static-catalog.json lists
				// as UNGUARDED, so there is no permission to transcribe, and
				// #302's acceptance notes forbid tightening what legacy leaves
				// open.
				//
				// The handler DOES read the project: eliteacore/handler.go:104
				// overlays `p_{projectID}.configuration` where
				// type = 'environment_settings' onto ten built-in defaults. So
				// this is not an ungated route that happens to read nothing. What
				// it discloses is the project's own feature switches —
				// chat_enabled, mcp_enabled and eight more booleans of the same
				// shape — which the web app reads to decide which menu entries to
				// draw. No credential, no member, no content.
				// TestEliteaCoreLegacyUngatedRoutesStayUngated pins this.
				r.Get("/platform_settings/prompt_lib/{projectID}", coreHandler.PlatformSettings)
				r.Get("/platform_settings/prompt_lib", coreHandler.PlatformSettings)

				// Search
				r.With(projectPermission("models.promptlib_shared.search")).
					Get("/search_options/prompt_lib/{projectID}", coreHandler.SearchOptions)

				// MCP OAuth & sync. All three pylon modules declare
				// `models.applications.tool.patch`: each one writes a project's
				// toolkit rows, whatever the transport in front of it looks like.
				// The two proxies are mode-less ProjectAPI paths in pylon, which
				// is why they carry no `prompt_lib` segment here either.
				requireMCPToolWrite := projectPermission("models.applications.tool.patch")
				r.With(requireMCPToolWrite).Post("/mcp_oauth_proxy/{projectID}", coreHandler.MCPOAuthProxy)
				r.With(requireMCPToolWrite).Post("/mcp_dcr_proxy/{projectID}", coreHandler.MCPDCRProxy)
				r.With(requireMCPToolWrite).Post("/mcp_sync_tools/prompt_lib/{projectID}", coreHandler.MCPSyncTools)

				// The MCP REST surface (issue 252 P1). All three stay at the
				// MEMBERSHIP tier, and unlike the tag writes above that is not a
				// gap waiting on a decision: pylon's tools_list.py and
				// tools_call.py are two of the thirty-seven handlers the legacy
				// catalogue lists as UNGUARDED, and internal_mcp_pat_status has
				// no pylon module at all. There is no permission to transcribe,
				// and #302's acceptance notes forbid tightening what legacy
				// leaves open — but each still names a project in its path and
				// then reads that project's toolkit rows or the caller's own
				// tokens, so the membership check they shipped with stays.
				//
				// The modes are pylon's, not a guess: tools_list/tools_call
				// register `c.DEFAULT_MODE` only, internal_mcp_pat_status
				// registers `prompt_lib` only. A mode pylon does not serve on a
				// path 404s here rather than being answered with something
				// plausible.
				//
				// tools_list is registered in BOTH mode shapes. pylon's
				// `api_tools.with_modes` registers a route with and without the
				// mode segment, so `/tools_list/1` and `/tools_list/default/1`
				// are one endpoint there. The Python execution worker builds
				// the mode-less form (`elitea_sdk/runtime/clients/client.py`),
				// and the hybrid edge matches the mode-less form too, so
				// registering only `/default/` would leave the caller that
				// actually exists unserved.
				//
				// tools_call still answers 501 with a stated reason — see
				// internal/api/v2/mcp/registry.go. It is registered rather than
				// left off so the refusal is explicit and pinned by a test: a
				// 404 leaves the next person free to wire a stub up.
				// The resolver asks for the personal project it could not
				// find, exactly as the MCP server route above does.
				mcpResolver := apimw.NewDBPersonalProjectResolver(cfg.Pool)
				if personalProjects != nil {
					mcpResolver = mcpResolver.WithPersonalProjectEnsurer(personalProjects)
				}
				mcpHandler := v2mcp.NewHandlerWithToolkitRuns(cfg.Pool, mcpResolver, cfg.MCPAgentStart, cfg.MCPToolkitRun, permissionResolver)
				r.Group(func(r chi.Router) {
					r.Use(projectScoped)
					r.Get("/tools_list/{projectID}", mcpHandler.ToolsList)
					r.Get("/tools_list/default/{projectID}", mcpHandler.ToolsList)
					r.Post("/tools_call/default/{projectID}", mcpHandler.ToolsCall)
					r.Get("/internal_mcp_pat_status/prompt_lib/{projectID}/{toolkitType}", mcpHandler.InternalMCPPATStatus)
				})

				// Import wizard
				r.With(projectPermission("models.applications.export_import.import")).
					Post("/import_wizard/prompt_lib/{projectID}", coreHandler.ExportImportPost)

				// Users / Roles (served under /admin/ for UI compat, registered
				// here as fallback). elitea_core has no users.py or roles.py;
				// pylon serves both from the ADMIN plugin, whose ProjectAPI
				// variants declare `configuration.users.users.view` and
				// `configuration.roles.roles.view`. Both handlers here read the
				// named project's membership — Users returns every member's
				// email — so the project-scoped tier is the right one.
				r.With(projectPermission("configuration.users.users.view")).
					Get("/users/{mode}/{projectID}", coreHandler.Users)
				r.With(projectPermission("configuration.roles.roles.view")).
					Get("/roles/{mode}/{projectID}", coreHandler.Roles)
				// /permissions stays ungated, and gating it would be the
				// agent_categories mistake in a second shape. It is the caller's
				// own permission SELF-READ: the handler resolves the requester's
				// permissions for the project and hands them back, so a
				// non-member already gets an empty list and there is nothing to
				// leak. pylon's counterpart is auth/api/v2/permissions.py, which
				// the legacy catalogue lists among the UNGUARDED handlers. The
				// admin plugin's permissions.py IS guarded, on
				// `configuration.roles.permissions.view`, but that is the
				// role→permission MATRIX editor and a different resource; the
				// legacy matrix grants that string to the default-mode ADMIN
				// alone, so borrowing it here would 403 every editor and viewer
				// on the request the web app uses to decide what to render.
				r.Get("/permissions/prompt_lib/{projectID}", coreHandler.Permissions)

				// The admin SERVICE DESCRIPTORS page (unit A14) — pylon's
				// provider hub. All three routes answer 501 with one reason;
				// see internal/api/v2/eliteacore/service_descriptors.go for
				// why, and for what each of them used to do.
				//
				// The listing was `r.Get("/admin/{mode}", …)` answering 200
				// with three hardcoded rows naming `elitea_core`, `auth` and
				// `indexer` — Pylon plugin names, not providers — in a shape
				// the client does not read, from a handler taking
				// `_ *http.Request` and therefore ungated. `administration` is
				// now a STATIC segment because that is the only mode pylon
				// registers on this path (`mode_handlers = {'administration':
				// AdminAPI}`); another mode 404s rather than being answered
				// with something plausible, and the handler states its mode
				// rather than reading a `{mode}` param a static segment does
				// not bind.
				//
				// The two register_descriptor verbs had NO route at all —
				// `coreHandler.RegisterDescriptor` was dead code answering
				// `{"ok": true}` to a discarded body. They are registered now
				// so the refusal is explicit and pinned by a test: a 404 leaves
				// the next person free to wire the stub back up.
				//
				// Gated on the permissions the pylon originals declare,
				// resolved in administration mode — `runtime.airun
				// .serviceproviders` for the listing (elitea_core/api/v2/
				// admin.py) and `provider_hub.descriptor.register` for the
				// writes (elitea_core/api/v2/register_descriptor.py). The gate
				// runs before the refusal: which subsystems a deployment runs
				// is itself information about it.
				r.With(central(v2core.ServiceDescriptorListPermission)).
					Get("/admin/administration", coreHandler.ServiceDescriptors)
				// The admin dashboard's published-agents adoption listing
				// (#255), registered next to the other `/…/administration`
				// admin routes. `administration` is the only mode pylon's
				// admin_published_agents.py registers, so it is a STATIC
				// segment here too. Gated on the permission that file declares,
				// resolved in administration mode: the listing names every
				// agent this deployment has published and who published it.
				//
				// A handler by this name already existed in the eliteacore
				// package and was mounted on NO route — see the note where it
				// used to be, in handler.go.
				r.With(central(v2core.PublishedAgentsListPermission)).
					Get("/admin_published_agents/administration", coreHandler.AdminPublishedAgents)
				requireDescriptorRegister := central(v2core.ServiceDescriptorRegisterPermission)
				r.With(requireDescriptorRegister).
					Post("/register_descriptor/{projectID}", coreHandler.RegisterDescriptor)
				r.With(requireDescriptorRegister).
					Delete("/register_descriptor/{projectID}", coreHandler.RegisterDescriptor)

				// ACTIVATION (ADR-0012 phase P3, migration 0109). Migration
				// 0107 could record an admission and physically could not put
				// one in force: its CHECK requires an overlay revision and
				// nothing could issue one. 0109 adds the overlay table, and the
				// FOREIGN KEY it puts on `overlay_revision` is what makes the
				// constraint mean what it reads as — before it, any string
				// satisfied the CHECK.
				//
				// A DIFFERENT PERMISSION FROM THE TWO ABOVE, and that split is
				// the point of registering them separately here rather than
				// reusing `requireDescriptorRegister`. Every facade registrar
				// files a registration at boot, so `.register` is handed out
				// freely; `.activate` is the switch that lets agents call the
				// provider. A holder of `.register` alone gets 403 on these two
				// routes, which the acceptance suite asserts.
				//
				// Both are POST — deactivate is not a DELETE, because DELETE on
				// this surface already means REVOKE, which is terminal and
				// records revoked_at/by. Deactivating returns a revision to the
				// state registration left it in, and spelling two different
				// lifecycle transitions with one verb is how an operator
				// reaches for the recoverable one and gets the terminal one.
				//
				// Audited: `/api/v2/elitea_core/register_descriptor/` is
				// already an audited prefix, and these paths sit under it.
				requireDescriptorActivate := central(v2core.ServiceDescriptorActivatePermission)
				r.With(requireDescriptorActivate).
					Post("/register_descriptor/{projectID}/activate", coreHandler.ActivateDescriptor)
				r.With(requireDescriptorActivate).
					Post("/register_descriptor/{projectID}/deactivate", coreHandler.DeactivateDescriptor)

				// === Budgets and usage (issue #246) ===
				//
				// The port of legacy/plugins/elitea_core/api/v2/
				// {project_budget,project_budgets,user_budget,user_budgets,
				// usage}.py. The project-budget WRITE lands on
				// gateway.project_budget, which the LLM gateway's failmode
				// store reads on every call, so this is an enforcement path and
				// not a second write-only config table (#218).
				//
				// `prompt_lib` and `administration` are STATIC segments, as
				// they are throughout this file: the mode is not a value the
				// caller chooses, it selects which handler and which gate
				// runs. That matters more here than elsewhere — the
				// project-scoped member read carries an ownership check the
				// administration one deliberately does not, and a shared
				// `{mode}` route would let any member skip it by asking for
				// `administration`.
				//
				// Gates, transcribed from the pylon `check_api` declarations:
				// the project-scoped reads resolve `models.project_context.view`
				// against the project in DEFAULT mode; the administration reads
				// and writes resolve `models.admin.project_budgets.view` /
				// `.edit` centrally. The default-mode grant that makes the
				// former resolvable at all is seeded by
				// migrations/shared/0062_budgets_quota_statistics.sql — without
				// it, a project-scoped gate is 403-for-everyone on a
				// Go-bootstrapped database.
				budgetsHandler := v2budgets.NewHandler(cfg.Pool)
				requireBudgetsView := central(v2budgets.AdminViewPermission)
				requireBudgetsEdit := central(v2budgets.AdminEditPermission)
				requireProjectBudgetRead := apimw.RequireResolvedPermissions(
					permissionResolver, platformauth.PermissionModeDefault,
					v2budgets.ProjectViewPermission,
				)
				r.With(requireProjectBudgetRead).
					Get("/project_budget/prompt_lib/{projectID}/budget", budgetsHandler.GetProjectBudget)
				r.With(requireBudgetsView).
					Get("/project_budget/administration/{projectID}/budget", budgetsHandler.GetProjectBudgetAdmin)
				r.With(requireBudgetsEdit).
					Put("/project_budget/administration/{projectID}/budget", budgetsHandler.PutProjectBudget)
				// Clearing a budget is the same authority as setting one, so it
				// takes the SAME permission rather than a new string. A separate
				// `…delete` permission would need its own grant migration to
				// reach anybody and would be 403-for-everyone until it did
				// (#386), while the operator who may set a ceiling to any value
				// can already remove it in every way that matters.
				r.With(requireBudgetsEdit).
					Delete("/project_budget/administration/{projectID}/budget", budgetsHandler.DeleteProjectBudget)
				r.With(requireBudgetsView).
					Get("/project_budgets/administration", budgetsHandler.ListProjectBudgets)
				r.With(requireProjectBudgetRead).
					Get("/user_budget/prompt_lib/{projectID}/user_budget/{userID}", budgetsHandler.GetUserBudget)
				r.With(requireBudgetsView).
					Get("/user_budget/administration/{projectID}/user_budget/{userID}", budgetsHandler.GetUserBudgetAdmin)
				r.With(requireBudgetsEdit).
					Put("/user_budget/administration/{projectID}/user_budget/{userID}", budgetsHandler.PutUserBudget)
				r.With(requireBudgetsEdit).
					Delete("/user_budget/administration/{projectID}/user_budget/{userID}", budgetsHandler.DeleteUserBudget)
				r.With(requireProjectBudgetRead).
					Get("/user_budgets/prompt_lib/{projectID}", budgetsHandler.ListUserBudgets)
				r.With(requireBudgetsView).
					Get("/user_budgets/administration/{projectID}", budgetsHandler.ListUserBudgetsAdmin)
				r.With(requireProjectBudgetRead).
					Get("/usage/prompt_lib/{projectID}/usage", budgetsHandler.GetUsage)

				// The admin audit trail (unit A14). All four are READS; the
				// surface has no writes. Two of them (`audit`, `audit_heatmap`)
				// had no route at all before this unit, and the other two were
				// stubs returning empty arrays.
				//
				// Gated on the permission the pylon originals declare
				// (`models.admin.audit_trail.view`,
				// legacy/plugins/elitea_core/api/v2/audit*.py), resolved from
				// auth_core__user_role per request. Unlike the admin USER
				// listing, these reads are gated rather than open: an audit row
				// names the user, the project and the action taken, so the
				// listing itself is the sensitive part.
				requireAuditTrail := apimw.RequireCentralPermissions(
					permissionResolver, platformauth.PermissionModeAdministration,
					"models.admin.audit_trail.view",
				)
				r.With(requireAuditTrail).Get("/audit/{mode}", coreHandler.AuditTrail)
				r.With(requireAuditTrail).Get("/audit_heatmap/{mode}", coreHandler.AuditHeatmap)
				r.With(requireAuditTrail).Get("/audit_traces/{mode}", coreHandler.AuditTraces)
				r.With(requireAuditTrail).Get("/audit_trace_heatmap/{mode}", coreHandler.AuditTraceHeatmap)

				// Per-user event counts for ONE project — the admin Projects
				// page's activity drawer (unit A14). Same table and therefore
				// the same gate as the four reads above; pylon's
				// project_user_activity.py declares that same permission. It had
				// no route and no handler at all before this unit.
				r.With(requireAuditTrail).
					Get("/project_user_activity/{mode}", coreHandler.ProjectUserActivity)
			})

			// === Social plugin ===
			//
			// The personal-project ensurer is wired here and nowhere else.
			// `GET /social/author` is the endpoint that answers
			// `personal_project_id`, and until this option existed it answered
			// "" for every account a fresh deployment ever created: nothing in
			// this service provisioned the `project_user_<uid>` project the
			// resolver looks for. The SPA reads "" as "no personal project yet"
			// and parks the browser on `/onboarding`, which is where every new
			// user was stuck.
			//
			// The SAME pipeline the project-create route and the support
			// assistant use, for the reason stated at that call site: a project
			// assembled by a second, simpler path is a half-provisioned project.
			socialOptions := []v2social.Option{}
			if personalProjects != nil {
				socialOptions = append(socialOptions,
					v2social.WithPersonalProjectEnsurer(personalProjects))
			}
			r.Mount("/social", v2social.NewHandler(cfg.Pool, socialOptions...).Routes())

			// === Tracing plugin (issue #250) ===
			//
			// Port of legacy/plugins/tracing/api/v2/{collect,otlp,status}.py —
			// see internal/api/v2/tracing's package doc for the option-(a)
			// architecture decision and the "runtime.plugins" admin-status gate
			// (reusing the permission system_info.py/plugin_config_*.py/
			// maintenance.py/runtime_*.py already declare, rather than seeding
			// legacy's unseeded "models.admin.tracing.view" fresh).
			requireTracingAdminStatus := apimw.RequireCentralPermissions(
				permissionResolver, platformauth.PermissionModeAdministration,
				"runtime.plugins",
			)
			tracingHandler := v2tracing.NewHandler(
				cfg.Pool,
				v2tracing.ConfigFromEnv("elitea-main"),
				func() trace.Tracer { return otel.Tracer("elitea-main/tracing") },
			)
			r.Mount("/tracing", tracingHandler.Routes(requireTracingAdminStatus))

			// Artifacts are mounted by mountArtifactRoutes below, outside
			// this /api/v2 group — see S11: this group's JSON compression
			// would buffer and encode a downloaded object rather than
			// streaming it (S12).

			// === Context Manager ===
			//
			// Six routes that shipped with NO gate of any kind. Each one names
			// {projectID} and then builds that project's schema from the segment
			// — internal/api/v2/contextmgr/handler.go:121 does
			// `fmt.Sprintf("p_%s", projectID)` and reads chat_message_group from
			// it — so any authenticated caller could read and write any
			// project's conversation summaries by editing one path segment. That
			// is the #302 hole in a group #302 did not reach.
			//
			// THE LEGACY MATRIX HAS NO ENTRY FOR THIS SURFACE, and this file
			// says so rather than implying a transcription. There is no
			// context_manager module in pylon at all:
			// testdata/legacy/legacy-rbac-static-catalog.json lists none, guarded
			// or unguarded. So there is no `check_api` declaration to copy and no
			// legacy role split to read.
			//
			// THE SPLIT THIS FILE PROPOSES, and why. Every route acts on ONE
			// conversation, named by {conversationID}, and the data is derived
			// from that conversation's own messages. So each route takes the
			// permission the conversation itself takes:
			//
			//   the two reads  → models.chat.conversation.details
			//   the four writes → models.chat.conversation.edit
			//
			// This is the rule this router already applies to the other Go-only
			// routes that act on a conversation and have no pylon module:
			// `attachment_storage` and `context_strategy` both take
			// `models.chat.conversation.edit` for the same reason, and their note
			// states it. Reusing the conversation strings also keeps the surface
			// GRANTABLE: 0068 seeds both in DEFAULT mode with the legacy matrix
			// split — `details` to admin, editor and viewer, `edit` to admin and
			// editor. A name invented for this surface would be granted by
			// nothing and would answer 403 to every caller (#354, #359).
			//
			// A VIEWER KEEPS THE READS AND LOSES THE WRITES. That is the same
			// line the chat screen already draws: a viewer may open a
			// conversation and may not edit it. `optimize_context` is a write by
			// that rule even though its handler is a stub today, because the
			// name states what the route is for and a gate must match the route,
			// not the current body.
			requireConversationContextRead := projectPermission("models.chat.conversation.details")
			requireConversationContextWrite := projectPermission("models.chat.conversation.edit")
			// === Pipeline triggers and schedules (issues 192, 193) ===
			//
			// The SETTINGS half of both. These six ARE session-authenticated
			// and project-gated; only the inbound call mounted above the Auth
			// group is not.
			//
			// The gates are the pipeline VERSION's own read and update
			// permissions, so a person who may edit a pipeline may configure
			// how it starts and a person who may only look at one may only look
			// at this. No new permission is introduced, which is what keeps
			// this feature off the "a new grant needs a new shared migration
			// AND a manifest head bump" path.
			//
			// `reveal` is separated from the plain read and carries the WRITE
			// permission, because handing back a live credential is a different
			// act from reporting that one exists. See the package's
			// triggers.go.
			if cfg.PipelineTriggers != nil {
				pipelineTriggers := cfg.PipelineTriggers
				requirePipelineRead := projectPermission(v2pipelinetriggers.ReadPermission)
				requirePipelineWrite := projectPermission(v2pipelinetriggers.WritePermission)
				r.Route("/pipeline_triggers", func(r chi.Router) {
					r.With(requirePipelineRead).
						Get("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.GetTrigger)
					r.With(requirePipelineWrite).
						Get("/secret/prompt_lib/{projectID}/{versionID}", pipelineTriggers.RevealTrigger)
					r.With(requirePipelineWrite).
						Post("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.CreateOrRotateTrigger)
					r.With(requirePipelineWrite).
						Delete("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.RevokeTrigger)
				})
				r.Route("/pipeline_schedules", func(r chi.Router) {
					r.With(requirePipelineRead).
						Get("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.GetSchedule)
					r.With(requirePipelineWrite).
						Put("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.SaveSchedule)
					r.With(requirePipelineWrite).
						Delete("/prompt_lib/{projectID}/{versionID}", pipelineTriggers.DeleteSchedule)
				})
			}

			ctxMgrHandler := v2contextmgr.NewHandler(cfg.Pool)
			r.Route("/context_manager", func(r chi.Router) {
				r.With(requireConversationContextWrite).
					Post("/optimize_context/{projectID}/{conversationID}", ctxMgrHandler.OptimizeContext)
				r.With(requireConversationContextRead).
					Get("/analytics/{projectID}/{conversationID}", ctxMgrHandler.GetAnalytics)
				r.With(requireConversationContextRead).
					Get("/summaries/{projectID}/{conversationID}", ctxMgrHandler.ListSummaries)
				r.With(requireConversationContextWrite).
					Post("/summaries/{projectID}/{conversationID}", ctxMgrHandler.CreateSummary)
				r.With(requireConversationContextWrite).
					Put("/summary/{projectID}/{conversationID}/{summaryID}", ctxMgrHandler.UpdateSummary)
				r.With(requireConversationContextWrite).
					Delete("/summary/{projectID}/{conversationID}/{summaryID}", ctxMgrHandler.DeleteSummary)
			})

			// === Support Assistant ===
			//
			// The in-app support widget: config, the caller's own support
			// conversations, and one turn against the operator-chosen agent.
			// See internal/api/v2/supportassistant for why no route here takes a
			// {projectID} — the hidden support project is resolved from
			// centry.platform_config and never from the request — and why the
			// gates name `models.chat.*` rather than the reference's
			// `models.support_assistant.*` strings, which this catalogue has
			// never seeded.
			//
			// The subrouter carries its OWN gates (Handler.Routes), because the
			// project they resolve against is one the router does not know: it
			// comes out of the section this handler reads.
			{
				// Mounted UNCONDITIONALLY, including without a pool.
				//
				// The widget asks `GET /support_assistant/config` on every page
				// load and treats a non-answer as a failure, so the route has to
				// exist on every deployment. With no pool the handler reads no
				// configuration, resolves the assistant as disabled, and answers
				// that question truthfully — the same degrade the branch that
				// used to sit here achieved with a hardcoded stub, minus the
				// second implementation of one boolean.
				supportOptions := []v2support.Option{
					v2support.WithPermissionResolver(coreResolver),
				}
				if cfg.ConvsRepo != nil {
					supportOptions = append(supportOptions, v2support.WithChatStore(cfg.ConvsRepo))
				}
				if cfg.SupportAssistantStart != nil {
					supportOptions = append(supportOptions, v2support.WithStartUseCase(cfg.SupportAssistantStart))
				}
				// The SAME provisioner the project-create route uses. The
				// hidden support project is provisioned by the pipeline every
				// other project goes through — tenant schema, RBAC roles,
				// secrets vault, buckets — because a project assembled by a
				// second, simpler path is exactly the half-provisioned project
				// the support routes would then 500 against.
				if projectProvisionerOK {
					supportOptions = append(supportOptions,
						v2support.WithProvisioner(supportProjectProvisioner{projectProvisioner}))
				}
				// Attachments (issue #625 item 2): the SAME artifact path chat
				// itself uses, not a second store — `convHandler` (this
				// function's one instance, S20a-wired above) writes the bytes
				// exactly as `POST .../attachments/prompt_lib/{p}/{c}` does, and
				// `artifactHandler` (also this function's one instance) serves
				// them back exactly as the generic `GET .../artifacts/objects/
				// {p}/{bucket}/*` route does. Both are nil-guarded the same way
				// every other optional dependency here is: absent storage means
				// the support routes answer 503, not a panic.
				if convHandler != nil {
					supportOptions = append(supportOptions, v2support.WithAttachmentUploader(convHandler))
				}
				if artifactHandler != nil {
					supportOptions = append(supportOptions, v2support.WithAttachmentDownloader(artifactHandler))
				}
				r.Mount("/support_assistant", v2support.NewHandler(cfg.Pool, supportOptions...).Routes())
			}

			// === Webhooks ===
			//
			// Five routes over `webhooks WHERE project_id = $1`. The listing
			// returns the row's `secret` and the PUT rotates it, and until #496
			// the subrouter carried no gate, so any authenticated caller could
			// name any project id. The gates are inside the package — see that
			// Routes() header for the permission each route takes and why the
			// `configurations` strings are the proposal for a surface the legacy
			// matrix has no entry for.
			//
			// NOTE FOR A READER MEASURING THE IMPACT: no migration in this
			// repository creates a `webhooks` table, and nothing dispatches from
			// it. On a database built from this corpus the routes answer the
			// repository's error rather than another project's rows. The gate is
			// still the fix: the disclosure is one CREATE TABLE away, and the
			// route must not be the thing that decides.
			if cfg.WebhookRepo != nil {
				r.Mount("/webhooks/prompt_lib/{projectID}", webhook.NewHandler(
					cfg.WebhookRepo,
					webhook.WithPermissionResolver(coreResolver),
				).Routes())
			}

			// === Events (SSE) ===
			//
			// The project event stream. Until #496 it carried no gate, so any
			// authenticated caller could subscribe to
			// events.ProjectChannel({projectID}) for any tenant. Both transport
			// arms take the same resolver: two registrations of one surface must
			// not carry two authorization contracts, and #152 records what
			// happens when the two arms of this exact fallback are allowed to
			// drift.
			if cfg.EventSource != nil {
				r.Mount("/events/prompt_lib/{projectID}", v2events.NewHandlerFromSource(
					cfg.EventSource,
					v2events.WithPermissionResolver(coreResolver),
				).Routes())
			} else if cfg.RedisClient != nil {
				r.Mount("/events/prompt_lib/{projectID}", v2events.NewHandler(
					cfg.RedisClient,
					v2events.WithPermissionResolver(coreResolver),
				).Routes())
			}
		})
	})

	// /llm has one composed backend: GatewayProxy, the mTLS reverse proxy to
	// elitea-llm-gateway-svc (LLM_GATEWAY_URL). The gateway is the only LLM
	// data plane — it pulls per-project credentials and model definitions from
	// p_{projectID}.configuration itself, so there is no second proxy for Main
	// to compose and no LiteLLM facade to fall back to (that facade, its
	// administration client and its ELITEA_LITELLM_* env surface were deleted;
	// nothing may reintroduce a fallback here).
	//
	// LLMProxy survives as a deliberately unwired seam: it is a plain
	// http.Handler with no composition site in cmd/elitea-main, declared
	// optional-by-design in router_nil_gate_test.go, and exists so an
	// alternative LLM backend can be mounted with the same Auth+Project
	// middleware without reopening this routing decision. It is NOT the old
	// facade. Gateway wins when both are set: it is the migration target, and
	// preferring anything else over it would be a silent downgrade.
	//
	// The gateway arm is mounted HERE, not in production_router.go's
	// mountReviewedProductionRoutes: NewRouter always builds this router
	// (#243 deleted the only other build path, which was unreachable in
	// every real deployment), so mounting it in exactly one place is what
	// matters now — mountReviewedProductionRoutes explicitly defers to this
	// registration rather than mounting /llm itself.
	//
	// Each backend keeps its own literal per-field nil gate rather than being
	// collapsed into one nil-check over a local. TestNilGatedRouterFieldsAreWiredOrDeclared
	// reads this file as SOURCE to prove no route group is gated behind a field
	// the composition root never assigns, so a gate hidden behind a local
	// variable is invisible to it — which is the precise failure mode it exists
	// to catch.
	mountLLM := func(proxy http.Handler, resolver apimw.PersonalProjectResolver) {
		r.Group(func(r chi.Router) {
			r.Use(apimw.Auth(apimw.AuthConfig{
						Validator:                  cfg.AuthValidator,
				PrincipalValidator:         cfg.PrincipalValidator,
				ForwardedIdentityVerifier:  cfg.Auth.ForwardedIdentityVerifier,
				SessionSecret:              cfg.SessionSecret,
				SessionStore:               cfg.Auth.SessionStore,
				RejectLegacySessionCookies: cfg.Auth.RejectLegacySessionCookies,
			}))
			// Membership admits the caller-supplied project selector header
			// (issue #318). Without it the edge admits no selector that names
			// another project, so /llm keeps billing the caller's own project
			// and never bills a project on an unchecked header.
			r.Use(apimw.Project(apimw.ProjectConfig{
				Resolver:   resolver,
				Membership: apimw.NewProjectMembership(cfg.Pool),
			}))
			r.Mount("/llm", proxy)
		})
	}
	// The /llm resolver ASKS for the personal project it could not find.
	//
	// `/social/author` is the SPA's reader of that value; this is the other
	// one, and it is the reader a PAT-only caller reaches — the SDK, a
	// scheduled job, a scripted client. Hooking only the SPA's endpoint left
	// those callers with no personal project and `project_not_resolved` on
	// every /llm request, permanently. The concrete type is asserted rather
	// than the interface widened: only the database-backed resolver can be
	// given an ensurer, and a composition that supplied any other one keeps
	// exactly the resolver it supplied.
	withPersonalProjects := func(resolver apimw.PersonalProjectResolver) apimw.PersonalProjectResolver {
		if personalProjects == nil {
			return resolver
		}
		if database, ok := resolver.(*apimw.DBPersonalProjectResolver); ok {
			return database.WithPersonalProjectEnsurer(personalProjects)
		}
		return resolver
	}
	if cfg.GatewayProxy != nil {
		mountLLM(cfg.GatewayProxy, withPersonalProjects(cfg.GatewayProjectResolver))
	} else if cfg.LLMProxy != nil {
		mountLLM(cfg.LLMProxy, withPersonalProjects(cfg.LLMProjectResolver))
	} else {
		// Issue #463: NO backend is composed, because LLM_GATEWAY_URL is empty.
		//
		// This arm used to be absent. The path then stayed unregistered and
		// answered 404, and the comment in production_router.go called that
		// "visible". It is not visible. A 404 on /llm/v1/models is the same
		// answer the caller gets for a misspelt path, so an operator who
		// forgot the variable and a user who typed the URL wrongly read one
		// response and cannot tell the two apart. The chart's own default for
		// LLM_GATEWAY_URL is the empty value, so this was the DEFAULT state of
		// a Kubernetes install: the gateway pods ran and nothing routed to
		// them, silently.
		//
		// 503 with a named code is the same refusal, said out loud. It names
		// the variable that turns the path on, so the answer itself is the
		// remedy. It does NOT route anywhere: there is still exactly one
		// composed backend for /llm, and nothing here reintroduces a fallback
		// to a superseded data plane.
		// Mount, not Handle, and on the same "/llm" pattern the composed arm
		// uses. The route-surface snapshot in production_router_test.go and the
		// spec-conformance suite both classify a /llm MOUNT as router plumbing
		// and exclude it; two r.Handle registrations would instead appear as
		// nine new API routes each and change a surface that has not changed.
		r.Mount("/llm", llmNotConfiguredHandler())
	}

	// Register reviewed current-compatible routes last. Some broad prototype
	// repositories own the same path with a partial or older method set (for
	// example conversations owns GET/DELETE messages and a legacy regenerate
	// POST). The reviewed handlers must remain authoritative whenever those
	// repositories are also composed; otherwise merely adding an unrelated
	// compatibility repository can silently remove agent execution or SSE.
	// The broad prototype compatibility handler above already owns the current
	// project-context GET. Keep that single live registration while adding the
	// reviewed routes it does not provide, including chat config and agent SSE.
	mountReviewedProductionRoutes(r, cfg)

	return r
}

// LLMNotConfiguredCode is the machine-readable code the /llm path answers with
// when no gateway backend is composed (issue #463). It is a DISTINCT code from
// the proxy's own upstream_unavailable: that one means "the gateway is
// configured and did not answer", this one means "no gateway is configured at
// all". An operator who cannot tell those apart cannot tell a broken deployment
// from an unconfigured one.
const LLMNotConfiguredCode = "llm_gateway_not_configured"

// llmNotConfiguredHandler answers every /llm request while LLM_GATEWAY_URL is
// empty.
//
// The body mirrors the shape internal/llmproxy already writes on an upstream
// error, so a client parses one envelope for both. 503 rather than 404: the
// path exists and the capability is off, which is a server state, and a client
// may retry after the operator sets the variable.
//
// The message names the environment variable on purpose. This response is read
// by the person who has to fix it far more often than by an end user, and the
// alternative is a log line in a pod they have not thought to look at.
func llmNotConfiguredHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"the LLM gateway is not configured: LLM_GATEWAY_URL is empty, so no /llm backend is composed","type":"service_unavailable","code":"` + LLMNotConfiguredCode + `"}}`))
	})
}

func mountRuntimeRoutes(router chi.Router, routes RuntimeRoutes) {
	// A partial composition is not exposed. Both public runtime paths share the
	// same authenticated-principal provenance and strict PostgreSQL membership
	// authorizer, so mounting only one is a startup wiring error and fails closed.
	if routes.Validation == nil || routes.ExecutionEvents == nil {
		return
	}
	router.Method(http.MethodPost, runtimeValidationPath, routes.Validation)
	router.Method(http.MethodGet, runtimeEventsPath, routes.ExecutionEvents)
}

// configurationSecretSealer builds the project-vault writer the compatibility
// configurations router seals provider credentials into.
//
// A nil result REFUSES every write that carries a schema-declared password
// field. That is the required behaviour. The alternative is the defect this
// function exists to close: the handler used to store the submitted `api_key`
// verbatim in p_{project}.configuration, and migrations/shared/0072 grants the
// read of that column to the project VIEWER role.
//
// cmd/elitea-main validates SECRETS_MASTER_KEY at start-up and stops on a
// malformed value, so the error branch here is the programmatic caller's case,
// not an operator's. An ABSENT variable is a supported state: the project key
// is then stored unwrapped, and every path here reads it that way.
//
// The sealer also carries the vault creator, because a project can hold rows
// and no vault. migrations/001_initial.sql inserts centry.project id 1 and
// creates p_1 without calling the provisioner, so the default project of a
// fresh install has no vault rows at all. Without the creator the first
// credential save into that project answers 503, and the deployment can store
// no provider credential.
//
// v2secrets.NewHandler is the creator because it is the ONE minter of a vault
// key (#399). It reads the same SECRETS_MASTER_KEY this function reads, so the
// creator and this writer share one key source.
func configurationSecretSealer(pool *pgxpool.Pool) v2configs.SecretSealer {
	if pool == nil {
		return nil
	}
	// MasterKeyFromEnv is the VALIDATOR here, not the source. It returns the
	// DECODED 32 key bytes, and this repository takes the ENCODED 44-byte
	// form — the same form loadOptionalFernetMasterKey reads out of
	// ELITEA_VAULT_MASTER_KEY_FILE, and the same form centrysecrets decodes.
	// Handing it the decoded bytes made NewCurrentSecretVaultRepository reject
	// the key, which returned a nil sealer, which refused EVERY credential save
	// with 503. deploy/docker-compose.yml REQUIRES the variable (`:?`), so that
	// is the default deployment.
	decoded, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	clear(decoded)
	masterKey := []byte(os.Getenv(v2secrets.MasterKeyEnvVar))
	sealer, err := dbrepos.NewCurrentSecretVaultRepository(pool, masterKey,
		dbrepos.WithProjectVaultCreator(v2secrets.NewHandler(pool)))
	clear(masterKey)
	if err != nil || sealer == nil {
		return nil
	}
	return sealer
}
