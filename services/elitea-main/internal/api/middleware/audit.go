package middleware

// The audit-trail EMITTER for `centry.audit_events` (see internal/audit for the
// table, the reader contract and the vocabulary).
//
// ── DECISION 1: WHERE the emission happens ────────────────────────────────
//
// Middleware, with a per-request annotation escape hatch for domain meaning.
//
// The trade-off is real: middleware catches everything cheaply but records HTTP
// shape rather than what the request MEANT, while explicit call sites record
// meaning and are forgotten on every new route. What settles it here is what
// the READERS display.
//
// The Audit Trail page's own columns are timestamp, user, event type, action,
// METHOD, ROUTE, STATUS and DURATION. Its heatmap axes are time and duration
// band. Its "user" tab filters `event_type = api`, which in the legacy
// implementation was produced by exactly this level: a Flask middleware span,
// extracted by `_extract_api` in
// legacy/plugins/tracing/utils/audit_processor.py, whose `action` is the span
// name `"METHOD /route"`. So HTTP shape is not an approximation of what this UI
// wants — for `event_type = api` it IS what this UI wants, and a set of
// hand-placed domain calls would have to reconstruct method, route, status and
// duration at every site to fill the same columns.
//
// What middleware cannot know is the entity: which user was suspended, which
// secret was written. That is what `audit.Annotate` is for. A handler adds
// entity type/id/name (and may override the action or event type); a handler
// that does not degrades its row from "PUT …/user_suspend/{mode}/{userID} on
// user 42" to "PUT …/user_suspend/{mode}/{userID}". It never degrades to NO
// ROW, which is the failure mode this whole change exists to remove and the one
// a call-site-only design reintroduces on every route somebody forgets.
//
// ── DECISION 2: WHAT is audited ───────────────────────────────────────────
//
// THE RULE: a request is audited when it is directed at an ADMINISTRATIVE or
// SECURITY-RELEVANT surface (auditedSurfaces below) AND it either changes state
// (POST/PUT/PATCH/DELETE) or is REFUSED for lack of authority (401/403).
//
// Both halves are load-bearing. Auditing everything makes a table that grows
// without bound and a page nobody can read — the reference implementation
// skipped GETs by default for the same reason (`audit_all_methods = False`).
// Auditing only mutations would drop the security question an audit trail is
// most often opened to answer: who tried to reach something they could not.
//
// The surface list is a PATH PREFIX list, checked before routing, so a new
// route under an already-audited prefix (a new /api/v2/admin/… endpoint) is
// audited the day it is added, without anyone remembering. That is the
// "forgotten on new routes" failure, closed by construction for the surfaces
// that matter.
//
// Deliberately NOT audited, and why:
//   - Content CRUD — agents, prompts, tools, icons, pins, likes, feedback,
//     notifications, context summaries. High volume, no security question, and
//     each one already has its own versioned record in its own table.
//   - /llm and the model call path. Legacy skipped it too (`/llm/` is in
//     `_DEFAULT_SKIP_PATHS`) because it is already recorded, per request, by
//     the gateway's own request log (shared migration 0099). A second ledger
//     here would double-count the analytics reads that join both.
//   - /api/v2/tracing/* — the telemetry INGEST. Auditing the audit intake is a
//     loop: each ingest call would write a row, and a busy tracer would fill
//     the trail with nothing but its own arrival.
//   - Health, metrics, static assets and the SPA shell: no principal, no state.
//   - GETs that succeed, everywhere. A read that was allowed is the highest
//     volume event on the platform and the lowest information one.
//
// ── DECISION 3: FAILURE POLICY ────────────────────────────────────────────
//
// Never fail the request; never vanish silently. `audit.Recorder.Record` cannot
// block and cannot return an error, so nothing here can change the outcome or
// the latency of the request it describes. Drops are counted and logged on a
// bounded cadence. The full argument, including the alternative that was
// rejected, is on `audit.PostgresRecorder`.
//
// ── DECISION 4: RETENTION ─────────────────────────────────────────────────
//
// This adds growth that this middleware does not bound, and it is bounded
// elsewhere — issue #619 built the sweeper this paragraph used to ask for.
// `services/elitea-scheduler/internal/auditretention` runs bounded, batched
// `DELETE FROM centry.audit_events WHERE timestamp < $cutoff` passes on a timer,
// with the window set by `AUDIT_RETENTION_DAYS` (365 days by default) and the
// sweep suppressed while the platform is in a maintenance window.
//
// Nothing on this path changed, and nothing on it should: the bound belongs to
// a daemon, not to a request. Two properties of the sweeper matter to a reader
// of this file. It only DELETES — the middleware remains the single writer of
// what an audit row means — and it deletes by `timestamp` alone, so no
// annotation, entity or status code here can put a row outside its reach.
//
// Sized against the policy above — administrative and security-relevant
// mutations plus refusals, not general traffic — the arrival rate is single
// digits per second at platform scale. If the sweep ever cannot keep up (it
// logs at WARN when a pass ends at its batch ceiling), the next step is monthly
// RANGE partitioning on `timestamp`, which is deliberately not done
// pre-emptively.
//
// ── PERFORMANCE ───────────────────────────────────────────────────────────
//
// A request outside auditedSurfaces pays one prefix scan over a 10-entry list
// and nothing else: no wrapper, no allocation, no context value. A request
// inside pays one ResponseWriter wrapper (the same `statusRecorder` the OTel
// middleware already installs), one context value, one 24-byte random read and
// one non-blocking channel send. There is no database work on the request path
// at all.

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// auditedSurfaces are the path prefixes an audit row is written for. Every
// entry is administrative or security-relevant per DECISION 2, and each names
// why it qualifies.
//
// Checked against `r.URL.Path`, which inside the /api/v2 group carries the full
// prefix. Ordered longest-lived first; the list is short enough that the scan
// is cheaper than a trie.
var auditedSurfaces = []string{
	// The whole admin panel: users, roles and permission grants, suspension,
	// invitations, maintenance windows, identity providers, SCIM bindings,
	// gateway providers/models/governance, budget alerts, plugin config.
	"/api/v2/admin/",
	// Personal access token lifecycle — credential issue and revoke.
	"/api/v2/auth/",
	// External identity provisioning: user and group lifecycle driven by an
	// outside IdP, which is precisely the change no human in the product makes.
	"/api/v2/scim/",
	// Secret write, overwrite, hide and delete.
	"/api/v2/secrets/",
	// Project configurations: this is where `ai_credentials` are written and
	// where stored credentials are exercised by the check_connection routes.
	"/api/v2/configurations/",
	// Project and group lifecycle, membership and quota.
	"/api/v2/projects/",
	// Per-project and per-user spend limits: a governance control.
	"/api/v2/elitea_core/project_budget/",
	"/api/v2/elitea_core/user_budget/",
	// Service descriptor registration — what the platform will call out to.
	"/api/v2/elitea_core/register_descriptor/",
	// Scheduled execution: creating one delegates future action to the platform.
	"/api/v2/scheduling/",
}

// AuditedSurfaces returns a copy of the prefix list, so a router-level test can
// assert that every prefix still matches a route that is actually mounted.
//
// That assertion is not ceremony. A hardcoded path that quietly stops matching
// anything is a live defect class in this repo: check-playwright-image-tag went
// on passing after the workflow it read moved, because "nothing found" took the
// OK branch. A surface prefix that matches no route audits nothing and reports
// nothing, which reads exactly like a surface with no traffic.
func AuditedSurfaces() []string {
	surfaces := make([]string, len(auditedSurfaces))
	copy(surfaces, auditedSurfaces)
	return surfaces
}

// auditedSurface reports whether path names a surface DECISION 2 covers.
func auditedSurface(path string) bool {
	for _, prefix := range auditedSurfaces {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// mutatingMethod reports whether the method can change state. HEAD and OPTIONS
// join GET on the "not a change" side; anything unrecognised is treated as a
// change, because an unknown verb reaching a handler is itself worth a row.
func mutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

// auditWorthy applies DECISION 2's rule to one finished request.
func auditWorthy(method string, status int) bool {
	if mutatingMethod(method) {
		return true
	}
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// Audit records administrative and security-relevant requests to
// `centry.audit_events`.
//
// It must be mounted BELOW authentication, so the principal is on the context
// when the row is built. The cost of that ordering, stated rather than hidden:
// a 401 produced by the Auth middleware itself never reaches this middleware
// and is not recorded. That is the right side of the trade — an unauthenticated
// request has no user_id, no user_email and no project to file the row under,
// so the row it would produce is a bare route string, while the refusals worth
// keeping (a known principal denied a permission) are produced by the per-route
// RBAC middleware, which sits BELOW this one and is captured.
//
// A nil recorder returns next unchanged, so a composition without a database
// carries no overhead at all.
func Audit(recorder audit.Recorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if recorder == nil || isNilRecorder(recorder) {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auditedSurface(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, slot := audit.ContextWithAnnotationSlot(r.Context())
			request := r.WithContext(ctx)
			recording := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			start := time.Now()
			next.ServeHTTP(recording, request)
			elapsed := time.Since(start)

			if !auditWorthy(r.Method, recording.status) {
				return
			}
			recorder.Record(ctx, buildEvent(request, recording.status, elapsed, slot))
		})
	}
}

// isNilRecorder catches the typed-nil case — `var r *audit.PostgresRecorder;
// Audit(r)` — which is not `== nil` as an interface and would otherwise install
// a middleware that does work on every admin request and then discards it. The
// same shape took /healthz down with a nil-receiver panic once already.
func isNilRecorder(recorder audit.Recorder) bool {
	typed, ok := recorder.(*audit.PostgresRecorder)
	return ok && typed == nil
}

func buildEvent(r *http.Request, status int, elapsed time.Duration, slot *audit.AnnotationSlot) audit.Event {
	route := matchedRoutePattern(r)
	statusCode := int32(status)
	durationMS := float64(elapsed.Nanoseconds()) / 1e6

	event := audit.Event{
		// The COMPLETION instant, matching legacy, which built the timestamp
		// from the span's end time. UTC because the column is TIMESTAMPTZ and
		// the page converts to the viewer's zone itself.
		Timestamp:  time.Now().UTC(),
		EventType:  "api",
		Action:     r.Method + " " + route,
		HTTPMethod: r.Method,
		HTTPRoute:  route,
		StatusCode: &statusCode,
		DurationMS: &durationMS,
		// Same rule as legacy `_extract_api`: an HTTP span is an error when the
		// status says the operation did not happen.
		IsError: status >= http.StatusBadRequest,
	}

	if user, ok := auth.UserFromContext(r.Context()); ok {
		event.UserEmail = user.Email
		// OwningUserID, not User.ID: a token principal's ID is the token row,
		// and filing an audit event under a token id would make
		// project_user_activity count a credential as a person.
		if id, resolved := user.OwningUserID(); resolved {
			event.UserID = &id
		}
	}
	if projectID, ok := requestProjectID(r); ok {
		event.ProjectID = &projectID
	}
	applyTraceIdentity(r, &event)

	if annotation, present := slot.Read(); present {
		if annotation.EventType != "" {
			event.EventType = annotation.EventType
		}
		if annotation.Action != "" {
			event.Action = annotation.Action
		}
		event.EntityType = annotation.EntityType
		event.EntityID = annotation.EntityID
		event.EntityName = annotation.EntityName
		if annotation.ProjectID != nil {
			event.ProjectID = annotation.ProjectID
		}
	}
	return event
}

// requestProjectID resolves the project the request acted on, in the order the
// rest of this package resolves it: the `{projectID}` URL parameter that 251 of
// this service's routes carry, then the `project_id` query parameter, then the
// project the edge resolved for the caller.
//
// A project that cannot be resolved is left NULL rather than defaulted. Filing
// an event under project 1 because nothing else was available is how the
// /social/author defect (#161) put every author in the same project.
func requestProjectID(r *http.Request) (int64, bool) {
	if raw := chi.URLParam(r, "projectID"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id, true
		}
	}
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id, true
		}
	}
	if project, ok := ProjectFromContext(r.Context()); ok && project.ProjectID > 0 {
		return int64(project.ProjectID), true
	}
	return 0, false
}

// applyTraceIdentity fills trace_id / span_id.
//
// When a real tracer provider is installed, they are the OTel ids of the span
// OtelMiddleware opened, hex-encoded exactly as legacy encoded them
// (`format(trace_id, '032x')`), so an audit row and its trace line up.
//
// When the provider is the no-op default — which it is in every deployment that
// has not configured an OTLP endpoint — the span context is invalid and the ids
// would be NULL. That is not harmless: the Audit Trail's "Traces" view groups
// on `trace_id IS NOT NULL`, so the whole view would render empty while the
// "Spans" view showed rows. So the ids are synthesised instead. One request
// with no distributed tracer IS one trace of one span, and recording it as such
// is the truthful reading, not a placeholder.
func applyTraceIdentity(r *http.Request, event *audit.Event) {
	if spanContext := trace.SpanContextFromContext(r.Context()); spanContext.IsValid() {
		event.TraceID = spanContext.TraceID().String()
		event.SpanID = spanContext.SpanID().String()
		return
	}
	var identity [24]byte
	if _, err := rand.Read(identity[:]); err != nil {
		// Entropy failure is not a reason to lose the row: the trail keeps the
		// event and forfeits only its grouping.
		return
	}
	event.TraceID = hex.EncodeToString(identity[:16])
	event.SpanID = hex.EncodeToString(identity[16:])
}
