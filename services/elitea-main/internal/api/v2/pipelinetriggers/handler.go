// Package pipelinetriggers is the two unattended ways to start a pipeline:
// an inbound signed trigger (issue 192) and a cron schedule (issue 193).
//
// Legacy had both — `elitea_core/api/v2/pipeline_trigger.py` and
// `elitea_core/rpc/pipeline_scheduling.py`. The Go stack had neither. The three
// `/pipeline_trigger/` routes that used to exist here were the API surface of
// the legacy subsystem, they were gated behind a nil `PipelineRunner`, they
// 404ed in every deployment, and they were deleted with issue 126. This package
// is the capability, rebuilt on the runtime this service actually has.
//
// # THIS IS AN ENTRY POINT, NOT AN EXECUTION ENGINE
//
// Running a pipeline is already solved: the Python worker runs one through the
// agent-execution path and emits `pipeline_finish`. So both halves below end in
// the SAME call the chat composer makes — `StartCurrentApplication` — over the
// SAME conversation-and-participant shape the MCP server's `tools/call` builds
// (internal/api/v2/mcp/execute.go, whose decision record applies here verbatim
// and is not repeated). One admission path, one set of budget, governance and
// trace attribution, one transcript a person can open afterwards.
//
// That is also why `run.go` is shared rather than duplicated per entry point.
// A second assembler for one contract is the failure the support assistant
// refused to build and the failure MCP refused to build; a third refusal is
// this one.
//
// # THE AUTH STORY, WHICH IS THE WHOLE RISK
//
// The inbound trigger is the one shape in this service where getting
// authorization wrong is worst: an entry point into EXECUTION that is
// unauthenticated by session and authenticated by a credential somebody else
// holds. Issue 11 in this repository was exactly that class — a guard that did
// not cover the credential-backed path, so an unverified header selected any
// tenant's data. Four rules answer it, and each has a test that fails if it is
// removed:
//
//  1. NOTHING THE CALLER SENDS SELECTS A TENANT. The project id is a path
//     segment, but it only says WHERE TO LOOK. What is run comes out of the
//     stored row: `application_id`, `version_id` and `created_by` are read from
//     the database and never from the request. A caller who guesses another
//     project's id finds no row with their token's hash in it and gets the same
//     401 as a caller who guessed nothing. There is no header, body field or
//     query parameter this package reads to decide whose pipeline runs.
//
//  2. THE SECRET IS COMPARED IN CONSTANT TIME, against a SHA-256 stored beside
//     the row. `subtle.ConstantTimeCompare` on two 32-byte digests, never
//     `==` on the secret and never a vault read on this path. Hashing first is
//     what makes the comparison fixed-width, which is what makes it constant
//     time at all.
//
//  3. EVERY REFUSAL IS THE SAME REFUSAL. Unknown token, wrong secret, revoked
//     trigger, wrong project, deleted version — one status, one body, no
//     timing branch before the compare. A refusal that says WHICH of those it
//     was is an oracle for enumerating a deployment's pipelines.
//
//  4. A TRIGGER GRANTS NO MORE THAN ITS CREATOR HAS. The run executes as
//     `created_by`, and that user's project permission is RE-RESOLVED on every
//     inbound call. A token issued by someone who has since lost access stops
//     working, without anybody having to remember to revoke it. This is the
//     same rule the schedule half applies, for the same reason: a credential
//     that outlives its holder's access is a standing privilege escalation.
//
// # WHO A SCHEDULED RUN EXECUTES AS, AND THE OTHER TWO HARD QUESTIONS
//
// Issue 193 names three things that are hard about scheduled execution and are
// not the cron parsing. Answered here, on the record:
//
//   - WHO. The schedule's AUTHOR — `pipeline_schedules.author_id`, the user who
//     saved the schedule — re-validated at FIRE TIME, not at save time. A fire
//     resolves the author's permissions in the project exactly as an HTTP
//     request would and refuses if `models.chat.messages.create` is gone. There
//     is no platform principal and no implicit admin: a scheduled run can do
//     precisely what its author could do at the moment it fired.
//   - OVERLAP. SKIP. If the previous run of the same schedule has not settled,
//     the tick does not start a second one, does not stamp `last_run`, and
//     records `last_result = 'skipped_overlap'`. Queuing would let a pipeline
//     slower than its own cron build an unbounded backlog behind a schedule
//     nobody is watching, which is the failure mode a schedule is least able to
//     report.
//   - FAILURE VISIBILITY. Both, and they answer different questions. An audit
//     event answers "what happened on this platform" and is what an operator
//     reads. `pipeline_schedules.last_result` answers "is MY schedule working"
//     and is what the pipeline's own settings tab shows the person who made it.
//     An audit trail alone would leave the tenant with no answer at all.
//
// # WHERE THE SCHEDULE TICK RUNS, AND WHY NOT IN services/elitea-scheduler
//
// The recorded decision on issue 193 says scheduled pipelines "ride
// elitea-scheduler ... with the maintenance gate, and no catch-up storm on
// resume". The two BEHAVIOURS are honoured exactly, and are the reason this
// paragraph exists rather than a silent deviation on placement.
//
// elitea-scheduler has exactly one dispatch primitive: a Redis PUBLISH of an
// `rpc_func` name onto the arbiter channel. Its own code records that this
// channel has no consumer in a Go stack — "legacy Pylon is the only consumer of
// the arbiter wire format" (services/elitea-scheduler/internal/scheduler/
// scheduler.go, issue 305). Admitting a pipeline run means freezing a version
// snapshot and writing chat rows and the runtime outbox in ONE transaction,
// which is elitea-main's, and which has no remote entry point. So firing from
// that daemon would need a NEW service credential and a NEW inbound route whose
// only caller is the scheduler — a second door into execution, built for
// exactly the risk rule 1 above exists to close, and reachable by anyone who
// obtains one shared secret rather than one pipeline's.
//
// The tick therefore runs on elitea-main's own platform scheduler
// (internal/application/scheduling), which already leases occurrences across
// replicas and already carries one tenant-facing recurring job —
// `index.schedule.scan.v1`. That is the direction the platform has already
// taken: services/elitea-scheduler/RETIREMENT.md records that the daemon's
// `index_scheduling` row is DISABLED in the hybrid deployment in favour of
// elitea-main's own scan.
//
// What moved with it, so nothing is lost:
//
//   - THE MAINTENANCE GATE. `schedulerun.go` reads the same two
//     `centry.platform_config` coordinates elitea-scheduler reads
//     (`maintenance` / `maintenance_enabled`) and suppresses DISPATCH ONLY,
//     once per tick, failing PERMISSIVE. elitea-main's platform scheduler had
//     no such gate before this package; the tenant workload is the first job on
//     it for which a closed platform must mean "start nothing".
//   - NO CATCH-UP STORM. A suppressed or skipped tick does not stamp
//     `last_run`, and due-ness is `next(last_run) <= now`, so a schedule that
//     missed six slots is due exactly ONCE when the window lifts. Both rules
//     are elitea-scheduler's, quoted in maintenance.go, and both are pinned by
//     tests here.
package pipelinetriggers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// RunPermission is what a caller must hold in the project for a pipeline to
// start. It is `models.chat.messages.create` — the permission the chat composer
// itself requires — and not a new one, so neither entry point is a way around
// the gate on the interactive path. MCP's `tools/call` made the same choice for
// the same reason.
const RunPermission = "models.chat.messages.create"

// RunPermissionMode is the RBAC mode the run permission is resolved in.
const RunPermissionMode = auth.PermissionModeDefault

// ReadPermission and WritePermission gate the SETTINGS routes. They are the
// permissions the pipeline version's own read and update already use, so a
// person who may edit a pipeline may configure how it is started, and a person
// who may only look at one may only look at this too. No new permission is
// introduced, which is what keeps this package out of the shared RBAC corpus
// and off the "new grants need a new migration AND a manifest head bump" path.
const (
	ReadPermission  = "models.applications.version.details"
	WritePermission = "models.applications.version.update"
)

// AgentStartUseCase is the one execution entry point this package uses. It is
// deliberately the narrowest possible slice of the chat start use case: this
// package admits turns and never regenerates, continues or cancels one.
type AgentStartUseCase interface {
	StartCurrentApplication(
		context.Context,
		agentexecutionapp.CurrentApplicationStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

// HiddenVault is the global vault's HIDDEN bucket, as
// internal/api/v2/secrets/admin_hidden.go exposes it.
//
// The plaintext trigger secret lives there and nowhere else. The bucket is
// excluded from the `{{secret.<name>}}` merge every project workload resolves
// against, so a running agent cannot read another pipeline's trigger secret by
// interpolating a guessable name — which the regular bucket would permit,
// because the names here are derived from ids that appear in URLs.
type HiddenVault interface {
	StoreAdminHiddenSecret(ctx context.Context, name, value string) error
	LookupAdminHiddenSecret(ctx context.Context, name string) (string, error)
	DeleteAdminHiddenSecret(ctx context.Context, name string) error
}

// Handler serves the settings routes and the inbound trigger.
type Handler struct {
	pool        *pgxpool.Pool
	start       AgentStartUseCase
	vault       HiddenVault
	permissions auth.PermissionResolver
	recorder    audit.Recorder
	logger      *slog.Logger
}

// Option configures a Handler.
type Option func(*Handler)

// WithAgentStart supplies the execution use case.
//
// Without it the inbound trigger and every scheduled fire refuse with 503 and
// say so. That is the honest degrade: a deployment with `runtime.enabled` off
// has no way to run a pipeline, and answering 202 with an execution id it never
// created would be the "route answers 200, nothing behind it" defect this
// repository keeps rediscovering.
func WithAgentStart(start AgentStartUseCase) Option {
	return func(h *Handler) { h.start = start }
}

// WithVault supplies the hidden-bucket vault. Without it a trigger cannot be
// created or revealed; the settings read and the schedule half still work.
func WithVault(vault HiddenVault) Option { return func(h *Handler) { h.vault = vault } }

// WithPermissions supplies the resolver used to RE-VALIDATE the trigger
// creator's and the schedule author's access at fire time.
//
// Without it every unattended run refuses. FAIL CLOSED: a handler that cannot
// decide must not decide "allowed" for the one capability here that spends
// money and drives tools. MCP's run path makes the same choice.
func WithPermissions(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissions = resolver }
}

// WithAuditRecorder supplies the `centry.audit_events` writer.
//
// The inbound trigger is mounted ABOVE the Auth group, so the audit MIDDLEWARE
// never sees it — the middleware is inside that group by design, because a row
// built above authentication has no principal to file itself under. This
// package therefore writes its own row rather than inheriting one, and issue
// 192 asks for exactly that.
func WithAuditRecorder(recorder audit.Recorder) Option {
	return func(h *Handler) { h.recorder = recorder }
}

// WithLogger supplies the logger. A nil logger is replaced by the default.
func WithLogger(logger *slog.Logger) Option {
	return func(h *Handler) {
		if logger != nil {
			h.logger = logger
		}
	}
}

// NewHandler builds the handler. A nil pool is permitted: every route then
// answers its own "not available on this deployment" refusal instead of
// panicking, which is what keeps a composition without a database mountable.
func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	handler := &Handler{pool: pool, logger: slog.Default()}
	for _, option := range options {
		option(handler)
	}
	return handler
}

func (h *Handler) log() *slog.Logger {
	if h == nil || h.logger == nil {
		return slog.Default()
	}
	return h.logger
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}
