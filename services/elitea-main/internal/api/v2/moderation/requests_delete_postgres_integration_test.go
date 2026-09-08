package moderation_test

// The withdraw route and the queue's paging, both from issue #544.
//
// ## What the issue measured
//
// An app request could be created and decided, and never removed. 20 runs of
// the end-to-end suite on one stack left 111 rows in `centry.moderation_state`,
// and runs 19 and 20 then failed: the queue's default order is `created_at`
// ASCENDING, the read asked for `?limit=100&offset=0`, and the newest row — the
// one the run had just filed — was the first to fall off the end. The failure
// said "the request must be present in the queue" and meant "it is not on page
// one".
//
// Two properties answer that, and each is asserted here against a real
// PostgreSQL rather than read from the source:
//
//  1. a caller can REMOVE their own request, so a stack that runs the suite
//     does not grow one row per run for ever (TestWithdraw*);
//  2. the queue pages, and a row past the first page is still reachable — by
//     offset and by name (TestQueuePagesPastTheFirstHundredRows).
//
// The harness (newModerationPool, moderationRouter, moderationDo, readQueue,
// the two personas) lives in requests_postgres_integration_test.go, in this
// same package.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/moderation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── helpers ───────────────────────────────────────────────────────────── */

// deleteListing is the withdraw answer: the rows that were removed, plus the
// number of decision notifications that went with them.
type deleteListing struct {
	Rows                 []requestRow `json:"rows"`
	Total                int          `json:"total"`
	NotificationsDeleted int          `json:"notifications_deleted"`
}

// gatedDeleteRouter mounts the withdraw route WITH the route-level gate
// internal/api/router.go puts on it, so the permission is asserted where a
// crafted request would meet it.
func gatedDeleteRouter(
	handler *moderation.Handler, resolver auth.PermissionResolver, principal auth.User,
) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(apimw.RequireResolvedPermissions(
		resolver, auth.PermissionModeDefault, "admin.moderation.create",
	)).Delete("/admin/moderation_status/{mode}/{projectID}/{entityID}", handler.RequestDelete)
	return router
}

// grantingResolverForUser answers with the permissions AND the caller's real
// id.
//
// The id matters here and nowhere else in this package: the gate REWRITES the
// principal's user id with the one the resolver returns
// (internal/api/middleware/rbac.go), so the row scope this handler applies is
// the RESOLVED identity. A resolver that answered a different id would leave
// the withdraw looking for rows nobody filed, and the test would read as a
// broken delete rather than as a harness that renamed the caller.
func grantingResolverForUser(userID int64, permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: userID, Permissions: permissions}, nil
	}
}

// countEntityRows reads the table directly. A re-read through the GET proves
// the product agrees with itself; this proves the ROW is gone, so a read that
// filtered the row out could not pass both.
func countEntityRows(t *testing.T, pool *pgxpool.Pool, entityID string) int {
	t.Helper()
	var total int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.moderation_state WHERE entity_id = $1`, entityID).
		Scan(&total); err != nil {
		t.Fatalf("count rows for %s: %v", entityID, err)
	}
	return total
}

func decodeDeleteListing(t *testing.T, body []byte) deleteListing {
	t.Helper()
	var listing deleteListing
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("decode delete body %q: %v", string(body), err)
	}
	return listing
}

/* ── the withdraw round trip ───────────────────────────────────────────── */

// TestWithdrawRemovesTheRequestAndItsDecisionNotification is the round trip the
// issue asks for: file, decide, withdraw, and nothing of either is left.
//
// The notification half is not decoration. `centry.notifications` is read by
// the requester with a limit, so leaving one dead notice per run behind repeats
// this issue on a second table — the newest notice is pushed off the first page
// by a hundred older ones, and the reader sees a decision they were never told
// about.
func TestWithdrawRemovesTheRequestAndItsDecisionNotification(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	router := moderationRouter(moderation.NewHandler(pool), ada)
	const entity = "e2e_app_request_probe_withdraw"

	created := moderationDo(t, router, http.MethodPost, entityURL(1, entity), map[string]any{
		"issue_type":  "Wikis",
		"description": "We need the wiki toolkit for onboarding docs.",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201 (body %s)", created.Code, created.Body.String())
	}
	var filed requestRow
	if err := json.Unmarshal(created.Body.Bytes(), &filed); err != nil {
		t.Fatalf("decode created row: %v", err)
	}

	// Decided first, so the withdraw runs against the state a real row ends in.
	// A delete that only worked on `pending` rows would leave every decided
	// probe behind, which is the accumulation this issue measured.
	decided := moderationDo(t, router, http.MethodPut, decisionURL,
		map[string]any{"id": filed.ID, "status": "approved"})
	if decided.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %s)", decided.Code, decided.Body.String())
	}
	if notifications := countNotifications(t, pool); notifications != 1 {
		t.Fatalf("the approval wrote %d notifications, want exactly 1", notifications)
	}

	recorder := moderationDo(t, router, http.MethodDelete, entityURL(1, entity), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	listing := decodeDeleteListing(t, recorder.Body.Bytes())
	if listing.Total != 1 || len(listing.Rows) != 1 {
		t.Fatalf("withdraw returned %d rows / total %d, want 1/1", len(listing.Rows), listing.Total)
	}
	if listing.Rows[0].ID != filed.ID {
		t.Errorf("withdraw returned row %d, want the one filed (%d)", listing.Rows[0].ID, filed.ID)
	}
	if listing.NotificationsDeleted != 1 {
		t.Errorf("notifications_deleted = %d, want 1 — the decision notice names a row that is gone",
			listing.NotificationsDeleted)
	}

	// The ROW is gone, by SQL…
	if rows := countEntityRows(t, pool, entity); rows != 0 {
		t.Fatalf("%d rows survive the withdraw, want 0", rows)
	}
	if notifications := countNotifications(t, pool); notifications != 0 {
		t.Fatalf("%d decision notifications survive the withdraw, want 0", notifications)
	}
	// …and through the two reads a client makes: the operator's queue and the
	// requester's own per-entity list.
	if queue := readQueue(t, router, "entity_id="+entity); queue.Total != 0 {
		t.Fatalf("the queue still lists %d rows for %s", queue.Total, entity)
	}
	own := moderationDo(t, router, http.MethodGet, entityURL(1, entity), nil)
	var ownListing requestListing
	if err := json.Unmarshal(own.Body.Bytes(), &ownListing); err != nil {
		t.Fatalf("decode own listing: %v", err)
	}
	if ownListing.Total != 0 {
		t.Fatalf("the requester still reads %d of their own rows for %s", ownListing.Total, entity)
	}

	// A second withdraw is a 404, not a 200 that removed nothing. A teardown
	// that reads success from an empty delete reports a clean stack while the
	// table keeps growing — which is exactly how this issue stayed invisible.
	again := moderationDo(t, router, http.MethodDelete, entityURL(1, entity), nil)
	if again.Code != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want 404 (body %s)", again.Code, again.Body.String())
	}
}

// TestWithdrawCannotReachAnotherPersonsRequest. The row is the record of what an
// operator was asked and what they answered. A caller may erase their own, and
// nobody else's — and naming the `administration` mode on the path must not
// widen that, exactly as it does not widen the read beside it.
func TestWithdrawCannotReachAnotherPersonsRequest(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	handler := moderation.NewHandler(pool)

	// The fixture gives ada a `wikis_Wikis` row in project 1.
	if rows := countEntityRows(t, pool, "wikis_Wikis"); rows != 2 {
		t.Fatalf("fixture has %d wikis_Wikis rows, want 2", rows)
	}

	graceRouter := moderationRouter(handler, grace)
	for _, mode := range []string{"default", "administration"} {
		target := fmt.Sprintf("/admin/moderation_status/%s/1/wikis_Wikis", mode)
		recorder := moderationDo(t, graceRouter, http.MethodDelete, target, nil)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("grace deleting ada's row on mode %q = %d, want 404 (body %s)",
				mode, recorder.Code, recorder.Body.String())
		}
	}
	if rows := countEntityRows(t, pool, "wikis_Wikis"); rows != 2 {
		t.Fatalf("a refused withdraw removed a row anyway (%d left, want 2)", rows)
	}

	// Ada removes HER row, and grace's row in project 2 stays. The scope is
	// three columns, and this proves all three are applied rather than one.
	adaRouter := moderationRouter(handler, ada)
	recorder := moderationDo(t, adaRouter, http.MethodDelete,
		"/admin/moderation_status/default/1/wikis_Wikis", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ada withdrawing her own row = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if rows := countEntityRows(t, pool, "wikis_Wikis"); rows != 1 {
		t.Fatalf("%d wikis_Wikis rows left, want 1 — grace's project-2 row must survive", rows)
	}
	var owner int64
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id FROM centry.moderation_state WHERE entity_id = 'wikis_Wikis'`).
		Scan(&owner); err != nil {
		t.Fatalf("read the surviving row: %v", err)
	}
	if owner != 4002 {
		t.Fatalf("the surviving row belongs to user %d, want grace (4002)", owner)
	}
}

// TestWithdrawIsRefusedWithoutTheCreatePermission. The route takes the same
// permission as the POST beside it — a person who may file a request may take
// it back — so a caller who may not file one may not delete one either.
func TestWithdrawIsRefusedWithoutTheCreatePermission(t *testing.T) {
	pool := newModerationPool(t)
	prepareFixture(t, pool)
	handler := moderation.NewHandler(pool)
	principal := auth.User{ID: "4001", UserID: "4001", Email: "ada@example.com"}
	const target = "/admin/moderation_status/default/1/wikis_Wikis"

	refused := gatedDeleteRouter(handler, grantingResolverForUser(4001, "admin.moderation.view"), principal)
	if recorder := moderationDo(t, refused, http.MethodDelete, target, nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("withdraw without admin.moderation.create = %d, want 403", recorder.Code)
	}
	if rows := countEntityRows(t, pool, "wikis_Wikis"); rows != 2 {
		t.Fatalf("the refused withdraw removed a row anyway (%d left, want 2)", rows)
	}

	allowed := gatedDeleteRouter(handler,
		grantingResolverForUser(4001, "admin.moderation.view", "admin.moderation.create"), principal)
	if recorder := moderationDo(t, allowed, http.MethodDelete, target, nil); recorder.Code != http.StatusOK {
		t.Fatalf("withdraw WITH the permission = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if rows := countEntityRows(t, pool, "wikis_Wikis"); rows != 1 {
		t.Fatalf("the permitted withdraw did not apply (%d rows left, want 1)", rows)
	}
}

/* ── the queue's paging ────────────────────────────────────────────────── */

// TestQueuePagesPastTheFirstHundredRows pins the read half of #544.
//
// The failure the issue measured is reproduced by construction: 104 rows, the
// wanted one filed LAST, and the queue's default order oldest-first. The
// assertions are the three ways a client can still reach it —
//
//   - `total` counts the whole set, not the page, so a client can tell there
//     are more pages;
//   - `limit` is capped at 100 and `offset` walks past that cap;
//   - `entity_id` names the row, so the read does not depend on the size of the
//     table at all. That is what the journey uses.
//
// A server that ignored `offset`, or that ignored `entity_id`, answers 200 with
// a plausible page in both cases. Only the identity of the rows says which.
func TestQueuePagesPastTheFirstHundredRows(t *testing.T) {
	pool := newModerationPool(t)
	prepareUsers(t, pool)
	const wanted = "e2e_app_request_probe_newest"
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.moderation_state (user_id, project_id, issue_type, entity_id, description, status, created_at)
SELECT 4001, 1, 'Wikis', 'older_' || n, 'filler ' || n, 'pending',
       TIMESTAMP '2026-08-01 09:00:00' + (n || ' minutes')::interval
FROM generate_series(1, 104) AS n;`); err != nil {
		t.Fatalf("seed 104 rows: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.moderation_state (user_id, project_id, issue_type, entity_id, description, status, created_at)
VALUES (4001, 1, 'Wikis', $1, 'the newest request', 'pending', TIMESTAMP '2026-08-02 09:00:00');`,
		wanted); err != nil {
		t.Fatalf("seed the newest row: %v", err)
	}
	router := moderationRouter(moderation.NewHandler(pool), ada)

	// A page is 100 rows at most, whatever is asked for, and `total` describes
	// the whole set — the two numbers a pager needs to know there is more.
	first := readQueue(t, router, "limit=1000&offset=0")
	if first.Total != 105 {
		t.Fatalf("total = %d, want 105", first.Total)
	}
	if len(first.Rows) != 100 {
		t.Fatalf("page one returned %d rows, want the 100-row cap", len(first.Rows))
	}
	for _, row := range first.Rows {
		if row.EntityID == wanted {
			t.Fatal("the newest row is on page one; the fixture no longer reproduces the failure")
		}
	}

	// The tail, by offset. This is the read a paging loop makes.
	second := readQueue(t, router, "limit=100&offset=100")
	if len(second.Rows) != 5 {
		t.Fatalf("page two returned %d rows, want 5", len(second.Rows))
	}
	found := false
	for _, row := range second.Rows {
		if row.EntityID == wanted {
			found = true
		}
	}
	if !found {
		t.Fatal("offset=100 did not reach the newest row: the server ignores offset")
	}

	// The read the journey makes: name the row. It is answered whatever the
	// size of the table, and it answers with THAT row and no other.
	named := readQueue(t, router, "entity_id="+wanted+"&sort_by=created_at&sort_order=desc&limit=100&offset=0")
	if named.Total != 1 || len(named.Rows) != 1 {
		t.Fatalf("the named read returned %d rows / total %d, want 1/1", len(named.Rows), named.Total)
	}
	if named.Rows[0].EntityID != wanted {
		t.Fatalf("the named read answered with %q, want %q", named.Rows[0].EntityID, wanted)
	}
}
