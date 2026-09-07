// Package supportassistant serves the IN-APP SUPPORT ASSISTANT — the floating
// widget every authenticated user can open to ask the platform's own support
// agent a question, with the page they are on attached as context.
//
// It is the Go port of `legacy/plugins/support_assistant/` (module.py, its five
// `api/v2/*.py` resources, `sio/support.py` and `rpc/support.py`).
//
// # What this package is
//
// A THIN FACADE over the chat stack this service already has, scoped to one
// hidden project. Nothing here re-implements conversations, message groups,
// attachments or agent execution; every route resolves the support project,
// proves the caller may act in it, and then does what the chat routes do. The
// reference makes the same choice — its resources are RPC calls into
// `elitea_core`'s `chat_*` handlers, not a second chat implementation — and it
// is the only choice that keeps one transcript format, one attachment path and
// one execution pipeline.
//
// # The three things the facade adds
//
//  1. THE PROJECT IS RESOLVED SERVER-SIDE. No support route takes a `{projectID}`
//     path segment. The id comes from `centry.platform_config`, never from the
//     caller, which is what makes it impossible to aim a support route at
//     somebody else's tenant. This replaces the reference's
//     `@add_support_project_id` decorator.
//
//  2. USERS ARE ENROLLED LAZILY. The support project is shared by everyone on
//     the platform, so a first-time caller holds no role in it and would be
//     refused. `ensureEnrolled` grants `viewer` on first use, exactly as
//     `module.ensure_user_enrolled` does.
//
//  3. CONVERSATIONS ARE SCOPED TO THEIR AUTHOR. Because the project is SHARED,
//     the generic conversation listing would show every user on the platform
//     every other user's support transcripts. Every statement in store.go is
//     therefore predicated on `author_id = <caller>` AND `source = 'support'`,
//     and that predicate is in ONE place so a new route cannot forget it.
//
// # Authorization: granted names, not new ones
//
// The routes are gated on the `models.chat.*` permissions, resolved in DEFAULT
// mode against the RESOLVED support project. That is deliberate and is the
// lesson of #354/#359: a permission string nothing grants answers 403 to
// everybody. Migration 0068 (and 0070 for `messages.create`) seeds exactly this
// set to `admin`, `editor` AND `viewer` in default mode, so the `viewer` role
// this package enrols people into already holds every permission its routes ask
// for, and no new permission migration is needed. The reference's
// `models.support_assistant.*` strings are NOT transcribed: this platform's
// catalogue has never carried them, so naming them would gate the whole feature
// on grants that do not exist.
//
// # What is NOT ported, and why
//
//   - `PUT /support_assistant/config` (api/v2/config.py's `put`). The reference
//     needs it because a Pylon plugin owns its own YAML; here the admin Features
//     page already writes this section into `centry.platform_config` through
//     `/api/v2/admin/…/config_values`, with schema validation and an audited
//     permission. A second writer over the same rows is a drift hazard with
//     nothing to gain — the page and this package would have to agree forever
//     about field names, types and defaults. The Features page is the single
//     writer; this package only reads.
//
//   - The socket.io surface (`sio/support.py`'s `support_predict`). This service
//     streams over SSE — see `internal/api/v2/executions` and the #93 chat
//     transport port — so `POST /support_assistant/predict/{uuid}` does what the
//     socket handler did (resolve the agent, attach it as a participant, start
//     the run) and answers with the same `events_url` the chat surface already
//     knows how to subscribe to.
package supportassistant

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// unavailableMessage is the single sentence every refusal in this package uses
// when the assistant is off, unbootstrapped or unconfigured. It is one constant
// so the three causes cannot be told apart from outside: which of them applies
// is operator configuration, and a client has the same job in all three cases —
// render nothing.
const unavailableMessage = "support assistant is not available"

// Permission strings. Every one is seeded by migrations/shared/0068 (and 0070
// for messages.create) to admin, editor and viewer in DEFAULT mode — see the
// package comment for why reusing them rather than inventing
// `models.support_assistant.*` is the whole authorization design.
const (
	PermissionConversationsList   = "models.chat.conversations.list"
	PermissionConversationsCreate = "models.chat.conversations.create"
	PermissionConversationRead    = "models.chat.conversation.details"
	PermissionMessagesCreate      = "models.chat.messages.create"
	// PermissionAttachmentsCreate gates the upload route (attachments.go).
	// Same string the main chat route's Post .../attachments/... uses
	// (router.go), granted by the SAME 0068 block as every permission above.
	PermissionAttachmentsCreate = "models.chat.attachments.create"
	// PermissionArtifactsView gates the read-back route (attachments.go).
	// NOT a `models.chat.*` string — attachment bytes are served by
	// internal/api/v2/artifacts' generic object-download route, gated on
	// its own catalogue (migrations/shared/0074_artifact_permissions.sql),
	// which grants this same string to admin, editor AND viewer too, so
	// reusing it needs no new grant either.
	PermissionArtifactsView = "configuration.artifacts.artifacts.view"
)

// StartUseCase is the agent-execution entry point the predict route delegates
// to. It is the same use case `internal/api/v2/agentexecution` drives, narrowed
// to the one method this package calls: a support turn is always an APPLICATION
// start against a configured agent, never an ad-hoc one against caller-supplied
// LLM settings.
type StartUseCase interface {
	StartCurrentApplication(
		context.Context,
		agentexecutionapp.CurrentApplicationStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

// Handler serves the support assistant surface.
type Handler struct {
	store                *store
	resolver             auth.PermissionResolver
	startCase            StartUseCase
	chat                 ChatStore
	attachmentUploader   AttachmentUploader
	attachmentDownloader AttachmentDownloader
	logger               *slog.Logger
}

// Option configures a Handler at construction.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver the route gates use. WITHOUT IT
// EVERY GATED ROUTE REFUSES: `RequireResolvedPermissionsForProject` answers 403
// on a nil resolver, which is the fail-closed direction and the one #301/#314
// record as the cost of getting this wrong the other way.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.resolver = resolver }
}

// WithStartUseCase supplies the agent-execution use case. Without it the
// predict route answers 503 rather than 500 — a deployment that has not wired
// agent execution cannot run a support turn, and saying so is more useful than
// a nil dereference.
func WithStartUseCase(useCase StartUseCase) Option {
	return func(h *Handler) { h.startCase = useCase }
}

// WithProvisioner supplies the project provisioner used to bootstrap the hidden
// support project on first use. Without it the assistant reports itself
// disabled until an operator names an existing project id on the Features page
// — it never silently serves conversations from a project it did not create.
func WithProvisioner(provisioner Provisioner) Option {
	return func(h *Handler) { h.store.provisioner = provisioner }
}

// WithLogger supplies the logger. Defaults to slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(h *Handler) {
		if logger != nil {
			h.logger = logger
		}
	}
}

// NewHandler builds the handler over a database pool.
func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	handler := &Handler{
		store:  &store{pool: pool},
		logger: slog.Default(),
	}
	for _, option := range options {
		option(handler)
	}
	handler.store.logger = handler.logger
	return handler
}

// Routes returns the subrouter mounted at /api/v2/support_assistant.
//
// The paths are the ones `@eliteaai/elitea-assistant`'s default adapter builds
// (`src/api/adapter.api.ts`: `${baseURL}/config/`, `/conversations/`,
// `/conversation/{id}`, `/attachments/{id}`) so the ported widget speaks the
// same wire the package was written against. Each is registered with AND
// without its trailing slash: the reference's Flask resources answer both, and
// a client that drops one should not get a 404 for a route that exists.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	// The config read is authenticated-only, with NO project gate, and that is
	// the one asymmetry in this file.
	//
	// It has to answer BEFORE the caller can be a member of anything: the widget
	// asks "am I enabled?" on every page load, including on a deployment where
	// the support project has not been bootstrapped and there is therefore no
	// project to hold a permission in. Gating it would answer 403 to the very
	// question "is this feature off?", which every client would then have to
	// interpret as "off" anyway. It discloses nothing a member could not see: a
	// switch, three display strings and the caller's OWN identity.
	r.Get("/config", h.Config)
	r.Get("/config/", h.Config)

	gated := func(permission string, handler http.HandlerFunc) http.Handler {
		return h.requireSupportProject(permission)(http.HandlerFunc(handler))
	}

	r.Method(http.MethodGet, "/conversations", gated(PermissionConversationsList, h.ListConversations))
	r.Method(http.MethodGet, "/conversations/", gated(PermissionConversationsList, h.ListConversations))
	r.Method(http.MethodPost, "/conversations", gated(PermissionConversationsCreate, h.CreateConversation))
	r.Method(http.MethodPost, "/conversations/", gated(PermissionConversationsCreate, h.CreateConversation))

	r.Method(http.MethodGet, "/conversation/{conversationUUID}", gated(PermissionConversationRead, h.GetConversation))
	r.Method(http.MethodPost, "/predict/{conversationUUID}", gated(PermissionMessagesCreate, h.Predict))

	// Attachments (issue #625 item 2) — see attachments.go for why these two
	// delegate to the SAME handlers the main chat surface's own attachment
	// routes use, rather than a second read/write path.
	r.Method(http.MethodPost, "/attachments/{conversationID}", gated(PermissionAttachmentsCreate, h.UploadAttachment))
	r.Method(http.MethodGet, "/attachments/{bucket}/*", gated(PermissionArtifactsView, h.GetAttachment))

	return r
}

// requireSupportProject is this package's `@add_support_project_id`.
//
// In order, it: resolves the section, refuses unless the assistant is READY
// (enabled, bootstrapped and pointed at an agent), enrols the caller, publishes
// the resolved settings on the request context, and only then applies the
// permission gate — resolved against the project it just resolved, never
// against anything in the request.
//
// THE ORDER MATTERS. Enrolment has to precede the gate, because the gate is the
// thing that would refuse an unenrolled caller; that is the same order the
// reference's decorator uses, and inverting it makes every user's first support
// request a 403 that clears itself on retry.
func (h *Handler) requireSupportProject(permission string) func(http.Handler) http.Handler {
	gate := apimw.RequireResolvedPermissionsForProject(
		h.resolver,
		auth.PermissionModeDefault,
		projectIDFromContext,
		permission,
	)
	return func(next http.Handler) http.Handler {
		// Read the composition inside-out: `resolve` runs FIRST and hands the
		// request to `gate`, which hands it to `next`. Building it the other way
		// round — gate outermost — is the bug this comment exists to prevent:
		// the extractor would find no project on the context and the gate would
		// refuse every support request on every deployment.
		return h.resolve(gate(next))
	}
}

// resolve loads the section, refuses when the assistant cannot serve, enrols the
// caller and publishes the settings on the request context.
func (h *Handler) resolve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok {
			apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
			return
		}
		userID, ok := user.OwningUserID()
		if !ok {
			apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
			return
		}

		settings, err := h.store.settings(r.Context())
		if err != nil {
			h.logger.Error("support assistant: resolve settings", "err", err)
			apierr.WriteStatus(w, http.StatusServiceUnavailable, unavailableMessage)
			return
		}
		if !settings.Ready() {
			// 503, not 404 and not 403. The reference answers 503 here too, and
			// it is the accurate status: the route exists and the caller may use
			// it, but the platform has not been configured to serve it yet.
			apierr.WriteStatus(w, http.StatusServiceUnavailable, unavailableMessage)
			return
		}

		if err := h.store.ensureEnrolled(r.Context(), settings.ProjectID, userID); err != nil {
			// Enrolment failure is LOGGED AND SURVIVED, matching
			// `module.ensure_user_enrolled`, which returns True on exception.
			// A caller who already holds a role — every returning user — is
			// unaffected by a transient write failure, and the gate below is
			// what actually decides. Refusing here would turn one failed INSERT
			// into a support outage for people who need no INSERT at all.
			h.logger.Warn("support assistant: enrolment failed",
				"project_id", settings.ProjectID, "user_id", userID, "err", err)
		}

		next.ServeHTTP(w, r.WithContext(withSettings(r.Context(), settings)))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("support assistant: write response", "err", err)
	}
}
