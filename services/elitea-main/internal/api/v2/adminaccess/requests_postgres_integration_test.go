package adminaccess_test

// The elevation request routes on PostgreSQL (ADR-0059 R2.2b, issue 2596):
// the handler over the real ledger (shared 0269, 0279), the real outbox
// (0277) and the real security audit outbox. The route gates are covered by
// requests_test.go; here they let every browser session through, so what is
// proven is what the ledger writes for a route call.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

var ledgerClock = time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

func newRequestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	pool := isolatedDatabase(t, databaseURL, "elitea_elevation_requests")
	// The accounts the corpus seeded are told nothing: only the people a
	// test seeds are recipients.
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// seedAccount is an account with the administration roles and a live
// browser session; it answers the account and the session's reference.
func seedAccount(t *testing.T, pool *pgxpool.Pool, roles ...string) (int64, int64) {
	t.Helper()
	ctx := t.Context()
	var id, ref int64
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Request test') RETURNING id`,
		"request-"+uuid.NewString()+"@autotest.local").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		tag, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, id, role)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("give %s: %v", role, err)
		}
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, created_at, last_seen_at, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', $3::timestamptz, $3::timestamptz, $3::timestamptz + interval '2 days', 0)
RETURNING ref`, strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), id, ledgerClock).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return id, ref
}

type ledgerHarness struct {
	pool     *pgxpool.Pool
	now      time.Time
	settings platformconfig.BreakGlassSettings
}

func (l *ledgerHarness) router(t *testing.T) http.Handler {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/elevation-request-test")
	if err != nil {
		t.Fatal(err)
	}
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	console.NewElevationsHandler(console.NewPostgresElevations(l.pool, appender),
		func(context.Context) (platformconfig.BreakGlassSettings, error) { return l.settings, nil },
		console.NewPostgresRoles(l.pool)).
		WithClock(func() time.Time { return l.now }).
		Register(r, console.ElevationGates{Request: pass, Approve: pass, History: pass})
	return r
}

func sessionOf(id, ref int64) auth.User {
	return auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession, BrowserSessionRef: ref}
}

type routeView struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Path     string `json:"path"`
	Level    string `json:"level"`
	Approver *struct {
		ID int64 `json:"id"`
	} `json:"approver"`
}

func decodeView(t *testing.T, body []byte) routeView {
	t.Helper()
	var view routeView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return view
}

func storedRow(t *testing.T, pool *pgxpool.Pool, id string) (path, decision, level string, approver int64) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `
SELECT path, COALESCE(decision, ''), COALESCE(level, ''), COALESCE(approver_user_id, 0)
FROM elitea_auth.admin_elevations WHERE id = $1::uuid`, id).Scan(&path, &decision, &level, &approver); err != nil {
		t.Fatal(err)
	}
	return path, decision, level, approver
}

func routeEvents(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event::text FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event -> 'data' -> 'elevation' ->> 'elevation_id' = $2 ORDER BY seq`,
		string(securityaudit.TypeAdminElevation), id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}

// A request through the route writes one request notice row for each
// standing holder of access.breakglass.approve other than the requester, in
// the transaction of its row and its event; an approval by a holder of
// `admin` records the level admin, and one by a holder of `super_admin` the
// level super_admin.
func TestTheRequestRoutesOnPostgreSQL(t *testing.T) {
	pool := newRequestDatabase(t)
	l := &ledgerHarness{pool: pool, now: ledgerClock.Add(time.Minute), settings: platformconfig.DefaultBreakGlassSettings()}
	h := l.router(t)
	admin, adminRef := seedAccount(t, pool, registry.RoleAdmin)
	superAdmin, superRef := seedAccount(t, pool, registry.RoleSuperAdmin)
	seedAccount(t, pool, registry.RoleSecurityAuditor)

	for approverID, want := range map[int64]struct {
		ref   int64
		level string
	}{admin: {adminRef, registry.ElevationLevelAdmin}, superAdmin: {superRef, registry.ElevationLevelSuperAdmin}} {
		person, ref := seedAccount(t, pool, registry.RoleEmergencyAccess)
		rec := call(t, h, sessionOf(person, ref), http.MethodPost, "/access/elevations", validRequest)
		if rec.Code != http.StatusCreated {
			t.Fatalf("request: %d %s", rec.Code, rec.Body)
		}
		requested := decodeView(t, rec.Body.Bytes())
		var recipients []int64
		rows, err := pool.Query(t.Context(), `
SELECT recipient_user_id FROM elitea_auth.admin_access_notices
WHERE elevation_id = $1::uuid AND kind = 'request' ORDER BY recipient_user_id`, requested.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			recipients = append(recipients, id)
		}
		rows.Close()
		wantRecipients := []int64{admin, superAdmin}
		slices.Sort(wantRecipients)
		if !slices.Equal(recipients, wantRecipients) {
			t.Fatalf("request notices to %v, want %v (every approver but the requester)", recipients, wantRecipients)
		}
		if events := routeEvents(t, pool, requested.ID); len(events) != 1 || !strings.Contains(events[0], `"requested"`) {
			t.Fatalf("request events = %v", events)
		}

		rec = call(t, h, sessionOf(approverID, want.ref), http.MethodPost, approvePath(uuid.MustParse(requested.ID)), ``)
		if rec.Code != http.StatusOK {
			t.Fatalf("approve by %d: %d %s", approverID, rec.Code, rec.Body)
		}
		if view := decodeView(t, rec.Body.Bytes()); view.Level != want.level || view.State != registry.ElevationStateApproved ||
			view.Approver == nil || view.Approver.ID != approverID {
			t.Fatalf("approved by %d = %s, want level %s", approverID, rec.Body, want.level)
		}
		if _, decision, level, approver := storedRow(t, pool, requested.ID); decision != registry.ElevationDecisionApproved ||
			level != want.level || approver != approverID {
			t.Fatalf("stored decision %s level %s approver %d", decision, level, approver)
		}
	}
}

// With approval switched off and its delay not over, a request still needs
// an approver; after the delay it is approved by the system at the level
// admin on the path self, and its event says that no approver was involved.
func TestApprovalOffTakesEffectAfterItsDelay(t *testing.T) {
	pool := newRequestDatabase(t)
	settings := platformconfig.DefaultBreakGlassSettings()
	settings.ApprovalRequested = false
	settings.ApprovalOffEffectiveFrom = ledgerClock.Add(settings.ApprovalOffDelay)
	l := &ledgerHarness{pool: pool, now: ledgerClock.Add(time.Hour), settings: settings}
	h := l.router(t)
	approver, _ := seedAccount(t, pool, registry.RoleAdmin)

	before, beforeRef := seedAccount(t, pool, registry.RoleEmergencyAccess)
	rec := call(t, h, sessionOf(before, beforeRef), http.MethodPost, "/access/elevations", validRequest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request during the delay: %d %s", rec.Code, rec.Body)
	}
	during := decodeView(t, rec.Body.Bytes())
	if path, decision, _, _ := storedRow(t, pool, during.ID); path != registry.ElevationPathApproved || decision != "" ||
		during.State != registry.ElevationStateRequested {
		t.Fatalf("during the delay: path %s decision %q state %s, want an undecided approved-path request", path, decision, during.State)
	}
	var notices int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_auth.admin_access_notices WHERE elevation_id = $1::uuid`,
		during.ID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("during the delay %d request notices (%v), want 1 (the approver %d)", notices, err, approver)
	}

	l.now = settings.ApprovalOffEffectiveFrom.Add(time.Minute)
	after, afterRef := seedAccount(t, pool, registry.RoleEmergencyAccess)
	rec = call(t, l.router(t), sessionOf(after, afterRef), http.MethodPost, "/access/elevations", validRequest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request after the delay: %d %s", rec.Code, rec.Body)
	}
	self := decodeView(t, rec.Body.Bytes())
	path, decision, level, approverID := storedRow(t, pool, self.ID)
	if path != registry.ElevationPathSelf || decision != registry.ElevationDecisionApproved ||
		level != registry.ElevationLevelAdmin || approverID != 0 || self.Approver != nil {
		t.Fatalf("after the delay: path %s decision %s level %s approver %d", path, decision, level, approverID)
	}
	events := routeEvents(t, pool, self.ID)
	if len(events) != 2 || !strings.Contains(events[1], `"approved"`) ||
		!strings.Contains(events[1], registry.ApproverNone) || !strings.Contains(events[1], `"source:system"`) {
		t.Fatalf("approval-off events = %v", events)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_auth.admin_access_notices WHERE elevation_id = $1::uuid`,
		self.ID).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("approval off wrote %d request notices (%v)", notices, err)
	}
}

// An approval after the request lifetime answers 410, records the expiry,
// and the requester's own request then reads as expired. The own request
// cannot be approved by its requester, and a person under an elevation
// cannot approve at all.
func TestALateOrOwnOrElevatedApprovalOnPostgreSQL(t *testing.T) {
	pool := newRequestDatabase(t)
	settings := platformconfig.DefaultBreakGlassSettings()
	l := &ledgerHarness{pool: pool, now: ledgerClock.Add(time.Minute), settings: settings}
	approver, approverRef := seedAccount(t, pool, registry.RoleAdmin)
	// A requester who is a standing approver too: only the own rule refuses.
	person, ref := seedAccount(t, pool, registry.RoleAdmin)
	rec := call(t, l.router(t), sessionOf(person, ref), http.MethodPost, "/access/elevations", validRequest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	id := uuid.MustParse(decodeView(t, rec.Body.Bytes()).ID)

	if rec := call(t, l.router(t), sessionOf(person, ref), http.MethodPost, approvePath(id), ``); rec.Code != http.StatusForbidden ||
		refusalCode(t, rec) != console.CodeElevationOwnRequest {
		t.Fatalf("own approval: %d %s", rec.Code, rec.Body)
	}

	// An approver under an active elevation.
	elevated, elevatedRef := seedAccount(t, pool, registry.RoleAdmin)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id, approver_user_id,
     requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, 'admin', 'approved', 'incident', 'r', $1, $3, $4, 'approved', $4, $4, $4::timestamptz + interval '1 hour')`,
		elevated, elevatedRef, approver, ledgerClock); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, l.router(t), sessionOf(elevated, elevatedRef), http.MethodPost, approvePath(id), ``); rec.Code != http.StatusForbidden ||
		refusalCode(t, rec) != console.CodeElevationApproverElevated {
		t.Fatalf("elevated approval: %d %s", rec.Code, rec.Body)
	}

	l.now = ledgerClock.Add(time.Minute + settings.RequestLifetime + time.Second)
	rec = call(t, l.router(t), sessionOf(approver, approverRef), http.MethodPost, approvePath(id), ``)
	if rec.Code != http.StatusGone || refusalCode(t, rec) != console.CodeElevationRequestExpired {
		t.Fatalf("late approval: %d %s, want 410", rec.Code, rec.Body)
	}
	if _, decision, _, _ := storedRow(t, pool, id.String()); decision != registry.ElevationDecisionRequestExpired {
		t.Fatalf("the expiry was not recorded: %q", decision)
	}
	rec = call(t, l.router(t), sessionOf(person, ref), http.MethodGet, "/access/elevations/mine", ``)
	if !strings.Contains(rec.Body.String(), `"state":"request_expired"`) {
		t.Fatalf("the own request after a late approval: %s", rec.Body)
	}
}
