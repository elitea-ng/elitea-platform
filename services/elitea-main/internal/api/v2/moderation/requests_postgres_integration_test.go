package moderation_test

// Unit A14 acceptance for app requests / moderation (issue #200).
//
// The bar from #130/#180/#207: every write below is asserted by WRITING and
// then RE-READING — through the product's own handlers AND, separately, by SQL.
// A status code proves nothing on this surface in particular, because every
// endpoint it replaces already answered 200:
//
//   - the admin queue was a `_ *http.Request` stub returning a fixed empty
//     page, so "nobody has asked for anything" and "this deployment cannot see
//     the queue" were the same screen;
//   - the decision endpoint had no route at all;
//   - the per-entity read answered `{"status":"approved"}` to every caller for
//     every entity, and the POST beside it created nothing. That is the one
//     worth naming twice: it is a gate that always says yes, fed by a button
//     that writes nowhere.
//
// Plus the boundary this unit adds deliberately. A moderation row has two
// authors who are not equally trusted, and neither may write the other's
// fields: TestRequesterCannotApproveTheirOwnRequest and
// TestDecisionRefusesToRewriteWhatWasRequested.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/moderation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type requestRow struct {
	ID               int64     `json:"id"`
	UserID           int64     `json:"user_id"`
	UserEmail        string    `json:"user_email"`
	ProjectID        int64     `json:"project_id"`
	IssueType        string    `json:"issue_type"`
	EntityID         string    `json:"entity_id"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	RejectionComment *string   `json:"rejection_comment"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	CreatedProjectID *int64    `json:"created_project_id,omitempty"`
}

type requestListing struct {
	Rows  []requestRow `json:"rows"`
	Total int          `json:"total"`
}

// The wire value of `pending`, spelled out here rather than imported: the test
// asserts what a CLIENT sends, and a constant shared with the handler would
// move with it.
const statusPendingLiteral = "pending"

const (
	queueURL    = "/admin/moderation_statuses/administration"
	decisionURL = "/admin/moderation_status/administration"
)

// moderationRouter mounts all five routes exactly as internal/api/router.go
// does, minus the route-level permission middleware, and injects `principal` so
// the three project-scoped handlers have an author to attribute a request to.
//
// It mounts BOTH the static `administration` registrations and the `{mode}`
// pair, because that composition is load-bearing: a static segment binds no URL
// parameter, so a handler that read `chi.URLParam(r, "mode")` would see an empty
// string on exactly the administration requests it exists for. #207's tests
// caught that; mounting only one shape would hide it again.
func moderationRouter(handler *moderation.Handler, principal auth.User) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Get(queueURL, handler.AdministrationRequests)
	router.Put(decisionURL, handler.AdministrationRequestUpdate)
	router.Get("/admin/moderation_status/{mode}/{projectID}/{entityID}", handler.Requests)
	router.Post("/admin/moderation_status/{mode}/{projectID}/{entityID}", handler.RequestCreate)
	router.Delete("/admin/moderation_status/{mode}/{projectID}/{entityID}", handler.RequestDelete)
	return router
}

func moderationDo(
	t *testing.T, router chi.Router, method, target string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	reader := bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// readQueue re-reads through the SAME GET handler the admin page calls. This is
// the assertion an unwired or no-op write cannot pass.
func readQueue(t *testing.T, router chi.Router, query string) requestListing {
	t.Helper()
	target := queueURL
	if query != "" {
		target += "?" + query
	}
	recorder := moderationDo(t, router, http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", target, recorder.Code, recorder.Body.String())
	}
	var listing requestListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode queue body %q: %v", recorder.Body.String(), err)
	}
	return listing
}

func entityURL(projectID int, entityID string) string {
	return fmt.Sprintf("/admin/moderation_status/default/%d/%s", projectID, entityID)
}

// requestSQL bypasses the read handlers entirely. A re-read through GET proves
// the product agrees with itself; this proves the ROW changed, so a read that
// synthesised the expected value could not pass both.
func requestSQL(t *testing.T, pool *pgxpool.Pool, id int64) (status string, comment *string, meta *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, rejection_comment, meta::text FROM centry.moderation_state WHERE id = $1`, id).
		Scan(&status, &comment, &meta); err != nil {
		t.Fatalf("read moderation row %d: %v", id, err)
	}
	return status, comment, meta
}

func countRequests(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.moderation_state`).Scan(&total); err != nil {
		t.Fatalf("count moderation rows: %v", err)
	}
	return total
}

// The two personas every test uses. `auth_core__user` rows exist so the queue's
// email join has something to resolve — the column is what the "Requesting
// User" column renders, and an unjoined listing looks identical to a joined one
// until a real address is expected.
const usersFixture = `
INSERT INTO auth_core__user (id, email, name) VALUES
    (4001, 'ada@example.com',  'Ada Lovelace'),
    (4002, 'grace@example.com','Grace Hopper')
ON CONFLICT (id) DO NOTHING;
SELECT setval(pg_get_serial_sequence('auth_core__user','id'), 5000, true);
`

var (
	ada   = auth.User{ID: "4001", UserID: "4001", Email: "ada@example.com"}
	grace = auth.User{ID: "4002", UserID: "4002", Email: "grace@example.com"}
)

// requestsFixture seeds the queue directly, for the read-shaped tests. The
// write-shaped tests go through the POST handler instead, so neither kind can
// pass on the strength of the other's setup.
const requestsFixture = `
INSERT INTO centry.moderation_state
    (user_id, project_id, issue_type, entity_id, description, status, rejection_comment, created_at) VALUES
    (4001, 1, 'Wikis',     'wikis_Wikis', 'We need the wiki toolkit for onboarding docs.', 'pending',  NULL, '2026-08-01 09:00:00'),
    (4001, 1, 'Inventory', 'inventory',   'Asset tracking for the hardware lab.',          'approved', NULL, '2026-08-02 09:00:00'),
    (4002, 2, 'Wikis',     'wikis_Wikis', 'Second team wants the same toolkit.',           'rejected', 'Not licensed for this tenant.', '2026-08-03 09:00:00');
`

func prepareFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, statement := range []string{usersFixture, requestsFixture} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}

func prepareUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), usersFixture); err != nil {
		t.Fatalf("seed users: %v", err)
	}
}

func rowByDescription(t *testing.T, listing requestListing, needle string) requestRow {
	t.Helper()
	for _, row := range listing.Rows {
		if strings.Contains(row.Description, needle) {
			return row
		}
	}
	t.Fatalf("listing has no row matching %q (has %d rows)", needle, len(listing.Rows))
	return requestRow{}
}

/* ── the round trip ────────────────────────────────────────────────────── */

// TestARequestFiledByTheProductAppearsInTheAdminQueue is the whole point of the
// unit in one test: the catalogue's "Request Access" button and the operator's
// queue must be two ends of one table. Before this unit the POST created nothing
// and the queue returned a constant, so both ends passed their own tests while
// nothing connected them.
func TestARequestFiledByTheProductAppearsInTheAdminQueue(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
		"issue_type":  "Wikis",
		"description": "We need the wiki toolkit for onboarding docs.",
		// Both shipped clients send these two. They are tolerated, not applied.
		"status": "pending",
		"meta":   map[string]any{},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}

	listing := readQueue(t, router, "")
	if listing.Total != 1 {
		t.Fatalf("queue total = %d, want 1 (body had %d rows)", listing.Total, len(listing.Rows))
	}
	row := listing.Rows[0]
	if row.Status != "pending" {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.EntityID != "wikis_Wikis" {
		t.Errorf("entity_id = %q, want wikis_Wikis — the path segment, not the body", row.EntityID)
	}
	if row.IssueType != "Wikis" {
		t.Errorf("issue_type = %q, want Wikis", row.IssueType)
	}
	if row.ProjectID != 1 {
		t.Errorf("project_id = %d, want 1", row.ProjectID)
	}
	// Authorship comes from the principal, never from the body.
	if row.UserID != 4001 {
		t.Errorf("user_id = %d, want 4001 (the authenticated caller)", row.UserID)
	}
	// The column the queue's "Requesting User" cell renders. Resolved by join,
	// not carried on the row.
	if row.UserEmail != "ada@example.com" {
		t.Errorf("user_email = %q, want ada@example.com", row.UserEmail)
	}
	if row.RejectionComment != nil {
		t.Errorf("rejection_comment = %q on a fresh request, want null", *row.RejectionComment)
	}
}

// TestPerEntityReadAnswersOnlyTheCallersOwnRequests replaces the fail-open stub.
// The endpoint's question is "has MY request been decided", and answering it
// from someone else's row — or from a constant — is the defect this unit exists
// to remove.
func TestPerEntityReadAnswersOnlyTheCallersOwnRequests(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	handler := moderation.NewHandler(pool)

	// Grace has an APPROVED request for `inventory` in project 1 seeded for Ada.
	// Before this unit both callers were told "approved" regardless.
	graceRouter := moderationRouter(handler, grace)
	recorder := moderationDo(t, graceRouter, http.MethodGet, entityURL(1, "inventory"), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", recorder.Code)
	}
	var listing requestListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.Total != 0 {
		t.Fatalf("grace sees %d rows for a request ada filed; want 0", listing.Total)
	}

	// Ada sees her own, with its real status.
	adaRouter := moderationRouter(handler, ada)
	recorder = moderationDo(t, adaRouter, http.MethodGet, entityURL(1, "inventory"), nil)
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.Total != 1 || listing.Rows[0].Status != "approved" {
		t.Fatalf("ada's own request = %+v, want one approved row", listing.Rows)
	}

	// And an entity nobody has asked about is NOT approved — the fail-open case.
	recorder = moderationDo(t, adaRouter, http.MethodGet, entityURL(1, "never_requested"), nil)
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.Total != 0 {
		t.Fatalf("an entity nobody requested returned %d rows; the stub answered 'approved' here",
			listing.Total)
	}
}

/* ── the queue's read ──────────────────────────────────────────────────── */

// TestQueueReadsEveryProjectsRequests: the admin queue is deliberately
// cross-tenant, and its `total` describes the FILTERED set so the page's
// pagination cannot advertise pages that are not there.
func TestQueueReadsEveryProjectsRequests(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	all := readQueue(t, router, "")
	if all.Total != 3 || len(all.Rows) != 3 {
		t.Fatalf("queue = %d rows / total %d, want 3/3", len(all.Rows), all.Total)
	}

	for query, want := range map[string]int{
		"status=pending":          1,
		"status=approved":         1,
		"project_id=2":            1,
		"entity_id=wikis_Wikis":   2,
		"issue_type=Inventory":    1,
		"search=grace":            1,
		"search=Ada":              2,
		"search=nobody@nowhere":   0,
		"project_id=not-a-number": 0,
	} {
		listing := readQueue(t, router, query)
		if listing.Total != want {
			t.Errorf("%s: total = %d, want %d", query, listing.Total, want)
		}
		if len(listing.Rows) != want {
			t.Errorf("%s: rows = %d, want %d (total said %d)", query, len(listing.Rows), want, listing.Total)
		}
	}
}

// TestQueuePagesWithoutRepeatingOrDroppingRows guards the `m.id` ORDER BY
// tiebreaker. `created_at` defaults to NOW(), so a burst of requests shares a
// timestamp; ordering on it alone is not a total order and PostgreSQL may return
// tied rows differently per page.
func TestQueuePagesWithoutRepeatingOrDroppingRows(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.moderation_state (user_id, project_id, issue_type, entity_id, description, status, created_at)
SELECT 4001, 1, 'Wikis', 'wikis_Wikis', 'burst ' || n, 'pending', '2026-08-04 12:00:00'
FROM generate_series(1, 10) AS n;`); err != nil {
		t.Fatalf("seed burst: %v", err)
	}
	router := moderationRouter(moderation.NewHandler(pool), ada)

	seen := map[int64]bool{}
	for offset := 0; offset < 10; offset += 4 {
		listing := readQueue(t, router, fmt.Sprintf("limit=4&offset=%d&sort_by=created_at", offset))
		if listing.Total != 10 {
			t.Fatalf("total = %d at offset %d, want 10", listing.Total, offset)
		}
		for _, row := range listing.Rows {
			if seen[row.ID] {
				t.Fatalf("row %d appeared on two pages", row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("paging returned %d distinct rows out of 10", len(seen))
	}
}

// TestQueueSortsOnTheColumnItWasAskedFor. pylon resolves `sort_by` with
// `getattr` and emits NO ordering when the attribute is missing, which under
// LIMIT/OFFSET is the unordered-paging bug above. An unknown column here falls
// back to `created_at` rather than to nothing.
func TestQueueSortsOnTheColumnItWasAskedFor(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	descending := readQueue(t, router, "sort_by=created_at&sort_order=desc")
	if descending.Rows[0].Description != "Second team wants the same toolkit." {
		t.Errorf("newest-first head = %q, want the 2026-08-03 row", descending.Rows[0].Description)
	}
	ascending := readQueue(t, router, "sort_by=created_at&sort_order=asc")
	if ascending.Rows[0].Description == descending.Rows[0].Description {
		t.Error("asc and desc returned the same first row; sort_order is not applied")
	}

	// An unknown column must still produce an ordering, and the allow-list is
	// also what keeps the interpolated ORDER BY injection-free.
	unknown := readQueue(t, router,
		"sort_by="+url.QueryEscape("rejection_comment; DROP TABLE centry.moderation_state--"))
	if unknown.Total != 3 {
		t.Fatalf("unknown sort column returned total %d, want 3", unknown.Total)
	}
	if countRequests(t, pool) != 3 {
		t.Fatal("the sort_by probe reached the database")
	}
}

// TestQueueSurfacesAFailureInsteadOfAnEmptyPage. The stub this replaces
// answered `{"rows":[],"total":0}` unconditionally; an operator reading that
// during an incident concludes there is nothing to act on.
func TestQueueSurfacesAFailureInsteadOfAnEmptyPage(t *testing.T) {
	pool := newModerationPool(t)
	if _, err := pool.Exec(context.Background(), `DROP TABLE centry.moderation_state`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	router := moderationRouter(moderation.NewHandler(pool), ada)

	recorder := moderationDo(t, router, http.MethodGet, queueURL, nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("queue read against a missing table status = %d, want 500 (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

/* ── the decision ──────────────────────────────────────────────────────── */

// TestApprovalIsRecordedAndTellsTheRequester. Approving grants no capability —
// nothing in either stack reads an approved row and unlocks anything — so the
// notification IS the outcome, and it is written in the same transaction as the
// status so a recorded decision cannot go undelivered.
func TestApprovalIsRecordedAndTellsTheRequester(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), grace)

	pending := rowByDescription(t, readQueue(t, router, "status=pending"), "onboarding docs")

	recorder := moderationDo(t, router, http.MethodPut, decisionURL,
		map[string]any{"id": pending.ID, "status": "approved"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	status, comment, _ := requestSQL(t, pool, pending.ID)
	if status != "approved" {
		t.Fatalf("row status = %q after approving, want approved", status)
	}
	if comment != nil {
		t.Errorf("rejection_comment = %q on an approval, want null", *comment)
	}

	// Re-read through the product's own listing, not only by SQL.
	if row := rowByDescription(t, readQueue(t, router, ""), "onboarding docs"); row.Status != "approved" {
		t.Fatalf("the listing still reports %q after the approval", row.Status)
	}

	var eventType, meta string
	var notifiedUser int64
	if err := pool.QueryRow(context.Background(), `
SELECT event_type, user_id, meta::text FROM centry.notifications ORDER BY id DESC LIMIT 1`).
		Scan(&eventType, &notifiedUser, &meta); err != nil {
		t.Fatalf("no notification was written for the decision: %v", err)
	}
	if eventType != "moderation_approved" {
		t.Errorf("event_type = %q, want moderation_approved", eventType)
	}
	if notifiedUser != 4001 {
		t.Errorf("notification went to user %d, want the REQUESTER 4001", notifiedUser)
	}
	// The renderers in both frontends have no branch for this event type, so the
	// sentence has to travel with the row.
	if !strings.Contains(meta, "has been approved") {
		t.Errorf("notification meta = %s, want a rendered message", meta)
	}
}

// TestRejectionRequiresAReason. pylon declares a pydantic validator for this and
// it does not fire when the key is ABSENT, so `{"id":n,"status":"rejected"}`
// rejects with a null reason there — and a bare refusal is what the requester
// receives.
func TestRejectionRequiresAReason(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), grace)
	pending := rowByDescription(t, readQueue(t, router, "status=pending"), "onboarding docs")

	for name, body := range map[string]map[string]any{
		"absent": {"id": pending.ID, "status": "rejected"},
		"null":   {"id": pending.ID, "status": "rejected", "rejection_comment": nil},
		"blank":  {"id": pending.ID, "status": "rejected", "rejection_comment": "   "},
	} {
		recorder := moderationDo(t, router, http.MethodPut, decisionURL, body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("reject with a %s reason status = %d, want 400", name, recorder.Code)
		}
		if status, _, _ := requestSQL(t, pool, pending.ID); status != "pending" {
			t.Fatalf("a refused rejection moved the row to %q", status)
		}
	}

	recorder := moderationDo(t, router, http.MethodPut, decisionURL, map[string]any{
		"id": pending.ID, "status": "rejected", "rejection_comment": "Not licensed for this tenant.",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("reject WITH a reason status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	status, comment, _ := requestSQL(t, pool, pending.ID)
	if status != "rejected" || comment == nil || *comment != "Not licensed for this tenant." {
		t.Fatalf("row = (%q, %v), want rejected with the reason stored", status, comment)
	}
}

// TestDecisionRefusesToRewriteWhatWasRequested is the moderator half of the
// security boundary. If the decision endpoint could edit `entity_id`,
// `issue_type` or `description`, an approved row would stop being evidence of
// what was approved; `meta` is refused because this endpoint REPLACES it, so a
// decision would silently destroy what the requester stored.
//
// Refused with 400, never ignored: a moderator who believes they retargeted a
// request and got a 200 has been told something false.
func TestDecisionRefusesToRewriteWhatWasRequested(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), grace)
	pending := rowByDescription(t, readQueue(t, router, "status=pending"), "onboarding docs")

	forgeries := []map[string]any{
		{"id": pending.ID, "status": "approved", "entity_id": "some_other_app"},
		{"id": pending.ID, "status": "approved", "issue_type": "Something Else"},
		{"id": pending.ID, "status": "approved", "description": "rewritten after the fact"},
		{"id": pending.ID, "status": "approved", "user_id": 4002},
		{"id": pending.ID, "status": "approved", "project_id": 99},
		{"id": pending.ID, "status": "approved", "meta": map[string]any{"granted": true}},
	}
	for _, body := range forgeries {
		recorder := moderationDo(t, router, http.MethodPut, decisionURL, body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("decision carrying %v status = %d, want 400", body, recorder.Code)
		}
	}

	// Nothing moved: not the status, and not the fields the forgeries named.
	status, _, meta := requestSQL(t, pool, pending.ID)
	if status != "pending" {
		t.Fatalf("a refused decision applied the status anyway (%q)", status)
	}
	if meta != nil {
		t.Fatalf("a refused decision wrote meta = %q", *meta)
	}
	row := rowByDescription(t, readQueue(t, router, ""), "onboarding docs")
	if row.EntityID != "wikis_Wikis" || row.IssueType != "Wikis" || row.UserID != 4001 {
		t.Fatalf("a refused decision rewrote the request: %+v", row)
	}
}

// TestDecisionRefusesToReopenAndToInventStatuses. pylon lets a decided request
// be moved back to `pending` with no record that it was ever answered, and
// accepts nothing else because its enum is closed; the enum is the part worth
// keeping.
func TestDecisionRefusesToReopenAndToInventStatuses(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), grace)
	decided := rowByDescription(t, readQueue(t, router, "status=approved"), "hardware lab")

	for _, status := range []any{"pending", "granted", "", nil} {
		body := map[string]any{"id": decided.ID}
		if status != nil {
			body["status"] = status
		}
		if recorder := moderationDo(t, router, http.MethodPut, decisionURL, body); recorder.Code != http.StatusBadRequest {
			t.Errorf("decision with status %v = %d, want 400", status, recorder.Code)
		}
	}

	// `pending` is refused for its OWN reason, and says so. Folding it into the
	// generic "status must be approved or rejected" would leave an operator who
	// tried to reopen a request believing the value was merely misspelled.
	recorder := moderationDo(t, router, http.MethodPut, decisionURL,
		map[string]any{"id": decided.ID, "status": statusPendingLiteral})
	if !strings.Contains(recorder.Body.String(), "cannot be returned to pending") {
		t.Errorf("reopening answered %q, want the reopen-specific reason", recorder.Body.String())
	}
	if status, _, _ := requestSQL(t, pool, decided.ID); status != "approved" {
		t.Fatalf("a refused decision moved the row to %q", status)
	}
}

// TestDecisionOnAMissingRequestIsNotFound: an id that matches nothing is a 404,
// not a 200 that changed nothing.
func TestDecisionOnAMissingRequestIsNotFound(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), grace)

	recorder := moderationDo(t, router, http.MethodPut, decisionURL,
		map[string]any{"id": 999999, "status": "approved"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("decision on a missing id status = %d, want 404", recorder.Code)
	}
}

/* ── the requester's half of the boundary ──────────────────────────────── */

// TestRequesterCannotApproveTheirOwnRequest. pylon's `ModerationStateCreate`
// declares `status: ModerationStatus = PENDING`, which is a DEFAULT and not a
// restriction: a body carrying `"status": "approved"` is stored verbatim, so any
// project member holding `admin.moderation.create` can file a request that is
// already decided and land it in the operator's queue as answered.
func TestRequesterCannotApproveTheirOwnRequest(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	for _, status := range []string{"approved", "rejected"} {
		recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
			"issue_type": "Wikis", "description": "let me in", "status": status,
		})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("POST with status=%s = %d, want 400", status, recorder.Code)
		}
	}
	if total := countRequests(t, pool); total != 0 {
		t.Fatalf("a refused create wrote %d rows", total)
	}

	// And the same request without the field succeeds, pending — so the refusal
	// cannot be passing for an unrelated reason.
	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
		"issue_type": "Wikis", "description": "let me in",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST without a status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}
	if row := readQueue(t, router, "").Rows[0]; row.Status != "pending" {
		t.Fatalf("a fresh request is %q, want pending", row.Status)
	}
}

// TestRequesterCannotForgeAuthorship. pylon accepts `user_id` on both the create
// and the update models and then ignores it, so a caller cannot tell that
// authorship — and therefore who the decision notification is delivered to — is
// not theirs to choose. Refused rather than ignored, for that reason.
func TestRequesterCannotForgeAuthorship(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
		"issue_type": "Wikis", "description": "filed as someone else", "user_id": 4002,
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST carrying user_id = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if total := countRequests(t, pool); total != 0 {
		t.Fatalf("a refused create wrote %d rows", total)
	}
}

// TestRequesterCannotWriteMeta. Nothing in either stack reads a key out of
// `meta`, so storing client JSON there buys nothing and would be the first thing
// a future consumer wrongly trusted. An EMPTY object is tolerated because both
// shipped clients send `{}` and refusing that would break them for no gain.
func TestRequesterCannotWriteMeta(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
		"issue_type": "Wikis", "description": "with baggage",
		"meta": map[string]any{"message": "Your request has been approved."},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST carrying a non-empty meta = %d, want 400", recorder.Code)
	}
	if total := countRequests(t, pool); total != 0 {
		t.Fatalf("a refused create wrote %d rows", total)
	}
}

// TestCreateValidatesWhatPostgresWouldOtherwiseReject. `issue_type` is
// VARCHAR(256); an over-long value is a 500 from PostgreSQL otherwise, which
// reads to the user as "the request failed to send".
func TestCreateValidatesWhatPostgresWouldOtherwiseReject(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	for name, body := range map[string]map[string]any{
		"no issue_type":     {"description": "d"},
		"blank issue_type":  {"issue_type": "  ", "description": "d"},
		"no description":    {"issue_type": "Wikis"},
		"blank description": {"issue_type": "Wikis", "description": " "},
		"long issue_type":   {"issue_type": strings.Repeat("x", 257), "description": "d"},
	} {
		if recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "e"), body); recorder.Code != http.StatusBadRequest {
			t.Errorf("POST with %s = %d, want 400", name, recorder.Code)
		}
	}
	if total := countRequests(t, pool); total != 0 {
		t.Fatalf("refused creates wrote %d rows", total)
	}
}

// TestATokenPrincipalCannotFileARequest. `OwningUserID` refuses a token
// principal as an author, which is the behaviour wanted rather than an
// obstacle: a request filed by an API key has nobody to notify of the decision
// and nobody for the operator to answer.
func TestATokenPrincipalCannotFileARequest(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool),
		auth.User{ID: "4001", TokenID: "77", AuthType: "token"})

	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "wikis_Wikis"), map[string]any{
		"issue_type": "Wikis", "description": "from a script",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST as a token principal = %d, want 403", recorder.Code)
	}
	if total := countRequests(t, pool); total != 0 {
		t.Fatalf("a refused create wrote %d rows", total)
	}
}

/* ── the model-connection request (Settings › AI Configuration) ─────────── */

// The vocabulary the AI Configuration panel's "Request a model connection"
// dialog files, spelled out here rather than imported from the client or the
// handler: these tests assert what travels on the WIRE, and a shared constant
// would move with whichever side changed it instead of failing.
//
// The dialog adds no route and no column. It reuses the catalogue's create
// call unchanged — same method, same path, same body — and differs only in
// these two values. That is the property worth pinning: "a model connection"
// is not a second mechanism, it is a second `issue_type` in the one queue an
// operator already reads.
const (
	modelConnectionIssueType = "Model Connection Request"
	providerConnectionEntity = "provider:anthropic"
	modelConnectionEntity    = "model:claude-opus-4"
)

// configurationsFixture is the state the clerical pin measures against: two
// rows of the project's OWN AI configuration, one of which has passed its
// connection check. `status_ok` is the column a "this connection works" badge
// reads, and it is the one a provisioning side effect would most plausibly
// touch without adding a row.
const configurationsFixture = `
INSERT INTO p_1.configuration (project_id, elitea_title, label, type, section, data, status_ok) VALUES
    (1, 'OpenAI GPT-4o',     'openai', 'openai', 'llm',       '{"name":"gpt-4o"}',           true),
    (1, 'OpenAI Embeddings', 'openai', 'openai', 'embedding', '{"name":"text-embedding-3"}', false);
`

func prepareConfigurations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), configurationsFixture); err != nil {
		t.Fatalf("seed configurations: %v", err)
	}
}

// configurationSnapshot renders every configuration row of project 1 as
// comparable text. `type` and `status_ok` are both in it on purpose: a
// provisioning side effect would either ADD a row for the requested provider
// or FLIP an existing row's connection state, and a COUNT alone cannot see the
// second.
func configurationSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT elitea_title || '/' || type || '/' || section || '/status_ok=' || status_ok
FROM p_1.configuration ORDER BY id`)
	if err != nil {
		t.Fatalf("read configurations: %v", err)
	}
	defer rows.Close()

	snapshot := make([]string, 0)
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan configuration: %v", err)
		}
		snapshot = append(snapshot, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read configurations: %v", err)
	}
	return snapshot
}

func countNotifications(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.notifications`).Scan(&total); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return total
}

// fileModelConnectionRequest posts one through the product's own create
// handler and returns the row it answered with.
func fileModelConnectionRequest(
	t *testing.T, router chi.Router, entityID, description string,
) requestRow {
	t.Helper()
	recorder := moderationDo(t, router, http.MethodPost, entityURL(1, entityID), map[string]any{
		"issue_type":  modelConnectionIssueType,
		"description": description,
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201 (body %s)", entityID, recorder.Code, recorder.Body.String())
	}
	var row requestRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode create response %q: %v", recorder.Body.String(), err)
	}
	return row
}

// TestAModelConnectionRequestReachesTheQueueUnderItsOwnIssueType.
//
// The two surfaces that file into `centry.moderation_state` — the App
// Catalogue's "Request Access" card and Settings › AI Configuration's "Request
// a model connection" dialog — are one table, and `issue_type` is the ONLY
// thing that separates an operator's two queues. If the filter did not
// discriminate, the model-connection queue would be the app-access queue with
// extra rows in it, and neither operator could work from either.
//
// The `provider:`/`model:` entity_id shape is asserted end to end because it
// travels as a PATH segment: a colon is legal there, but the value that comes
// back is the only proof the route did not mangle it.
func TestAModelConnectionRequestReachesTheQueueUnderItsOwnIssueType(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)

	provider := fileModelConnectionRequest(t, router, providerConnectionEntity,
		"We would like Anthropic models connected to this project.")
	model := fileModelConnectionRequest(t, router, modelConnectionEntity,
		"claude-opus-4 specifically, for the review pipeline.")
	if provider.EntityID != providerConnectionEntity || model.EntityID != modelConnectionEntity {
		t.Fatalf("entity_id came back as %q/%q, want %q/%q — the colon-prefixed address must survive the path",
			provider.EntityID, model.EntityID, providerConnectionEntity, modelConnectionEntity)
	}

	// A catalogue request on the same wire, so the filter below has something
	// to exclude rather than an empty table to agree with.
	if recorder := moderationDo(t, router, http.MethodPost, entityURL(1, "inventory"), map[string]any{
		"issue_type": "Inventory", "description": "Asset tracking for the hardware lab.",
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("POST the catalogue request = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}

	filtered := readQueue(t, router, "issue_type="+url.QueryEscape(modelConnectionIssueType))
	if filtered.Total != 2 || len(filtered.Rows) != 2 {
		t.Fatalf("issue_type=%q returned %d rows / total %d, want 2/2",
			modelConnectionIssueType, len(filtered.Rows), filtered.Total)
	}
	for _, row := range filtered.Rows {
		if row.IssueType != modelConnectionIssueType {
			t.Errorf("a row of issue_type %q survived the filter", row.IssueType)
		}
		if !strings.HasPrefix(row.EntityID, "provider:") && !strings.HasPrefix(row.EntityID, "model:") {
			t.Errorf("entity_id = %q, want a provider:/model: address", row.EntityID)
		}
	}

	// The filter narrows the ANSWER; it does not describe the table. Without
	// this the assertion above would also pass on a queue that lost the
	// catalogue row.
	if all := readQueue(t, router, ""); all.Total != 3 {
		t.Fatalf("unfiltered queue total = %d, want 3", all.Total)
	}
	if catalogue := readQueue(t, router, "issue_type=Inventory"); catalogue.Total != 1 {
		t.Fatalf("the catalogue's own queue total = %d, want 1", catalogue.Total)
	}

	// And through the per-entity read the REQUESTING client uses, whose
	// `issue_type` query parameter is the same discriminator applied to one
	// address at a time.
	recorder := moderationDo(t, router, http.MethodGet,
		entityURL(1, providerConnectionEntity)+"?issue_type="+url.QueryEscape(modelConnectionIssueType), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("per-entity read = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing requestListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode per-entity body %q: %v", recorder.Body.String(), err)
	}
	if listing.Total != 1 || listing.Rows[0].EntityID != providerConnectionEntity {
		t.Fatalf("per-entity read = %+v, want the one provider request", listing.Rows)
	}
	if listing.Rows[0].Status != statusPendingLiteral {
		t.Errorf("a fresh model-connection request is %q, want pending", listing.Rows[0].Status)
	}
}

// TestApprovingAModelConnectionRequestNotifiesTheRequester.
//
// Approval grants NOTHING here (see TestApprovingAModelConnectionProvisionsNothing),
// so the notification is the entire outcome the requester ever sees. A
// decision they are never told about is, to them, indistinguishable from no
// decision — and the 200 the PUT answers with says nothing about the row in
// `centry.notifications`, so it is read back server-side.
func TestApprovingAModelConnectionRequestNotifiesTheRequester(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	handler := moderation.NewHandler(pool)

	filed := fileModelConnectionRequest(t, moderationRouter(handler, ada), providerConnectionEntity,
		"We would like Anthropic models connected to this project.")
	if notifications := countNotifications(t, pool); notifications != 0 {
		t.Fatalf("filing a request notified %d times; only a decision notifies", notifications)
	}

	moderator := moderationRouter(handler, grace)
	if recorder := moderationDo(t, moderator, http.MethodPut, decisionURL,
		map[string]any{"id": filed.ID, "status": "approved"}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if status, _, _ := requestSQL(t, pool, filed.ID); status != "approved" {
		t.Fatalf("row status = %q after approving, want approved", status)
	}

	var eventType, meta string
	var notifiedUser, notifiedProject int64
	var isSeen bool
	if err := pool.QueryRow(context.Background(), `
SELECT event_type, user_id, project_id, is_seen, meta::text
FROM centry.notifications ORDER BY id DESC LIMIT 1`).
		Scan(&eventType, &notifiedUser, &notifiedProject, &isSeen, &meta); err != nil {
		t.Fatalf("no notification was written for the model-connection decision: %v", err)
	}
	if eventType != "moderation_approved" {
		t.Errorf("event_type = %q, want moderation_approved", eventType)
	}
	// Ada filed it, Grace decided it. The notification is delivered to the
	// AUTHOR, never to the person who answered.
	if notifiedUser != 4001 {
		t.Errorf("notification went to user %d, want the requester 4001", notifiedUser)
	}
	if notifiedProject != 1 {
		t.Errorf("notification project_id = %d, want 1", notifiedProject)
	}
	if isSeen {
		t.Error("the notification was written already seen")
	}
	// Neither frontend's notification renderer has a branch for this event
	// type, so what the requester reads has to travel on the row — including
	// WHICH request was answered.
	for _, want := range []string{modelConnectionIssueType, providerConnectionEntity, "has been approved"} {
		if !strings.Contains(meta, want) {
			t.Errorf("notification meta = %s, want it to carry %q", meta, want)
		}
	}
	if notifications := countNotifications(t, pool); notifications != 1 {
		t.Errorf("the decision wrote %d notifications, want exactly 1", notifications)
	}
}

// TestAModelConnectionDecisionCannotRetypeTheRequest.
//
// The generic refusal is already pinned by
// TestDecisionRefusesToRewriteWhatWasRequested; this is the same boundary read
// from the new issue_type's side, because that is where retyping would be
// TEMPTING rather than obviously wrong: `issue_type` is now the queue
// selector, so an operator who could rewrite it could move a request out of
// their queue and into someone else's — or approve an app-access request as a
// model connection — and the row would carry no trace of either.
func TestAModelConnectionDecisionCannotRetypeTheRequest(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	handler := moderation.NewHandler(pool)

	filed := fileModelConnectionRequest(t, moderationRouter(handler, ada), providerConnectionEntity,
		"We would like Anthropic models connected to this project.")
	moderator := moderationRouter(handler, grace)

	for _, body := range []map[string]any{
		{"id": filed.ID, "status": "approved", "issue_type": "Application Access Request"},
		{"id": filed.ID, "status": "approved", "entity_id": "provider:openai"},
		{"id": filed.ID, "status": "rejected", "rejection_comment": "wrong queue", "issue_type": "Wikis"},
	} {
		recorder := moderationDo(t, moderator, http.MethodPut, decisionURL, body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("decision carrying %v = %d, want 400", body, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "not editable by the person answering it") {
			t.Errorf("the refusal read %q, want the immutable-record reason", recorder.Body.String())
		}
	}

	// Refused, and refused WHOLE: not the status, not the fields the bodies
	// named, and no notification telling the requester they were answered.
	if status, _, _ := requestSQL(t, pool, filed.ID); status != statusPendingLiteral {
		t.Fatalf("a refused decision moved the row to %q", status)
	}
	row := readQueue(t, moderator, "").Rows[0]
	if row.IssueType != modelConnectionIssueType || row.EntityID != providerConnectionEntity {
		t.Fatalf("a refused decision rewrote the request: %+v", row)
	}
	if notifications := countNotifications(t, pool); notifications != 0 {
		t.Fatalf("a refused decision notified the requester %d times", notifications)
	}
}

// TestApprovingAModelConnectionProvisionsNothing — the CLERICAL PIN.
//
// Approval here is a clerical act: it moves `status` and it notifies the
// author. It connects no provider, creates no configuration, and enables no
// model. Somebody reading "Model Connection Request → approved" will
// reasonably assume the opposite, which is exactly why the absence is asserted
// rather than left to the reader: an approval hook added later would make this
// test fail, and that is the point at which the product would owe the operator
// an explanation of what approving now does.
//
// A COUNT of new rows is not enough on its own. `status_ok` is the column a
// "connection verified" badge reads, and flipping it needs no new row — so the
// snapshot covers both shapes of side effect.
func TestApprovingAModelConnectionProvisionsNothing(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	prepareConfigurations(t, pool)
	handler := moderation.NewHandler(pool)

	before := configurationSnapshot(t, pool)
	if len(before) != 2 {
		t.Fatalf("the fixture seeded %d configuration rows, want 2 — the pin below would assert nothing", len(before))
	}

	filed := fileModelConnectionRequest(t, moderationRouter(handler, ada), providerConnectionEntity,
		"We would like Anthropic models connected to this project.")
	moderator := moderationRouter(handler, grace)
	if recorder := moderationDo(t, moderator, http.MethodPut, decisionURL,
		map[string]any{"id": filed.ID, "status": "approved"}); recorder.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	// The approval really happened — without this the absences below would
	// also hold for a decision that silently did nothing at all.
	if status, _, _ := requestSQL(t, pool, filed.ID); status != "approved" {
		t.Fatalf("row status = %q after approving, want approved", status)
	}

	var provisioned int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM p_1.configuration
WHERE type ILIKE '%anthropic%' OR label ILIKE '%anthropic%'
   OR elitea_title ILIKE '%anthropic%' OR data::text ILIKE '%claude%'`).Scan(&provisioned); err != nil {
		t.Fatalf("look for a provisioned configuration: %v", err)
	}
	if provisioned != 0 {
		t.Fatalf("approving a model-connection request created %d configuration row(s) for the requested entity",
			provisioned)
	}

	after := configurationSnapshot(t, pool)
	if len(after) != len(before) {
		t.Fatalf("the approval changed the configuration count from %d to %d", len(before), len(after))
	}
	for index := range before {
		if before[index] != after[index] {
			t.Fatalf("configuration row %d changed across the approval: %q -> %q",
				index, before[index], after[index])
		}
	}

	// Stated separately from the snapshot comparison so a failure names the
	// column. `p_1` is the only tenant schema a fresh 001_initial.sql creates,
	// so this IS "anywhere" on this database.
	var verified int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM p_1.configuration WHERE status_ok`).Scan(&verified); err != nil {
		t.Fatalf("count verified configurations: %v", err)
	}
	if verified != 1 {
		t.Fatalf("%d configurations report status_ok after the approval, want the 1 the fixture seeded", verified)
	}

	// What the approval IS allowed to have done, so this test cannot pass by
	// the decision having failed outright.
	if notifications := countNotifications(t, pool); notifications != 1 {
		t.Fatalf("the approval wrote %d notifications, want exactly 1", notifications)
	}
	if total := countRequests(t, pool); total != 1 {
		t.Fatalf("the approval left %d moderation rows, want the 1 that was filed", total)
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// gatedModerationRouter mounts the two administration routes WITH the
// route-level middleware, exactly as internal/api/router.go composes them.
func gatedModerationRouter(
	handler *moderation.Handler, resolver auth.PermissionResolver, principal auth.User,
) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "admin.moderation",
	)).Get(queueURL, handler.AdministrationRequests)
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "admin.moderation.edit",
	)).Put(decisionURL, handler.AdministrationRequestUpdate)
	return router
}

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context, principal auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

func grantingResolver(permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
	}
}

// TestDecisionIsRefusedWithoutTheEditPermission. `window.admin_ui_config.permissions`
// hands every session the same hardcoded array, so hiding the approve button
// changes nothing about what a crafted request can do.
func TestDecisionIsRefusedWithoutTheEditPermission(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	handler := moderation.NewHandler(pool)
	principal := auth.User{ID: "4002", UserID: "4002"}
	pending := rowByDescription(t,
		readQueue(t, moderationRouter(handler, principal), "status=pending"), "onboarding docs")

	gated := gatedModerationRouter(handler, grantingResolver("admin.moderation"), principal)
	recorder := moderationDo(t, gated, http.MethodPut, decisionURL,
		map[string]any{"id": pending.ID, "status": "approved"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("decision without admin.moderation.edit = %d, want 403", recorder.Code)
	}
	if status, _, _ := requestSQL(t, pool, pending.ID); status != "pending" {
		t.Fatalf("the refused decision approved the request anyway (%q)", status)
	}

	allowed := gatedModerationRouter(handler,
		grantingResolver("admin.moderation", "admin.moderation.edit"), principal)
	if recorder := moderationDo(t, allowed, http.MethodPut, decisionURL,
		map[string]any{"id": pending.ID, "status": "approved"}); recorder.Code != http.StatusOK {
		t.Fatalf("decision WITH the permission = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if status, _, _ := requestSQL(t, pool, pending.ID); status != "approved" {
		t.Fatal("the permitted decision did not apply")
	}
}

// TestQueueIsRefusedWithoutTheViewPermission. The queue lists every tenant's
// requests, each naming a user, a project and what they asked for, so the
// listing itself is the sensitive part — and it was mounted UNGATED before this
// unit.
func TestQueueIsRefusedWithoutTheViewPermission(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	handler := moderation.NewHandler(pool)
	principal := auth.User{ID: "4002", UserID: "4002"}

	gated := gatedModerationRouter(handler, grantingResolver("admin.moderation.edit"), principal)
	if recorder := moderationDo(t, gated, http.MethodGet, queueURL, nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("queue read without admin.moderation = %d, want 403", recorder.Code)
	}

	allowed := gatedModerationRouter(handler, grantingResolver("admin.moderation"), principal)
	if recorder := moderationDo(t, allowed, http.MethodGet, queueURL, nil); recorder.Code != http.StatusOK {
		t.Fatalf("queue read WITH admin.moderation = %d, want 200", recorder.Code)
	}
}

/* ── harness bootstrap ─────────────────────────────────────────────────── */

// newModerationPool creates an isolated database and applies the REAL bootstrap
// migration — the same 001_initial.sql a fresh deployment gets — so the
// `centry.moderation_state` DDL these tests read through is the shipped one
// rather than a second copy that could drift from it. That matters especially
// here: this unit ADDED that table to the migration, and a test that created its
// own copy would pass whether or not deployments get one.
func newModerationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_moderation_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	return pool
}
