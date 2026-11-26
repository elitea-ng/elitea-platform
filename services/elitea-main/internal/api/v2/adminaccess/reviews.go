package adminaccess

// The review after use (ADR-0059 decision 5, "Review after use"; story
// R2.9a, issue 2523):
//
//	GET  /api/v2/admin/access/elevations/{elevationID}/review  the review: the elevation, its reviewers, its changes, the attestation
//	POST /api/v2/admin/access/elevations/{elevationID}/review  the attestation: an outcome and a note
//	GET  /api/v2/admin/access/reverts                          the revert list (platform administrators)
//	POST /api/v2/admin/access/reverts/{itemID}/handled         mark a revert entry handled
//
// Only a browser session reaches a gate (RequireElevationCredential): a
// personal access token, a native session, a run token and a sealed session
// answer 403 before any permission is read. The review routes are behind
// access.breakglass.review, the revert list behind access.breakglass.manage.
//
// The holder, the requester and the approver of an elevation neither read
// nor write its review (403 elevation_reviewer_involved). The attestation is
// written only by a reviewer recorded when the elevation ended (403
// elevation_reviewer_not_eligible otherwise): the current roles do not
// decide it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The routes, relative to the /api/v2/admin group.
const (
	ElevationReviewPath = "/access/elevations/{elevationID}/review"
	RevertsPath         = "/access/reverts"
	RevertHandledPath   = "/access/reverts/{itemID}/handled"
)

// The stable codes of the review refusals (v2.yaml ElevationRefusal).
const (
	CodeElevationReviewerInvolved    = "elevation_reviewer_involved"
	CodeElevationReviewerNotEligible = "elevation_reviewer_not_eligible"
	CodeElevationNotReviewable       = "elevation_not_reviewable"
	CodeRevertNotFound               = "revert_not_found"
)

// defaultChangesLimit is the page of the changes of a review.
const defaultChangesLimit = 50

// ReviewStore runs the review. *PostgresReviews is the production one.
type ReviewStore interface {
	Elevation(ctx context.Context, id uuid.UUID) (registry.ElevationRecord, error)
	Reviewers(ctx context.Context, id uuid.UUID) ([]int64, error)
	Items(ctx context.Context, id uuid.UUID) ([]registry.ReviewItem, error)
	Changes(ctx context.Context, id uuid.UUID, limit, offset int) ([]registry.ElevationAuditRow, int, error)
	Review(ctx context.Context, review registry.ElevationReview) (registry.ReviewResult, error)
	Reverts(ctx context.Context, open bool, limit, offset int) ([]registry.RevertEntry, int, error)
	MarkHandled(ctx context.Context, id uuid.UUID, by int64, at time.Time) (bool, error)
	People(ctx context.Context, ids []int64) (map[int64]Person, error)
}

// PostgresReviews is the ReviewStore over the ledger and its items.
type PostgresReviews struct {
	*PostgresElevations
}

// NewPostgresReviews returns the store. A nil appender fails every review:
// an attestation is not written without its event.
func NewPostgresReviews(pool *pgxpool.Pool, appender securityaudit.Appender) *PostgresReviews {
	return &PostgresReviews{PostgresElevations: NewPostgresElevations(pool, appender)}
}

// Elevation implements ReviewStore.
func (p *PostgresReviews) Elevation(ctx context.Context, id uuid.UUID) (registry.ElevationRecord, error) {
	return registry.ReadElevation(ctx, p.pool, id)
}

// Reviewers implements ReviewStore.
func (p *PostgresReviews) Reviewers(ctx context.Context, id uuid.UUID) ([]int64, error) {
	return registry.ElevationReviewerIDs(ctx, p.pool, id)
}

// Items implements ReviewStore.
func (p *PostgresReviews) Items(ctx context.Context, id uuid.UUID) ([]registry.ReviewItem, error) {
	return registry.ElevationReviewItems(ctx, p.pool, id)
}

// Changes implements ReviewStore.
func (p *PostgresReviews) Changes(ctx context.Context, id uuid.UUID, limit, offset int) ([]registry.ElevationAuditRow, int, error) {
	return registry.ElevationAuditRows(ctx, p.pool, id, limit, offset)
}

// Review implements ReviewStore.
func (p *PostgresReviews) Review(ctx context.Context, review registry.ElevationReview) (registry.ReviewResult, error) {
	var result registry.ReviewResult
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		result, err = registry.ReviewElevation(ctx, tx, p.appender, review)
		return err
	})
	return result, err
}

// Reverts implements ReviewStore.
func (p *PostgresReviews) Reverts(ctx context.Context, open bool, limit, offset int) ([]registry.RevertEntry, int, error) {
	return registry.RevertList(ctx, p.pool, open, limit, offset)
}

// MarkHandled implements ReviewStore.
func (p *PostgresReviews) MarkHandled(ctx context.Context, id uuid.UUID, by int64, at time.Time) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var err error
		changed, err = registry.MarkRevertHandled(ctx, tx, id, by, at)
		return err
	})
	return changed, err
}

// ReviewsHandler serves the routes.
type ReviewsHandler struct {
	store ReviewStore
	now   func() time.Time
}

// NewReviewsHandler returns the handler. A nil store answers 503.
func NewReviewsHandler(store ReviewStore) *ReviewsHandler {
	return &ReviewsHandler{store: store, now: time.Now}
}

// WithClock replaces the clock (tests).
func (h *ReviewsHandler) WithClock(now func() time.Time) *ReviewsHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// ReviewGates are the route gates behind the credential check.
type ReviewGates struct {
	// Review is access.breakglass.review: the review and the attestation.
	Review func(http.Handler) http.Handler
	// Manage is access.breakglass.manage: the revert list.
	Manage func(http.Handler) http.Handler
}

// Register mounts the routes on the /api/v2/admin router.
func (h *ReviewsHandler) Register(r chi.Router, gates ReviewGates) {
	review := r.With(RequireElevationCredential, h.recordedReviewerOr(gates.Review))
	manage := r.With(RequireElevationCredential, gates.Manage)
	review.Get(ElevationReviewPath, h.Read)
	review.Post(ElevationReviewPath, h.Write)
	manage.Get(RevertsPath, h.Reverts)
	manage.Post(RevertHandledPath, h.MarkHandled)
}

/* ── the wire shapes (v2.yaml ElevationReview*) ────────────────────────── */

type reviewBody struct {
	Outcome           string `json:"outcome"`
	Note              string `json:"note"`
	ReplacementIssued *bool  `json:"replacement_issued"`
}

type attestationView struct {
	Outcome           string     `json:"outcome"`
	Note              *string    `json:"note"`
	ReplacementIssued *bool      `json:"replacement_issued"`
	Reviewer          personBody `json:"reviewer"`
	ReviewedAt        time.Time  `json:"reviewed_at"`
}

type reviewerChangeView struct {
	Person personBody `json:"person"`
	Actor  personBody `json:"actor"`
	Action string     `json:"action"`
	At     time.Time  `json:"at"`
}

type changeView struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	UserID     int64     `json:"user_id"`
	Action     string    `json:"action"`
	Method     string    `json:"method"`
	Route      string    `json:"route"`
	Status     int       `json:"status"`
	EntityType *string   `json:"entity_type"`
	EntityID   *int64    `json:"entity_id"`
	EntityName *string   `json:"entity_name"`
	Flagged    bool      `json:"flagged"`
}

type changesView struct {
	Total int          `json:"total"`
	Rows  []changeView `json:"rows"`
}

type revertView struct {
	ID          string      `json:"id"`
	ElevationID string      `json:"elevation_id"`
	Holder      personBody  `json:"holder"`
	Change      changeView  `json:"change"`
	OpenedAt    time.Time   `json:"opened_at"`
	HandledAt   *time.Time  `json:"handled_at"`
	HandledBy   *personBody `json:"handled_by"`
}

type reviewView struct {
	Elevation       elevationView        `json:"elevation"`
	Reviewers       []personBody         `json:"reviewers"`
	CanReview       bool                 `json:"can_review"`
	Review          *attestationView     `json:"review"`
	ReviewerChanges []reviewerChangeView `json:"reviewer_changes"`
	Changes         changesView          `json:"changes"`
	Reverts         []revertView         `json:"reverts"`
}

type revertListBody struct {
	Total int          `json:"total"`
	Rows  []revertView `json:"rows"`
}

type revertHandledBody struct {
	ID      string `json:"id"`
	Changed bool   `json:"changed"`
}

/* ── the routes ────────────────────────────────────────────────────────── */

// Read answers the review of one elevation: its changes paged by limit and
// offset, flagged first.
func (h *ReviewsHandler) Read(w http.ResponseWriter, r *http.Request) {
	actor, id, ok := h.reviewCaller(w, r)
	if !ok {
		return
	}
	limit, offset, ok := pageOf(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("limit") == "" {
		limit = defaultChangesLimit
	}
	elevation, ok := h.involvedRefused(w, r, id, actor)
	if !ok {
		return
	}
	h.writeReview(w, r, elevation, actor, limit, offset, http.StatusOK)
}

// Write records the attestation.
func (h *ReviewsHandler) Write(w http.ResponseWriter, r *http.Request) {
	actor, id, ok := h.reviewCaller(w, r)
	if !ok {
		return
	}
	var body reviewBody
	if !decodeElevationBody(w, r, &body) {
		return
	}
	if body.Outcome != registry.ReviewOutcomeJustified && body.Outcome != registry.ReviewOutcomeUnauthorized {
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid,
			"outcome must be justified or unauthorized")
		return
	}
	principal, _ := auth.UserFromContext(r.Context())
	result, err := h.store.Review(r.Context(), registry.ElevationReview{
		ID:                id,
		ReviewerUserID:    actor,
		Outcome:           body.Outcome,
		Note:              strings.TrimSpace(body.Note),
		ReplacementIssued: body.ReplacementIssued,
		At:                h.now().UTC(),
		Source:            securityaudit.ElevationSourceConsole,
		AuthenticationRef: browserSessionRef(principal.BrowserSessionRef),
		CorrelationID:     correlationID(r.Context()),
	})
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	h.writeReview(w, r, result.Elevation, actor, defaultChangesLimit, 0, http.StatusOK)
}

// Reverts answers the revert list, unhandled entries first; ?open=true
// answers only those.
func (h *ReviewsHandler) Reverts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.caller(w, r); !ok {
		return
	}
	limit, offset, ok := pageOf(w, r)
	if !ok {
		return
	}
	open := false
	switch r.URL.Query().Get("open") {
	case "", "false":
	case "true":
		open = true
	default:
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid, "open must be true or false")
		return
	}
	entries, total, err := h.store.Reverts(r.Context(), open, limit, offset)
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	ids := []int64{}
	for _, e := range entries {
		ids = append(ids, e.HolderUserID, e.HandledBy)
	}
	people, ok := h.people(w, r, ids)
	if !ok {
		return
	}
	body := revertListBody{Total: total, Rows: make([]revertView, 0, len(entries))}
	for _, e := range entries {
		body.Rows = append(body.Rows, revertViewOf(e, people))
	}
	writeElevationJSON(w, http.StatusOK, body)
}

// MarkHandled marks one revert entry handled, once.
func (h *ReviewsHandler) MarkHandled(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeElevationRefusal(w, http.StatusNotFound, CodeRevertNotFound, "no such revert entry")
		return
	}
	changed, err := h.store.MarkHandled(r.Context(), id, actor, h.now().UTC())
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	writeElevationJSON(w, http.StatusOK, revertHandledBody{ID: id.String(), Changed: changed})
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func (h *ReviewsHandler) caller(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return 0, false
	}
	if !ElevationCredentialAdmitted(principal.Credential()) {
		writeElevationRefusal(w, http.StatusForbidden, CodeElevationCredentialRefused,
			"a review route takes a browser session only")
		return 0, false
	}
	actor, ok := principal.OwningUserID()
	if !ok || actor <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return 0, false
	}
	if h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the elevation reviews are not configured")
		return 0, false
	}
	return actor, true
}

// recordedReviewerOr admits a reviewer recorded for the elevation of the
// path past gate: eligibility for a review is the record made when the
// elevation ended (R2.9a), so a reviewer whose access.breakglass.review was
// taken away afterwards (GuardPendingReviewer lists that change) still reads
// and writes THIS review. Everyone else goes through gate, the standing
// permission; the handler's own rules apply to both.
func (h *ReviewsHandler) recordedReviewerOr(gate func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		gated := gate(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.store != nil {
				principal, ok := auth.UserFromContext(r.Context())
				actor, owned := principal.OwningUserID()
				id, err := uuid.Parse(chi.URLParam(r, "elevationID"))
				if ok && owned && actor > 0 && err == nil {
					reviewers, err := h.store.Reviewers(r.Context(), id)
					if err == nil && slices.Contains(reviewers, actor) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}
			gated.ServeHTTP(w, r)
		})
	}
}

func (h *ReviewsHandler) reviewCaller(w http.ResponseWriter, r *http.Request) (int64, uuid.UUID, bool) {
	actor, ok := h.caller(w, r)
	if !ok {
		return 0, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "elevationID"))
	if err != nil {
		writeElevationRefusal(w, http.StatusNotFound, CodeElevationNotFound, "no such elevation")
		return 0, uuid.Nil, false
	}
	return actor, id, true
}

// involvedRefused reads the elevation and refuses a person involved in it.
func (h *ReviewsHandler) involvedRefused(w http.ResponseWriter, r *http.Request, id uuid.UUID, actor int64) (registry.ElevationRecord, bool) {
	elevation, err := h.store.Elevation(r.Context(), id)
	if err != nil {
		h.writeReviewError(w, r, err)
		return registry.ElevationRecord{}, false
	}
	if registry.InvolvedIn(elevation, actor) {
		h.writeReviewError(w, r, registry.ErrReviewerInvolved)
		return registry.ElevationRecord{}, false
	}
	return elevation, true
}

func (h *ReviewsHandler) writeReview(w http.ResponseWriter, r *http.Request, elevation registry.ElevationRecord,
	actor int64, limit, offset, status int,
) {
	ctx := r.Context()
	reviewers, err := h.store.Reviewers(ctx, elevation.ID)
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	items, err := h.store.Items(ctx, elevation.ID)
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	changes, total, err := h.store.Changes(ctx, elevation.ID, limit, offset)
	if err != nil {
		h.writeReviewError(w, r, err)
		return
	}
	ids := append([]int64{elevation.UserID, elevation.ApproverUserID}, reviewers...)
	for _, item := range items {
		ids = append(ids, item.UserID, item.ActorUserID, item.HandledBy)
	}
	people, ok := h.people(w, r, ids)
	if !ok {
		return
	}
	now := h.now().UTC()
	view := reviewView{
		Elevation:       elevationViewOf(elevation, now, 0, people, true),
		Reviewers:       make([]personBody, 0, len(reviewers)),
		CanReview:       elevation.ReviewState == registry.ElevationReviewPending && slices.Contains(reviewers, actor),
		ReviewerChanges: []reviewerChangeView{},
		Changes:         changesView{Total: total, Rows: make([]changeView, 0, len(changes))},
		Reverts:         []revertView{},
	}
	for _, id := range reviewers {
		view.Reviewers = append(view.Reviewers, personOf(id, people))
	}
	for _, change := range changes {
		view.Changes.Rows = append(view.Changes.Rows, changeViewOf(change))
	}
	for _, item := range items {
		switch item.Kind {
		case registry.ReviewItemReview:
			view.Review = &attestationView{
				Outcome: item.Outcome, Note: optionalString(item.Note), ReplacementIssued: item.ReplacementIssued,
				Reviewer: personOf(item.UserID, people), ReviewedAt: item.CreatedAt,
			}
		case registry.ReviewItemReviewerChange:
			view.ReviewerChanges = append(view.ReviewerChanges, reviewerChangeView{
				Person: personOf(item.UserID, people), Actor: personOf(item.ActorUserID, people),
				Action: item.Action, At: item.CreatedAt,
			})
		case registry.ReviewItemRevert:
			view.Reverts = append(view.Reverts, revertViewOf(registry.RevertEntry{
				ReviewItem: item, HolderUserID: elevation.UserID,
				Change: registry.ElevationAuditRow{ID: item.AuditEventID, Action: item.Action, Flagged: true, At: item.CreatedAt},
			}, people))
		}
	}
	writeElevationJSON(w, status, view)
}

func (h *ReviewsHandler) people(w http.ResponseWriter, r *http.Request, ids []int64) (map[int64]Person, bool) {
	ids = slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return id <= 0 })
	slices.Sort(ids)
	people, err := h.store.People(r.Context(), slices.Compact(ids))
	if err != nil {
		h.writeReviewError(w, r, err)
		return nil, false
	}
	return people, true
}

func personOf(id int64, people map[int64]Person) personBody {
	person := people[id]
	return personBody{ID: id, Name: person.Name, Email: person.Email}
}

func changeViewOf(c registry.ElevationAuditRow) changeView {
	view := changeView{
		ID: c.ID, At: c.At, UserID: c.UserID, Action: c.Action, Method: c.Method, Route: c.Route, Status: c.Status,
		EntityType: optionalString(c.EntityType), EntityName: optionalString(c.EntityName), Flagged: c.Flagged,
	}
	if c.EntityID != 0 {
		id := c.EntityID
		view.EntityID = &id
	}
	return view
}

func revertViewOf(e registry.RevertEntry, people map[int64]Person) revertView {
	view := revertView{
		ID: e.ID.String(), ElevationID: e.ElevationID.String(), Holder: personOf(e.HolderUserID, people),
		Change: changeViewOf(e.Change), OpenedAt: e.CreatedAt, HandledAt: optionalTime(e.HandledAt),
	}
	if e.HandledBy > 0 {
		by := personOf(e.HandledBy, people)
		view.HandledBy = &by
	}
	return view
}

func browserSessionRef(ref int64) string {
	if ref <= 0 {
		return ""
	}
	return fmt.Sprintf("browser_session:%d", ref)
}

// writeReviewError maps the review's refusals to their status and code.
func (h *ReviewsHandler) writeReviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, registry.ErrElevationNotFound):
		writeElevationRefusal(w, http.StatusNotFound, CodeElevationNotFound, "no such elevation")
	case errors.Is(err, registry.ErrRevertNotFound):
		writeElevationRefusal(w, http.StatusNotFound, CodeRevertNotFound, "no such revert entry")
	case errors.Is(err, registry.ErrReviewerInvolved):
		writeElevationRefusal(w, http.StatusForbidden, CodeElevationReviewerInvolved,
			"the holder, the requester and the approver of an elevation do not review it")
	case errors.Is(err, registry.ErrReviewerNotEligible):
		writeElevationRefusal(w, http.StatusForbidden, CodeElevationReviewerNotEligible,
			"only a reviewer recorded when the elevation ended reviews it")
	case errors.Is(err, registry.ErrElevationNotReviewable):
		writeElevationRefusal(w, http.StatusConflict, CodeElevationNotReviewable,
			"the elevation is not awaiting review: it has not ended, it was reviewed already, "+
				"or nobody independent could review it by its deadline")
	case errors.Is(err, registry.ErrElevationInvalid):
		writeElevationRefusal(w, http.StatusBadRequest, CodeElevationRequestInvalid, err.Error())
	case errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
	default:
		(&ElevationsHandler{}).writeReadError(w, r, err)
	}
}
