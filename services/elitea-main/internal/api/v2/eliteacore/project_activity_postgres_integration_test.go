package eliteacore_test

// Unit A14 acceptance for `GET /elitea_core/project_user_activity/{mode}`
// (issue #200).
//
// The endpoint it replaces answered 200 with `{"rows":[],"total":0}`, the
// request discarded and the database untouched — so a status assertion, and
// even "the body has a `rows` key", both pass against the stub. Every case
// below seeds KNOWN events and asserts the counts, the grouping and the
// scoping against them.
//
// The two shapes worth naming:
//
//   - COUNTING ANOTHER PROJECT'S EVENTS. The drawer is per-project; a missing
//     `project_id` predicate produces plausible-looking numbers that are wrong.
//     TestProjectUserActivityCountsOnlyTheRequestedProject is the guard.
//   - ROW MULTIPLICATION BY EMAIL. `user_email` is denormalised onto every
//     audit row, so grouping by `(user_id, user_email)` — as the legacy runtime
//     does — splits one user across two rows, and the client's `Map` keyed on
//     `user_id` keeps only the last. TestProjectUserActivityMergesAUserWhose
//     EmailChanged is the guard.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise.
// No test prints a seeded row's contents beyond the ids and counts under
// assertion — audit records are sensitive even when synthetic.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

type projectActivityRow struct {
	UserID     int64   `json:"user_id"`
	UserEmail  *string `json:"user_email"`
	EventCount int64   `json:"event_count"`
}

type projectActivityBody struct {
	Rows []projectActivityRow `json:"rows"`
}

func projectActivityRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/elitea_core/project_user_activity/{mode}", handler.ProjectUserActivity)
	return router
}

func readProjectActivity(t *testing.T, router chi.Router, query string) projectActivityBody {
	t.Helper()
	recorder := auditGet(t, router, "/elitea_core/project_user_activity/administration?"+query)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET project_user_activity status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var body projectActivityBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func activityFor(body projectActivityBody, userID int64) (projectActivityRow, bool) {
	for _, row := range body.Rows {
		if row.UserID == userID {
			return row, true
		}
	}
	return projectActivityRow{}, false
}

// projectActivityFixtureSQL seeds three users across two projects.
//
//	user 701 — 3 events in project 8801, 1 in project 8802
//	user 702 — 1 event in project 8801, under TWO different email spellings
//	user 703 — 2 events in project 8801, all outside the narrow date window
//	user 704 — 2 events in project 8801, TYING user 702's count so the ordering
//	           has a tie to break
//	NULL     — 1 event in project 8801 with no user at all
const projectActivityFixtureSQL = `
INSERT INTO centry.audit_events (timestamp, user_id, user_email, project_id, event_type, action) VALUES
    ('2026-03-10T10:00:00', 701, 'seven-oh-one@autotest.local', 8801, 'api', 'a'),
    ('2026-03-10T10:05:00', 701, 'seven-oh-one@autotest.local', 8801, 'api', 'b'),
    ('2026-03-10T10:10:00', 701, 'seven-oh-one@autotest.local', 8801, 'api', 'c'),
    ('2026-03-10T10:15:00', 701, 'seven-oh-one@autotest.local', 8802, 'api', 'other-project'),
    ('2026-03-10T10:20:00', 702, 'old-address@autotest.local',  8801, 'api', 'd'),
    ('2026-03-10T10:25:00', 702, 'new-address@autotest.local',  8801, 'api', 'e'),
    ('2026-03-01T09:00:00', 703, 'seven-oh-three@autotest.local', 8801, 'api', 'f'),
    ('2026-03-01T09:30:00', 703, 'seven-oh-three@autotest.local', 8801, 'api', 'g'),
    ('2026-03-10T10:35:00', 704, 'seven-oh-four@autotest.local', 8801, 'api', 'i'),
    ('2026-03-10T10:40:00', 704, 'seven-oh-four@autotest.local', 8801, 'api', 'j'),
    ('2026-03-10T10:30:00', NULL, NULL,                          8801, 'schedule', 'h');
`

func seedProjectActivity(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), projectActivityFixtureSQL); err != nil {
		t.Fatalf("seed project activity fixture: %v", err)
	}
}

func TestProjectUserActivityCountsOnlyTheRequestedProject(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	body := readProjectActivity(t, router, "project_id=8801")

	row, found := activityFor(body, 701)
	if !found {
		t.Fatalf("user 701 missing from the answer (%d rows)", len(body.Rows))
	}
	// 4 would mean project 8802's event was counted too.
	if row.EventCount != 3 {
		t.Errorf("user 701 event_count = %d, want 3", row.EventCount)
	}

	other := readProjectActivity(t, router, "project_id=8802")
	if len(other.Rows) != 1 {
		t.Fatalf("project 8802 returned %d rows, want 1", len(other.Rows))
	}
	if other.Rows[0].EventCount != 1 {
		t.Errorf("project 8802 event_count = %d, want 1", other.Rows[0].EventCount)
	}
}

func TestProjectUserActivityMergesAUserWhoseEmailChanged(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	body := readProjectActivity(t, router, "project_id=8801")

	appearances := 0
	for _, row := range body.Rows {
		if row.UserID == 702 {
			appearances++
		}
	}
	// The legacy GROUP BY (user_id, user_email) answers 2 here, each with count 1,
	// and the client's Map keeps whichever arrives last — a square showing "1
	// event" for a user who produced two.
	if appearances != 1 {
		t.Fatalf("user 702 appears %d times, want 1", appearances)
	}
	row, _ := activityFor(body, 702)
	if row.EventCount != 2 {
		t.Errorf("user 702 event_count = %d, want 2", row.EventCount)
	}
	// The address reported is the most recent one, not an arbitrary pick.
	if row.UserEmail == nil || *row.UserEmail != "new-address@autotest.local" {
		t.Errorf("user 702 user_email = %v, want the most recent address", row.UserEmail)
	}
}

func TestProjectUserActivityExcludesEventsWithNoUser(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	for _, row := range readProjectActivity(t, router, "project_id=8801").Rows {
		if row.UserID == 0 {
			t.Errorf("a row with no user id came back: %+v", row)
		}
	}
}

func TestProjectUserActivityHonoursTheDateWindow(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	body := readProjectActivity(t, router,
		"project_id=8801&date_from=2026-03-10T00:00:00Z&date_to=2026-03-10T23:59:59Z")

	if _, found := activityFor(body, 703); found {
		t.Error("user 703's events fall outside the window but were counted")
	}
	if row, found := activityFor(body, 701); !found || row.EventCount != 3 {
		t.Errorf("user 701 inside the window = %+v, want 3 events", row)
	}

	// The same query without bounds must see 703 — otherwise the window
	// assertion above would pass for the wrong reason.
	if _, found := activityFor(readProjectActivity(t, router, "project_id=8801"), 703); !found {
		t.Error("user 703 is absent even without a date window")
	}
}

func TestProjectUserActivityOrdersByCountWithAStableTiebreak(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	rows := readProjectActivity(t, router, "project_id=8801").Rows
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	// Users 702 and 704 both have 2 events, so `ORDER BY count DESC` alone
	// leaves their order to PostgreSQL — the mutation that removes the
	// `user_id` tiebreaker is invisible without a tie to break.
	tied := 0
	for _, row := range rows {
		if row.EventCount == 2 {
			tied++
		}
	}
	if tied < 2 {
		t.Fatalf("fixture no longer produces a tie: %v", activityShape(rows))
	}
	for index := 1; index < len(rows); index++ {
		previous, current := rows[index-1], rows[index]
		if current.EventCount > previous.EventCount {
			t.Fatalf("rows are not ordered by descending count: %v", activityShape(rows))
		}
		if current.EventCount == previous.EventCount && current.UserID < previous.UserID {
			t.Fatalf("tied rows are not ordered by user id: %v", activityShape(rows))
		}
	}
}

// activityShape renders only the ids and counts under assertion. Audit records
// are sensitive even when synthetic, so no failure message prints an address.
func activityShape(rows []projectActivityRow) []string {
	shape := make([]string, 0, len(rows))
	for _, row := range rows {
		shape = append(shape, fmt.Sprintf("user %d: %d", row.UserID, row.EventCount))
	}
	return shape
}

func TestProjectUserActivityRejectsAMissingOrBadProjectID(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	for _, query := range []string{"", "project_id=", "project_id=0", "project_id=-4", "project_id=abc"} {
		recorder := auditGet(t, router, "/elitea_core/project_user_activity/administration?"+query)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d, want 400", query, recorder.Code)
		}
	}
}

func TestProjectUserActivityReturnsAnEmptyArrayForAnUnknownProject(t *testing.T) {
	pool := newAuditPool(t)
	seedProjectActivity(t, pool)
	router := projectActivityRouter(eliteacore.NewHandler(pool))

	body := readProjectActivity(t, router, "project_id=999999")
	if body.Rows == nil {
		t.Fatal("rows is null, not an empty array — the client maps over it")
	}
	if len(body.Rows) != 0 {
		t.Errorf("unknown project returned %d rows", len(body.Rows))
	}
}
