package adminaccess_test

// The review after use on PostgreSQL (ADR-0059 R2.9a, issue 2523): the
// review routes over the real ledger (shared 0269), its items (0283), the
// admin audit rows (centry.audit_events with 0268/0275) and the security
// audit outbox. The gates let every browser session through; review_test.go
// covers them.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const reviewNoteMarker = "REVIEW-NOTE-5d1e the routing change matches the ticket"

type reviewHarness struct {
	pool                      *pgxpool.Pool
	holder, approver, auditor int64
	admin                     int64
	holderRef, auditorRef     int64
	adminRef                  int64
	now                       time.Time
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	pool := newRequestDatabase(t)
	h := &reviewHarness{pool: pool, now: ledgerClock.Add(2 * time.Hour)}
	h.holder, h.holderRef = seedAccount(t, pool, registry.RoleEmergencyAccess)
	h.approver, _ = seedAccount(t, pool, registry.RoleSuperAdmin)
	h.auditor, h.auditorRef = seedAccount(t, pool, registry.RoleSecurityAuditor)
	h.admin, h.adminRef = seedAccount(t, pool, registry.RoleAdmin)
	return h
}

func (h *reviewHarness) appender(t *testing.T) securityaudit.Appender {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/elevation-review-test")
	if err != nil {
		t.Fatal(err)
	}
	return appender
}

func (h *reviewHarness) router(t *testing.T) http.Handler {
	t.Helper()
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	console.NewReviewsHandler(console.NewPostgresReviews(h.pool, h.appender(t))).
		WithClock(func() time.Time { return h.now }).
		Register(r, console.ReviewGates{Review: pass, Manage: pass})
	return r
}

// ended starts an elevation of the holder on path and ends it by its holder.
func (h *reviewHarness) ended(t *testing.T, path string) registry.ElevationRecord {
	t.Helper()
	start := registry.ElevationStart{
		UserID: h.holder, BrowserSessionRef: h.holderRef, Level: registry.ElevationLevelAdmin, Path: path,
		ReasonCode: registry.ElevationReasonIncident, Reason: "the provider is down",
		At: ledgerClock.Add(time.Minute), Duration: time.Hour,
	}
	if path == registry.ElevationPathApproved {
		start.ApproverUserID = h.approver
	}
	var elevation registry.ElevationRecord
	if err := pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
		started, err := registry.StartElevation(t.Context(), tx, h.appender(t), start)
		if err != nil {
			return err
		}
		elevation, _, err = registry.EndElevation(t.Context(), tx, h.appender(t), registry.ElevationEnd{
			ID: started.ID, Reason: registry.ElevationEndHolder, At: ledgerClock.Add(20 * time.Minute),
		})
		return err
	}); err != nil {
		t.Fatalf("start and end an elevation: %v", err)
	}
	return elevation
}

// auditRow writes one admin audit row, under the elevation when id is set.
func (h *reviewHarness) auditRow(t *testing.T, id *uuid.UUID, flagged bool, minute int) int64 {
	t.Helper()
	var row int64
	if err := h.pool.QueryRow(t.Context(), `
INSERT INTO centry.audit_events
    (timestamp, user_id, event_type, action, http_method, http_route, status_code, elevation_id, elevation_flagged)
VALUES ($1, $2, 'api', 'gateway.provider.update', 'PUT', '/api/v2/admin/gateway/providers/administration/{providerID}',
        200, $3, $4)
RETURNING id`, ledgerClock.Add(time.Duration(minute)*time.Minute), h.holder, id, flagged).Scan(&row); err != nil {
		t.Fatalf("seed an audit row: %v", err)
	}
	return row
}

type reviewResponse struct {
	Elevation struct {
		State string `json:"state"`
	} `json:"elevation"`
	CanReview bool `json:"can_review"`
	Review    *struct {
		Outcome           string `json:"outcome"`
		Note              string `json:"note"`
		ReplacementIssued *bool  `json:"replacement_issued"`
	} `json:"review"`
	ReviewerChanges []struct {
		Action string `json:"action"`
	} `json:"reviewer_changes"`
	Changes struct {
		Total int `json:"total"`
		Rows  []struct {
			ID      int64 `json:"id"`
			Flagged bool  `json:"flagged"`
		} `json:"rows"`
	} `json:"changes"`
	Reverts []struct {
		ID string `json:"id"`
	} `json:"reverts"`
}

func decodeReview(t *testing.T, body []byte) reviewResponse {
	t.Helper()
	var review reviewResponse
	if err := json.Unmarshal(body, &review); err != nil {
		t.Fatalf("decode the review: %v: %s", err, body)
	}
	return review
}

func reviewPath(id uuid.UUID) string { return "/access/elevations/" + id.String() + "/review" }

// The review lists exactly the audit rows of the elevation (3 inside, 2
// outside), flagged rows first.
func TestTheReviewListsExactlyTheChangesOfTheElevationFlaggedFirst(t *testing.T) {
	h := newReviewHarness(t)
	elevation := h.ended(t, registry.ElevationPathApproved)
	other := uuid.New()
	inside := []int64{
		h.auditRow(t, &elevation.ID, false, 5),
		h.auditRow(t, &elevation.ID, false, 6),
		h.auditRow(t, &elevation.ID, true, 7),
	}
	h.auditRow(t, &other, true, 5)
	h.auditRow(t, nil, true, 5)

	rec := call(t, h.router(t), sessionOf(h.auditor, h.auditorRef), http.MethodGet, reviewPath(elevation.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read the review: %d %s", rec.Code, rec.Body)
	}
	review := decodeReview(t, rec.Body.Bytes())
	if review.Changes.Total != 3 || len(review.Changes.Rows) != 3 {
		t.Fatalf("changes = %+v, want the 3 rows of the elevation", review.Changes)
	}
	got := []int64{}
	for _, row := range review.Changes.Rows {
		got = append(got, row.ID)
	}
	if !review.Changes.Rows[0].Flagged || review.Changes.Rows[0].ID != inside[2] {
		t.Fatalf("the flagged row is not first: %+v", review.Changes.Rows)
	}
	slices.Sort(got)
	if !slices.Equal(got, inside) {
		t.Fatalf("changes = %v, want %v", got, inside)
	}
	if !review.CanReview || review.Elevation.State != registry.ElevationReviewPending {
		t.Fatalf("the recorded auditor cannot review a pending elevation: %+v", review)
	}
}

// An `unauthorized` outcome opens one revert entry per flagged change,
// notifies, and the platform administrators work the list.
func TestAnUnauthorizedOutcomeOpensOneRevertEntryPerFlaggedChange(t *testing.T) {
	h := newReviewHarness(t)
	elevation := h.ended(t, registry.ElevationPathApproved)
	h.auditRow(t, &elevation.ID, true, 5)
	h.auditRow(t, &elevation.ID, true, 6)
	h.auditRow(t, &elevation.ID, false, 7)
	router := h.router(t)

	rec := call(t, router, sessionOf(h.auditor, h.auditorRef), http.MethodPost, reviewPath(elevation.ID),
		`{"outcome":"unauthorized","note":"no ticket covers these changes"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	review := decodeReview(t, rec.Body.Bytes())
	if len(review.Reverts) != 2 || review.Review == nil || review.Review.Outcome != registry.ReviewOutcomeUnauthorized ||
		review.Elevation.State != registry.ElevationReviewed {
		t.Fatalf("after an unauthorized review: %+v", review)
	}
	var notices int
	if err := h.pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_auth.admin_access_notices WHERE elevation_id = $1 AND kind = $2`,
		elevation.ID, string(registry.NoticeReviewUnauthorized)).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices == 0 {
		t.Fatal("an unauthorized review notified nobody")
	}

	admin := sessionOf(h.admin, h.adminRef)
	list := call(t, router, admin, http.MethodGet, "/access/reverts?open=true", "")
	var body struct {
		Total int `json:"total"`
		Rows  []struct {
			ID     string `json:"id"`
			Change struct {
				Flagged bool `json:"flagged"`
			} `json:"change"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || list.Code != http.StatusOK || body.Total != 2 {
		t.Fatalf("the revert list: %d %s", list.Code, list.Body)
	}
	handled := "/access/reverts/" + body.Rows[0].ID + "/handled"
	for i, want := range []bool{true, false} {
		rec := call(t, router, admin, http.MethodPost, handled, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":`+map[bool]string{true: "true", false: "false"}[want]) {
			t.Fatalf("mark %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	open := call(t, router, admin, http.MethodGet, "/access/reverts?open=true", "")
	if err := json.Unmarshal(open.Body.Bytes(), &body); err != nil || body.Total != 1 {
		t.Fatalf("after one mark the open list is %s", open.Body)
	}
	// A second review is refused: the attestation is written once.
	again := call(t, router, sessionOf(h.auditor, h.auditorRef), http.MethodPost, reviewPath(elevation.ID),
		`{"outcome":"justified"}`)
	if again.Code != http.StatusConflict || refusalCode(t, again) != console.CodeElevationNotReviewable {
		t.Fatalf("a second review: %d %s", again.Code, again.Body)
	}
}

// The review of a sealed elevation says whether a replacement credential was
// issued; the `reviewed` event commits with it and carries no note text.
func TestASealedReviewRecordsTheReplacementAndTheEventCarriesNoNote(t *testing.T) {
	h := newReviewHarness(t)
	elevation := h.ended(t, registry.ElevationPathSealed)
	router := h.router(t)
	auditor := sessionOf(h.auditor, h.auditorRef)

	if rec := call(t, router, auditor, http.MethodPost, reviewPath(elevation.ID), `{"outcome":"justified"}`); rec.Code !=
		http.StatusBadRequest {
		t.Fatalf("a sealed review without replacement_issued: %d %s", rec.Code, rec.Body)
	}
	rec := call(t, router, auditor, http.MethodPost, reviewPath(elevation.ID),
		`{"outcome":"justified","note":"`+reviewNoteMarker+`","replacement_issued":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	review := decodeReview(t, rec.Body.Bytes())
	if review.Review == nil || review.Review.ReplacementIssued == nil || !*review.Review.ReplacementIssued ||
		review.Review.Note != reviewNoteMarker {
		t.Fatalf("the sealed review: %+v", review.Review)
	}
	var raw string
	if err := h.pool.QueryRow(t.Context(), `
SELECT event::text FROM elitea_runtime.security_audit_outbox
WHERE event -> 'data' ->> 'action' = $1 AND event -> 'data' -> 'elevation' ->> 'elevation_id' = $2`,
		securityaudit.ElevationReviewed.EventAction(), elevation.ID.String()).Scan(&raw); err != nil {
		t.Fatalf("the reviewed event: %v", err)
	}
	if strings.Contains(raw, "REVIEW-NOTE-5d1e") {
		t.Fatalf("the reviewed event carries the note text: %s", raw)
	}
	for _, reason := range []string{"outcome:justified", "replacement_issued:true"} {
		if !strings.Contains(raw, reason) {
			t.Fatalf("the reviewed event has no %q: %s", reason, raw)
		}
	}
}
