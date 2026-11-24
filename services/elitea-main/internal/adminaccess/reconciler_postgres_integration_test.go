package adminaccess_test

// The elevation reconciler on the real schema (ADR-0059 decision 5, decision
// 8; story R2.16, issue 2609): the ledger of shared 0269 and its trigger, and
// the chained outbox of shared 0183.

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const reconcilerRequestLifetime = 30 * time.Minute

func newReconciler(t *testing.T, pool *pgxpool.Pool, appender securityaudit.Appender, now time.Time) *adminaccess.ElevationReconciler {
	t.Helper()
	reconciler, err := adminaccess.NewElevationReconciler(pool, appender,
		func(context.Context) (time.Duration, error) { return reconcilerRequestLifetime, nil },
		func() time.Time { return now })
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	return reconciler
}

func reconcile(t *testing.T, reconciler *adminaccess.ElevationReconciler) adminaccess.ReconcileResult {
	t.Helper()
	result, err := reconciler.Pass(t.Context())
	if err != nil {
		t.Fatalf("reconciler pass: %v", err)
	}
	return result
}

// seedLiveSession creates a browser session of the person live from an hour
// before at to a day after.
func seedLiveSession(t *testing.T, pool *pgxpool.Pool, userID int64, at time.Time) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, created_at, last_seen_at, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', $3::timestamptz - interval '2 hours', $3::timestamptz, $3::timestamptz + interval '1 day', 0)
RETURNING ref`, sessionID(), userID, at).Scan(&ref); err != nil {
		t.Fatalf("seed a browser session: %v", err)
	}
	return ref
}

// startAt starts an approved admin-level elevation of a new person at at.
func startAt(t *testing.T, pool *pgxpool.Pool, at time.Time, duration time.Duration) adminaccess.ElevationRecord {
	t.Helper()
	start := validElevationStart()
	start.UserID = seedPerson(t, pool)
	start.ApproverUserID = seedPerson(t, pool, adminaccess.RoleAdmin)
	start.BrowserSessionRef = seedLiveSession(t, pool, start.UserID, at)
	start.At, start.Duration = at, duration
	return startElevation(t, pool, start)
}

// insertRequest writes an undecided request, as the request route of R2.2b
// will.
func insertRequest(t *testing.T, pool *pgxpool.Pool, requestedAt time.Time) uuid.UUID {
	t.Helper()
	person := seedPerson(t, pool)
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, path, reason_code, reason, requester_user_id, requested_at)
VALUES ($1, $2, 'approved', 'incident', 'the provider is down', $1, $3)
RETURNING id`, person, seedLiveSession(t, pool, person, requestedAt), requestedAt).Scan(&id); err != nil {
		t.Fatalf("insert a request: %v", err)
	}
	return id
}

func sessionPrincipal(userID, ref int64) auth.User {
	id := strconv.FormatInt(userID, 10)
	return auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: ref}
}

func actions(events []elevationEventRow) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.action)
	}
	return out
}

// An elevation one second past its expiry already resolves nothing before
// the reconciler runs; one pass records it as expired, under review, with one
// `expired` event; a second pass writes nothing.
func TestReconcilerRecordsAnExpiredElevation(t *testing.T) {
	pool := newElevationDatabase(t)
	// The resolver reads the wall clock, so these rows are placed on it.
	now := time.Now().UTC().Truncate(time.Microsecond)
	lapsed := startAt(t, pool, now.Add(-time.Hour-time.Second), time.Hour)
	active := startAt(t, pool, now.Add(-time.Minute), time.Hour)

	resolver := legacyrbac.NewPostgresResolver(pool)
	resolve := func(e adminaccess.ElevationRecord) auth.PermissionResolution {
		t.Helper()
		resolution, err := resolver.ResolvePermissions(t.Context(), sessionPrincipal(e.UserID, e.BrowserSessionRef),
			auth.PermissionModeAdministration, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return resolution
	}
	// The control: an active elevation resolves its level's set.
	if resolution := resolve(active); len(resolution.ElevatedPermissions) == 0 {
		t.Fatalf("premise: the active elevation resolves nothing: %+v", resolution)
	}
	if resolution := resolve(lapsed); resolution.Elevated() {
		t.Fatalf("an elevation past its expiry resolves %v before the reconciler ran", resolution.ElevatedPermissions)
	}

	reconciler := newReconciler(t, pool, auditAppender(t), now)
	if result := reconcile(t, reconciler); result.Expired != 1 || result.RequestsExpired != 0 {
		t.Fatalf("first pass = %+v, want one expired elevation", result)
	}
	ended := readElevation(t, pool, lapsed.ID)
	if ended.EndReason != adminaccess.ElevationEndExpired || ended.ReviewState != adminaccess.ElevationReviewPending ||
		!ended.EndedAt.Equal(lapsed.ExpiresAt) {
		t.Fatalf("the lapsed elevation after a pass: %+v", ended)
	}
	if got := actions(elevationEvents(t, pool, lapsed.ID.String())); len(got) != 2 || got[1] != "expired" {
		t.Fatalf("events = %v, want started then one expired", got)
	}
	if current := readElevation(t, pool, active.ID); !current.EndedAt.IsZero() {
		t.Fatalf("the reconciler ended an active elevation: %+v", current)
	}

	if result := reconcile(t, reconciler); result != (adminaccess.ReconcileResult{}) {
		t.Fatalf("second pass = %+v, want nothing", result)
	}
	if got := elevationEvents(t, pool, lapsed.ID.String()); len(got) != 2 {
		t.Fatalf("a second pass wrote an event: %v", actions(got))
	}
}

// A request 31 minutes old is decided request_expired with one event; one 29
// minutes old is not; an approval afterwards is refused.
func TestReconcilerExpiresAnUnansweredRequest(t *testing.T) {
	pool := newElevationDatabase(t)
	old := insertRequest(t, pool, elevationClock.Add(-31*time.Minute))
	fresh := insertRequest(t, pool, elevationClock.Add(-29*time.Minute))

	if result := reconcile(t, newReconciler(t, pool, auditAppender(t), elevationClock)); result.RequestsExpired != 1 {
		t.Fatalf("pass = %+v, want one expired request", result)
	}
	request := readElevation(t, pool, old)
	if request.Decision != adminaccess.ElevationDecisionRequestExpired ||
		request.State(elevationClock) != adminaccess.ElevationStateRequestExpired ||
		!request.DecidedAt.Equal(request.RequestedAt.Add(reconcilerRequestLifetime)) {
		t.Fatalf("the old request: %+v", request)
	}
	if got := actions(elevationEvents(t, pool, old.String())); len(got) != 1 || got[0] != "request_expired" {
		t.Fatalf("events = %v, want one request_expired", got)
	}
	if current := readElevation(t, pool, fresh); current.Decision != "" {
		t.Fatalf("a request inside its lifetime was decided: %+v", current)
	}
	approver := seedPerson(t, pool, adminaccess.RoleAdmin)
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_auth.admin_elevations
SET decision = 'approved', decided_at = requested_at + interval '40 minutes', level = 'admin', approver_user_id = $2
WHERE id = $1`, old, approver); err == nil {
		t.Fatal("an approval of an expired request was accepted")
	}
	if again := reconcile(t, newReconciler(t, pool, auditAppender(t), elevationClock)); again != (adminaccess.ReconcileResult{}) {
		t.Fatalf("second pass = %+v", again)
	}
}

// Two reconcilers started together process each row once.
func TestReconcilersInParallelProcessEachRowOnce(t *testing.T) {
	pool := newElevationDatabase(t)
	const rows = 12
	var elevations, requests []uuid.UUID
	for range rows {
		elevations = append(elevations, startAt(t, pool, elevationClock.Add(-2*time.Hour), time.Hour).ID)
		requests = append(requests, insertRequest(t, pool, elevationClock.Add(-time.Hour)))
	}
	var wg sync.WaitGroup
	results := make([]adminaccess.ReconcileResult, 2)
	errs := make([]error, 2)
	gate := make(chan struct{})
	for i := range 2 {
		reconciler := newReconciler(t, pool, auditAppender(t), elevationClock)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			results[i], errs[i] = reconciler.Pass(context.Background())
		}()
	}
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reconciler %d: %v", i, err)
		}
	}
	if expired := results[0].Expired + results[1].Expired; expired != rows {
		t.Errorf("expired %d elevations in total, want %d (%+v)", expired, rows, results)
	}
	if expired := results[0].RequestsExpired + results[1].RequestsExpired; expired != rows {
		t.Errorf("expired %d requests in total, want %d (%+v)", expired, rows, results)
	}
	for _, id := range elevations {
		if got := actions(elevationEvents(t, pool, id.String())); len(got) != 2 || got[1] != "expired" {
			t.Errorf("elevation %s events = %v, want started and one expired", id, got)
		}
	}
	for _, id := range requests {
		if got := actions(elevationEvents(t, pool, id.String())); len(got) != 1 {
			t.Errorf("request %s events = %v, want one request_expired", id, got)
		}
	}
}

// With the appender failing nothing is reconciled; the next pass completes it.
func TestReconcilerRetriesARowWhoseEventFailed(t *testing.T) {
	pool := newElevationDatabase(t)
	elevation := startAt(t, pool, elevationClock.Add(-2*time.Hour), time.Hour)
	request := insertRequest(t, pool, elevationClock.Add(-time.Hour))

	result, err := newReconciler(t, pool, securityaudit.FailingAppender{}, elevationClock).Pass(t.Context())
	if err == nil || result != (adminaccess.ReconcileResult{}) {
		t.Fatalf("a pass with a failing appender = %+v, %v; want an error and nothing written", result, err)
	}
	if current := readElevation(t, pool, elevation.ID); !current.EndedAt.IsZero() || current.ReviewState != "" {
		t.Fatalf("the elevation was reconciled without its event: %+v", current)
	}
	if current := readElevation(t, pool, request); current.Decision != "" {
		t.Fatalf("the request was decided without its event: %+v", current)
	}
	if got := elevationEvents(t, pool, request.String()); len(got) != 0 {
		t.Fatalf("events after the failed pass: %v", actions(got))
	}

	if result := reconcile(t, newReconciler(t, pool, auditAppender(t), elevationClock)); result.Expired != 1 || result.RequestsExpired != 1 {
		t.Fatalf("the next pass = %+v, want both completed", result)
	}
	if current := readElevation(t, pool, elevation.ID); current.ReviewState != adminaccess.ElevationReviewPending {
		t.Fatalf("after the retry: %+v", current)
	}
}

// Every end reason arrives at review_pending, the early ends included.
func TestEveryEndReasonMovesToReview(t *testing.T) {
	pool := newElevationDatabase(t)
	for _, reason := range []string{
		adminaccess.ElevationEndHolder, adminaccess.ElevationEndApprover, adminaccess.ElevationEndOperator,
		adminaccess.ElevationEndExpired, adminaccess.ElevationEndLogout, adminaccess.ElevationEndSessionRevoked,
		adminaccess.ElevationEndUserSuspended, adminaccess.ElevationEndUserDeleted, adminaccess.ElevationEndSCIMDeactivated,
	} {
		elevation := startAt(t, pool, elevationClock, time.Hour)
		end := adminaccess.ElevationEnd{ID: elevation.ID, Reason: reason, At: elevationClock.Add(time.Minute)}
		switch reason {
		case adminaccess.ElevationEndApprover:
			end.ActorUserID = elevation.ApproverUserID
		case adminaccess.ElevationEndExpired:
			end.At = elevation.ExpiresAt
		}
		ended, changed := endElevation(t, pool, end)
		if !changed || ended.EndReason != reason || ended.ReviewState != adminaccess.ElevationReviewPending ||
			ended.State(end.At) != adminaccess.ElevationStateReviewPending {
			t.Errorf("end reason %s: changed %v, %+v", reason, changed, ended)
		}
	}
}

// The reconciler refuses to be built without its parts.
func TestNewElevationReconcilerNeedsEveryPart(t *testing.T) {
	t.Parallel()
	lifetime := func(context.Context) (time.Duration, error) { return time.Minute, nil }
	clock := func() time.Time { return elevationClock }
	var pool *pgxpool.Pool
	for name, build := range map[string]func() (*adminaccess.ElevationReconciler, error){
		"no database": func() (*adminaccess.ElevationReconciler, error) {
			return adminaccess.NewElevationReconciler(nil, securityaudit.FailingAppender{}, lifetime, clock)
		},
		"no appender": func() (*adminaccess.ElevationReconciler, error) {
			return adminaccess.NewElevationReconciler(pool, nil, lifetime, clock)
		},
		"no lifetime": func() (*adminaccess.ElevationReconciler, error) {
			return adminaccess.NewElevationReconciler(pool, securityaudit.FailingAppender{}, nil, clock)
		},
		"no clock": func() (*adminaccess.ElevationReconciler, error) {
			return adminaccess.NewElevationReconciler(pool, securityaudit.FailingAppender{}, lifetime, nil)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}
