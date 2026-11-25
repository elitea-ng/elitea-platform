package adminaccess

// The elevation request routes (ADR-0059 decision 5, "Path 1" steps 1 to 3,
// decision 6, the failure states, decision 7, the credentials table; story
// R2.2b, issue 2596):
//
//	POST /api/v2/admin/access/elevations                       request
//	POST /api/v2/admin/access/elevations/{elevationID}/approve approve
//	POST /api/v2/admin/access/elevations/{elevationID}/deny    deny
//	GET  /api/v2/admin/access/elevations/mine                  the caller's own request (Home)
//	GET  /api/v2/admin/access/elevations/pending               the approval queue
//	GET  /api/v2/admin/access/elevations                       the history
//
// None of them starts an elevation: the start and the end are R2.2c (issue
// 2597). The rules of the ledger (registry.RequestElevation,
// registry.AnswerElevationRequest) are checked in the transaction; this file
// adds what only the request knows:
//
//   - THE CREDENTIAL FIRST. Only a browser session of the kind `session` or
//     `form` with a server-side row reaches a gate: a personal access token,
//     a native session, a run token, a system token and a sealed session
//     answer 403 `elevation_credential_refused` before any permission is
//     read (decision 7: none of them can call an elevation route; a sealed
//     session cannot request or approve).
//   - the route gate: access.breakglass.request (request, own request),
//     access.breakglass.approve (approve, deny, queue), access.roles.view
//     (history);
//   - an approver who holds an active elevation, in any session, is refused
//     (`elevation_approver_elevated`); the ledger checks it in the
//     transaction of the answer;
//   - the duration: absent is the section's default, above its cap is 400
//     `elevation_duration_over_cap`.
//
// The history route returns no reason TEXT to a caller who is neither the
// requester, the approver nor a holder of access.breakglass.review; the
// reason code is enough for the list.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The routes, relative to the /api/v2/admin group.
const (
	ElevationsPath        = "/access/elevations"
	ElevationsMinePath    = "/access/elevations/mine"
	ElevationsPendingPath = "/access/elevations/pending"
	ElevationApprovePath  = "/access/elevations/{elevationID}/approve"
	ElevationDenyPath     = "/access/elevations/{elevationID}/deny"
)

// The stable codes of the refusals (v2.yaml ElevationRefusal).
const (
	CodeElevationCredentialRefused    = "elevation_credential_refused"
	CodeElevationRequestInvalid       = "elevation_request_invalid"
	CodeElevationDurationOverCap      = "elevation_duration_over_cap"
	CodeElevationRequesterNotEligible = "elevation_requester_not_eligible"
	CodeElevationRequestPending       = "elevation_request_pending"
	CodeElevationNotFound             = "elevation_not_found"
	CodeElevationRequestDecided       = "elevation_request_decided"
	CodeElevationRequestExpired       = "elevation_request_expired"
	CodeElevationOwnRequest           = "elevation_own_request"
	CodeElevationApproverElevated     = "elevation_approver_elevated"
	CodeElevationApproverNotEligible  = "elevation_approver_not_eligible"
)

// The bounds of the reads.
const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 200
	maxPendingRows      = 200
	maxRequestBodyBytes = 16 << 10
)

// Person is an account the views name: the person elevated and the approver.
type Person struct {
	ID    int64
	Name  string
	Email string
}

// ElevationStore runs the ledger. *PostgresElevations is the production one.
type ElevationStore interface {
	Request(ctx context.Context, request registry.ElevationRequest) (registry.ElevationRequested, error)
	// Answer approves or denies. On registry.ErrElevationRequestExpired the
	// recorded expiry is committed.
	Answer(ctx context.Context, answer registry.ElevationAnswer) (registry.ElevationRecord, error)
	LatestRequest(ctx context.Context, userID int64) (registry.ElevationRecord, bool, error)
	// Pending reads the queue of the approver exclude (never their own
	// requests), at most limit rows, and the size of the whole queue.
	Pending(ctx context.Context, now time.Time, lifetime time.Duration, exclude int64, limit int) ([]registry.ElevationRecord, int, error)
	History(ctx context.Context, limit, offset int) ([]registry.ElevationRecord, int, error)
	People(ctx context.Context, ids []int64) (map[int64]Person, error)
	// HoldsStanding reports whether the person holds permission through a
	// standing role.
	HoldsStanding(ctx context.Context, userID int64, permission string) (bool, error)
}

// BreakGlassSettingsReader reads the break_glass section.
type BreakGlassSettingsReader func(ctx context.Context) (platformconfig.BreakGlassSettings, error)

// PostgresElevations is the ElevationStore over the ledger of shared 0269.
type PostgresElevations struct {
	pool     *pgxpool.Pool
	appender securityaudit.Appender
}

// NewPostgresElevations returns the store. A nil appender fails every write:
// the ledger takes no step without its event.
func NewPostgresElevations(pool *pgxpool.Pool, appender securityaudit.Appender) *PostgresElevations {
	return &PostgresElevations{pool: pool, appender: appender}
}

// Request implements ElevationStore.
func (p *PostgresElevations) Request(ctx context.Context, request registry.ElevationRequest) (registry.ElevationRequested, error) {
	var written registry.ElevationRequested
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		written, err = registry.RequestElevation(ctx, tx, p.appender, request)
		return err
	})
	return written, err
}

// Answer implements ElevationStore. A refusal rolls back, except the expiry
// the ledger records for an answer that came too late: that is committed, so
// the request reads as expired at once.
func (p *PostgresElevations) Answer(ctx context.Context, answer registry.ElevationAnswer) (registry.ElevationRecord, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return registry.ElevationRecord{}, fmt.Errorf("answer the elevation request: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	record, answerErr := registry.AnswerElevationRequest(ctx, tx, p.appender, answer)
	if answerErr != nil && !errors.Is(answerErr, registry.ErrElevationRequestExpired) {
		return record, answerErr
	}
	if err := tx.Commit(ctx); err != nil {
		return registry.ElevationRecord{}, fmt.Errorf("answer the elevation request: commit: %w", err)
	}
	return record, answerErr
}

// LatestRequest implements ElevationStore.
func (p *PostgresElevations) LatestRequest(ctx context.Context, userID int64) (registry.ElevationRecord, bool, error) {
	return registry.LatestElevationRequestOf(ctx, p.pool, userID)
}

// Pending implements ElevationStore.
func (p *PostgresElevations) Pending(ctx context.Context, now time.Time, lifetime time.Duration, exclude int64, limit int) ([]registry.ElevationRecord, int, error) {
	return registry.PendingElevationRequests(ctx, p.pool, now, lifetime, exclude, limit)
}

// History implements ElevationStore.
func (p *PostgresElevations) History(ctx context.Context, limit, offset int) ([]registry.ElevationRecord, int, error) {
	return registry.ElevationHistory(ctx, p.pool, limit, offset)
}

// HoldsStanding implements ElevationStore.
func (p *PostgresElevations) HoldsStanding(ctx context.Context, userID int64, permission string) (bool, error) {
	return registry.HoldsStandingPermission(ctx, p.pool, userID, permission)
}

// People implements ElevationStore.
func (p *PostgresElevations) People(ctx context.Context, ids []int64) (map[int64]Person, error) {
	people := map[int64]Person{}
	if len(ids) == 0 {
		return people, nil
	}
	rows, err := p.pool.Query(ctx, `
SELECT id, COALESCE(name, ''), COALESCE(email, '')
FROM public.auth_core__user WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the people of the elevations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.ID, &person.Name, &person.Email); err != nil {
			return nil, fmt.Errorf("read the people of the elevations: %w", err)
		}
		people[person.ID] = person
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the people of the elevations: %w", err)
	}
	return people, nil
}

// ElevationsHandler serves the routes.
type ElevationsHandler struct {
	store    ElevationStore
	settings BreakGlassSettingsReader
	roles    RoleReader
	now      func() time.Time
}

// NewElevationsHandler returns the handler. A nil store, settings reader or
// role reader answers 503.
//
// The handler resolves no permission itself: the gates do, and the rules
// that read the caller's roles (the approver's level, that the approver is
// under no elevation, that the history's reader holds
// access.breakglass.review) read the STANDING roles and the ledger in
// SQL, so an elevation never counts towards them.
func NewElevationsHandler(store ElevationStore, settings BreakGlassSettingsReader, roles RoleReader) *ElevationsHandler {
	return &ElevationsHandler{store: store, settings: settings, roles: roles, now: time.Now}
}

// WithClock replaces the clock (tests).
func (h *ElevationsHandler) WithClock(now func() time.Time) *ElevationsHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// ElevationGates are the route gates the production router puts behind the
// credential check.
type ElevationGates struct {
	// Request is access.breakglass.request: the request and the own request.
	Request func(http.Handler) http.Handler
	// Approve is access.breakglass.approve: approve, deny and the queue.
	Approve func(http.Handler) http.Handler
	// History is access.roles.view: the history.
	History func(http.Handler) http.Handler
}

// Register mounts the routes on the /api/v2/admin router. The credential
// check runs BEFORE each gate, so a token never has its permissions read on
// an elevation route.
func (h *ElevationsHandler) Register(r chi.Router, gates ElevationGates) {
	request := r.With(RequireElevationCredential, gates.Request)
	approve := r.With(RequireElevationCredential, gates.Approve)
	history := r.With(RequireElevationCredential, gates.History)
	request.Post(ElevationsPath, h.CreateRequest)
	request.Get(ElevationsMinePath, h.Mine)
	approve.Post(ElevationApprovePath, h.Approve)
	approve.Post(ElevationDenyPath, h.Deny)
	approve.Get(ElevationsPendingPath, h.Pending)
	history.Get(ElevationsPath, h.History)
}

// ElevationCredentialAdmitted reports whether a credential may call an
// elevation route: a browser session of the kind `session` or `form` with a
// server-side row. Every other kind is refused, the sealed session included.
func ElevationCredentialAdmitted(credential auth.Credential) bool {
	switch credential.Kind {
	case auth.CredentialKindSession, auth.CredentialKindForm:
		return credential.BrowserSessionRef > 0
	}
	return false
}

// RequireElevationCredential refuses every credential but a browser session
// (ElevationCredentialAdmitted) with 403 elevation_credential_refused, before
// the route gate reads a permission. An unauthenticated request is 401.
func RequireElevationCredential(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok {
			apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !ElevationCredentialAdmitted(principal.Credential()) {
			writeElevationRefusal(w, http.StatusForbidden, CodeElevationCredentialRefused,
				"an elevation route takes a browser session only: a token, a native session, a run token and a sealed session cannot call it")
			return
		}
		next.ServeHTTP(w, r)
	})
}

/* ── the wire shapes (v2.yaml ElevationRequest*) ───────────────────────── */

type elevationRequestBody struct {
	ReasonCode      string  `json:"reason_code"`
	Reason          string  `json:"reason"`
	Ticket          *string `json:"ticket"`
	DurationMinutes *int64  `json:"duration_minutes"`
}

type elevationDenyBody struct {
	ReasonCode string `json:"reason_code"`
}

type personBody struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type elevationView struct {
	ID               string      `json:"id"`
	State            string      `json:"state"`
	Path             string      `json:"path"`
	Level            *string     `json:"level"`
	ReasonCode       string      `json:"reason_code"`
	Reason           *string     `json:"reason"`
	Ticket           *string     `json:"ticket"`
	DurationMinutes  *int64      `json:"duration_minutes"`
	Person           personBody  `json:"person"`
	Approver         *personBody `json:"approver"`
	DenialReasonCode *string     `json:"denial_reason_code"`
	RequestedAt      time.Time   `json:"requested_at"`
	RequestExpiresAt *time.Time  `json:"request_expires_at"`
	DecidedAt        *time.Time  `json:"decided_at"`
	StartedAt        *time.Time  `json:"started_at"`
	ExpiresAt        *time.Time  `json:"expires_at"`
	EndedAt          *time.Time  `json:"ended_at"`
	EndReason        *string     `json:"end_reason"`
	ReviewState      *string     `json:"review_state"`
	TimeLeftSeconds  int64       `json:"time_left_seconds"`
}

type ownElevationBody struct {
	Request        *elevationView `json:"request"`
	ApprovingRoles []string       `json:"approving_roles"`
}

type elevationListBody struct {
	Total int             `json:"total"`
	Rows  []elevationView `json:"rows"`
}

/* ── the writes ────────────────────────────────────────────────────────── */

// CreateRequest asks for an elevation.
func (h *ElevationsHandler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	principal, actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body elevationRequestBody
	if !decodeElevationBody(w, r, &body) {
		return
	}
	reason := strings.TrimSpace(body.Reason)
	switch {
	case !slices.Contains(registry.ElevationReasonCodes(), body.ReasonCode):
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
			"reason_code must be one of "+strings.Join(registry.ElevationReasonCodes(), ", "))
		return
	case reason == "":
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid, "a request needs a reason")
		return
	}
	settings, ok := h.readSettings(w, r)
	if !ok {
		return
	}
	duration := settings.OnlineDefault
	if body.DurationMinutes != nil {
		if *body.DurationMinutes < 1 {
			writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
				"duration_minutes must be at least 1")
			return
		}
		if *body.DurationMinutes > int64(settings.OnlineMax/time.Minute) {
			writeElevationRefusal(w, http.StatusBadRequest, CodeElevationDurationOverCap,
				fmt.Sprintf("duration_minutes is over the cap of %d minutes", int64(settings.OnlineMax/time.Minute)))
			return
		}
		duration = time.Duration(*body.DurationMinutes) * time.Minute
	}
	ticket := ""
	if body.Ticket != nil {
		ticket = *body.Ticket
	}
	now := h.now().UTC()
	policy := settings.At(now)
	written, err := h.store.Request(r.Context(), registry.ElevationRequest{
		UserID:            actor,
		BrowserSessionRef: principal.BrowserSessionRef,
		ReasonCode:        body.ReasonCode,
		Reason:            reason,
		Ticket:            ticket,
		Duration:          duration,
		Cap:               settings.OnlineMax,
		ApprovalRequired:  policy.ApprovalRequired,
		RequestLifetime:   settings.RequestLifetime,
		At:                now,
		CorrelationID:     correlationID(r.Context()),
	})
	if err != nil {
		h.writeLedgerError(w, r, err)
		return
	}
	view, ok := h.views(w, r, []registry.ElevationRecord{written.Elevation}, now, settings.RequestLifetime, func(registry.ElevationRecord) bool { return true })
	if !ok {
		return
	}
	writeElevationJSON(w, http.StatusCreated, view[0])
}

// Approve approves a request; the level is the approver's.
func (h *ElevationsHandler) Approve(w http.ResponseWriter, r *http.Request) { h.answer(w, r, true) }

// Deny denies a request with a reason code.
func (h *ElevationsHandler) Deny(w http.ResponseWriter, r *http.Request) { h.answer(w, r, false) }

func (h *ElevationsHandler) answer(w http.ResponseWriter, r *http.Request, approve bool) {
	principal, actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "elevationID"))
	if err != nil {
		writeElevationRefusal(w, http.StatusNotFound, CodeElevationNotFound, "no such elevation request")
		return
	}
	var denial string
	if !approve {
		var body elevationDenyBody
		if !decodeElevationBody(w, r, &body) {
			return
		}
		if !slices.Contains(registry.ElevationDenialReasonCodes(), body.ReasonCode) {
			writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
				"reason_code must be one of "+strings.Join(registry.ElevationDenialReasonCodes(), ", "))
			return
		}
		denial = body.ReasonCode
	}
	// A person under an elevation cannot answer (decision 5, step 2): the
	// ledger refuses it for every session of the person, in the transaction
	// of the answer (registry.ErrElevationApproverElevated).
	settings, ok := h.readSettings(w, r)
	if !ok {
		return
	}
	now := h.now().UTC()
	record, err := h.store.Answer(r.Context(), registry.ElevationAnswer{
		ID:                 id,
		ApproverUserID:     actor,
		ApproverSessionRef: principal.BrowserSessionRef,
		Approve:            approve,
		DenialReasonCode:   denial,
		RequestLifetime:    settings.RequestLifetime,
		At:                 now,
		CorrelationID:      correlationID(r.Context()),
	})
	if err != nil {
		h.writeLedgerError(w, r, err)
		return
	}
	view, ok := h.views(w, r, []registry.ElevationRecord{record}, now, settings.RequestLifetime, func(registry.ElevationRecord) bool { return true })
	if !ok {
		return
	}
	writeElevationJSON(w, http.StatusOK, view[0])
}

/* ── the reads ─────────────────────────────────────────────────────────── */

// Mine answers the caller's latest request with its state, the time left and
// the roles that can approve it (Home).
func (h *ElevationsHandler) Mine(w http.ResponseWriter, r *http.Request) {
	_, actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	settings, ok := h.readSettings(w, r)
	if !ok {
		return
	}
	roles, err := h.roles.GrantingRoles(r.Context(), []string{registry.AccessBreakglassApprove})
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	body := ownElevationBody{ApprovingRoles: make([]string, 0, len(roles))}
	for _, role := range roles {
		if role.Marker == string(registry.MarkerUnassignable) {
			continue
		}
		body.ApprovingRoles = append(body.ApprovingRoles, role.Name)
	}
	slices.Sort(body.ApprovingRoles)
	record, found, err := h.store.LatestRequest(r.Context(), actor)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	if found {
		now := h.now().UTC()
		views, ok := h.views(w, r, []registry.ElevationRecord{record}, now, settings.RequestLifetime, func(registry.ElevationRecord) bool { return true })
		if !ok {
			return
		}
		body.Request = &views[0]
	}
	writeElevationJSON(w, http.StatusOK, body)
}

// Pending answers the requests the caller can answer: undecided, within
// their lifetime, and not the caller's own.
func (h *ElevationsHandler) Pending(w http.ResponseWriter, r *http.Request) {
	_, actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	settings, ok := h.readSettings(w, r)
	if !ok {
		return
	}
	now := h.now().UTC()
	pending, total, err := h.store.Pending(r.Context(), now, settings.RequestLifetime, actor, maxPendingRows)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	// The store leaves the caller's own requests out; this is the closed
	// failure of a store that does not.
	pending = slices.DeleteFunc(pending, func(e registry.ElevationRecord) bool {
		return e.RequesterUserID == actor || e.UserID == actor
	})
	// An approver reads the reason text: the decision depends on it.
	views, ok := h.views(w, r, pending, now, settings.RequestLifetime, func(registry.ElevationRecord) bool { return true })
	if !ok {
		return
	}
	writeElevationJSON(w, http.StatusOK, elevationListBody{Total: max(total, len(views)), Rows: views})
}

// History answers the ledger newest first, paged by limit and offset.
func (h *ElevationsHandler) History(w http.ResponseWriter, r *http.Request) {
	_, actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	limit, offset, ok := pageOf(w, r)
	if !ok {
		return
	}
	reviewer, err := h.store.HoldsStanding(r.Context(), actor, registry.AccessBreakglassReview)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	settings, ok := h.readSettings(w, r)
	if !ok {
		return
	}
	history, total, err := h.store.History(r.Context(), limit, offset)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	views, ok := h.views(w, r, history, h.now().UTC(), settings.RequestLifetime, func(e registry.ElevationRecord) bool {
		return reviewer || e.RequesterUserID == actor || e.UserID == actor || e.ApproverUserID == actor
	})
	if !ok {
		return
	}
	writeElevationJSON(w, http.StatusOK, elevationListBody{Total: total, Rows: views})
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// caller answers the principal and its account, or writes the refusal.
func (h *ElevationsHandler) caller(w http.ResponseWriter, r *http.Request) (auth.User, int64, bool) {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return auth.User{}, 0, false
	}
	if !ElevationCredentialAdmitted(principal.Credential()) {
		// The route's middleware refuses first; this is the closed failure
		// of a mount without it.
		writeElevationRefusal(w, http.StatusForbidden, CodeElevationCredentialRefused,
			"an elevation route takes a browser session only")
		return auth.User{}, 0, false
	}
	actor, ok := principal.OwningUserID()
	if !ok || actor <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return auth.User{}, 0, false
	}
	if h.store == nil || h.settings == nil || h.roles == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the elevation requests are not configured")
		return auth.User{}, 0, false
	}
	return principal, actor, true
}

func (h *ElevationsHandler) readSettings(w http.ResponseWriter, r *http.Request) (platformconfig.BreakGlassSettings, bool) {
	settings, err := h.settings(r.Context())
	if err != nil {
		h.writeReadError(w, r, err)
		return platformconfig.BreakGlassSettings{}, false
	}
	return settings, true
}

// views renders rows; reason says which rows show the reason text.
func (h *ElevationsHandler) views(w http.ResponseWriter, r *http.Request, rows []registry.ElevationRecord,
	now time.Time, lifetime time.Duration, reason func(registry.ElevationRecord) bool,
) ([]elevationView, bool) {
	ids := []int64{}
	for _, row := range rows {
		ids = append(ids, row.UserID)
		if row.ApproverUserID > 0 {
			ids = append(ids, row.ApproverUserID)
		}
	}
	slices.Sort(ids)
	people, err := h.store.People(r.Context(), slices.Compact(ids))
	if err != nil {
		h.writeReadError(w, r, err)
		return nil, false
	}
	views := make([]elevationView, 0, len(rows))
	for _, row := range rows {
		views = append(views, elevationViewOf(row, now, lifetime, people, reason(row)))
	}
	return views, true
}

func elevationViewOf(e registry.ElevationRecord, now time.Time, lifetime time.Duration, people map[int64]Person, withReason bool) elevationView {
	person := people[e.UserID]
	view := elevationView{
		ID:               e.ID.String(),
		State:            registry.RequestStateAt(e, now, lifetime),
		Path:             e.Path,
		Level:            optionalString(e.Level),
		ReasonCode:       e.ReasonCode,
		Person:           personBody{ID: e.UserID, Name: person.Name, Email: person.Email},
		DenialReasonCode: optionalString(e.DenialReasonCode),
		RequestedAt:      e.RequestedAt.UTC(),
		DecidedAt:        optionalTime(e.DecidedAt),
		StartedAt:        optionalTime(e.StartedAt),
		ExpiresAt:        optionalTime(e.ExpiresAt),
		EndedAt:          optionalTime(e.EndedAt),
		EndReason:        optionalString(e.EndReason),
		ReviewState:      optionalString(e.ReviewState),
	}
	if withReason {
		view.Reason = optionalString(e.Reason)
		view.Ticket = optionalString(e.Ticket)
	}
	if e.Duration > 0 {
		minutes := int64(e.Duration / time.Minute)
		view.DurationMinutes = &minutes
	}
	if e.ApproverUserID > 0 {
		approver := people[e.ApproverUserID]
		view.Approver = &personBody{ID: e.ApproverUserID, Name: approver.Name, Email: approver.Email}
	}
	switch view.State {
	case registry.ElevationStateRequested:
		deadline := registry.RequestDeadline(e, lifetime)
		view.RequestExpiresAt = optionalTime(deadline)
		view.TimeLeftSeconds = secondsUntil(now, deadline)
	case registry.ElevationStateActive:
		view.TimeLeftSeconds = secondsUntil(now, e.ExpiresAt)
	}
	return view
}

func secondsUntil(now, until time.Time) int64 {
	if !until.After(now) {
		return 0
	}
	return int64(until.Sub(now) / time.Second)
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pageOf reads limit and offset.
func pageOf(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit, offset := defaultHistoryLimit, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid, name+" must be a whole number")
			return 0, 0, false
		}
		*target = n
	}
	if limit < 1 || limit > maxHistoryLimit {
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
			fmt.Sprintf("limit must be between 1 and %d", maxHistoryLimit))
		return 0, 0, false
	}
	return limit, offset, true
}

func decodeElevationBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(into)
	if err == nil {
		// One JSON value and nothing after it.
		if trailing := decoder.Decode(&struct{}{}); !errors.Is(trailing, io.EOF) {
			err = errors.New("data after the request")
		}
	}
	if err != nil {
		message := "the body is not a valid request"
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			message = fmt.Sprintf("the body is larger than %d bytes", maxRequestBodyBytes)
		}
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid, message)
		return false
	}
	return true
}

// writeLedgerError maps the ledger's refusals to their status and code.
func (h *ElevationsHandler) writeLedgerError(w http.ResponseWriter, r *http.Request, err error) {
	for _, refusal := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{registry.ErrElevationDurationOverCap, http.StatusBadRequest, CodeElevationDurationOverCap, "the duration is over the cap"},
		{registry.ErrElevationSession, http.StatusForbidden, CodeElevationCredentialRefused, "this session cannot hold an elevation"},
		{registry.ErrElevationNoSession, http.StatusForbidden, CodeElevationCredentialRefused, "this session cannot hold an elevation"},
		{registry.ErrElevationPerson, http.StatusForbidden, CodeElevationRequesterNotEligible, "the account is not active"},
		{registry.ErrElevationRequester, http.StatusForbidden, CodeElevationRequesterNotEligible,
			"a request needs access.breakglass.request through a standing role"},
		{registry.ErrElevationRequestPending, http.StatusConflict, CodeElevationRequestPending,
			"a request of yours is waiting for an answer or for its start"},
		{registry.ErrElevationNotFound, http.StatusNotFound, CodeElevationNotFound, "no such elevation request"},
		{registry.ErrElevationRequestExpired, http.StatusGone, CodeElevationRequestExpired,
			"the request expired unanswered; the requester can make a new one"},
		{registry.ErrElevationRequestDecided, http.StatusConflict, CodeElevationRequestDecided, "the request was decided already"},
		{registry.ErrElevationOwnRequest, http.StatusForbidden, CodeElevationOwnRequest, "nobody answers their own request"},
		{registry.ErrElevationApproverElevated, http.StatusForbidden, CodeElevationApproverElevated,
			"a person under an elevation cannot answer an elevation request"},
		{registry.ErrElevationPersonNotEligible, http.StatusForbidden, CodeElevationRequesterNotEligible,
			"the person of the request can no longer hold an elevation"},
		{registry.ErrElevationApproverNotEligible, http.StatusForbidden, CodeElevationApproverNotEligible,
			"an answer needs access.breakglass.approve through a standing role"},
		{registry.ErrElevationInvalid, http.StatusBadRequest, CodeElevationRequestInvalid, ""},
	} {
		if errors.Is(err, refusal.err) {
			message := refusal.message
			if message == "" {
				message = err.Error()
			}
			writeElevationRefusal(w, refusal.status, refusal.code, message)
			return
		}
	}
	h.writeReadError(w, r, err)
}

func (h *ElevationsHandler) writeReadError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	slog.ErrorContext(r.Context(), "admin access: an elevation request route failed", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "the elevation requests could not be read or written")
}

func writeElevationRefusal(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

func writeElevationJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func correlationID(ctx context.Context) string {
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		return spanContext.TraceID().String()
	}
	return ""
}
