package adminaccess_test

// The elevation ledger on the real schema (ADR-0059 decision 5; story R2.1a,
// issue 2515): 001_initial.sql and the shared corpus, so the trigger and the
// constraints under test are the ones shared 0269 creates.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// The trigger refuses an UPDATE of every column fixed at insert, of the
// decision and the start once written, DELETE and TRUNCATE. It lets the end
// and the review be written, each once, the review forward only.
func TestElevationLedgerIsAppendOnlyExceptTheEndAndTheReview(t *testing.T) {
	pool := newElevationDatabase(t)
	ctx := t.Context()
	elevation := startElevation(t, pool, seededStart(t, pool))

	for column, assignment := range map[string]string{
		"reason":              `reason = 'something else'`,
		"reason_code":         `reason_code = 'other'`,
		"level":               `level = 'super_admin'`,
		"browser_session_ref": `browser_session_ref = browser_session_ref + 1`,
		"expires_at":          `expires_at = expires_at + interval '1 minute'`,
		"started_at":          `started_at = started_at - interval '1 minute'`,
		"user_id":             `user_id = 99`,
		"path":                `path = 'sealed', approver_user_id = NULL`,
		"ticket":              `ticket = 'INC-2'`,
		"requester_user_id":   `requester_user_id = 98`,
		"approver_user_id":    `approver_user_id = 97`,
		"decision":            `decision = 'denied', level = NULL`,
		"requested_at":        `requested_at = requested_at - interval '1 minute'`,
		"id":                  `id = gen_random_uuid()`,
	} {
		_, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET `+assignment+` WHERE id = $1`, elevation.ID)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("UPDATE of %s was not refused by the trigger: %v", column, err)
		}
	}
	for name, statement := range map[string]string{
		"DELETE":   `DELETE FROM elitea_auth.admin_elevations WHERE id = $1`,
		"TRUNCATE": `TRUNCATE elitea_auth.admin_elevations`,
	} {
		var err error
		if strings.Contains(statement, "$1") {
			_, err = pool.Exec(ctx, statement, elevation.ID)
		} else {
			_, err = pool.Exec(ctx, statement)
		}
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s was not refused by the trigger: %v", name, err)
		}
	}
	if after := readElevation(t, pool, elevation.ID); normalized(after) != normalized(elevation) {
		t.Fatalf("a refused UPDATE changed the row:\nbefore %+v\nafter  %+v", elevation, after)
	}

	// The end and the review state are writable.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations
SET ended_at = started_at + interval '5 minutes', end_reason = 'holder', review_state = 'review_pending'
WHERE id = $1`, elevation.ID); err != nil {
		t.Fatalf("UPDATE of the end time, the end reason and the review state was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET review_state = 'reviewed' WHERE id = $1`,
		elevation.ID); err != nil {
		t.Fatalf("UPDATE of the review state to reviewed was refused: %v", err)
	}
	// ... each once, and the review forward only.
	for name, assignment := range map[string]string{
		"a second end time":         `ended_at = ended_at + interval '1 minute'`,
		"a second end reason":       `end_reason = 'operator'`,
		"the review state backward": `review_state = 'review_pending'`,
		"the review state cleared":  `review_state = NULL`,
		"another final review":      `review_state = 'unreviewed_no_reviewer'`,
	} {
		if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET `+assignment+` WHERE id = $1`,
			elevation.ID); err == nil {
			t.Errorf("UPDATE of %s was accepted", name)
		}
	}
	ended := readElevation(t, pool, elevation.ID)
	if ended.EndReason != adminaccess.ElevationEndHolder || ended.ReviewState != adminaccess.ElevationReviewed ||
		!ended.EndedAt.Equal(elevation.StartedAt.Add(5*time.Minute)) {
		t.Fatalf("the end and the review are not as written: %+v", ended)
	}

	// A column a later migration adds is frozen unless that migration
	// replaces the trigger function on purpose.
	if _, err := pool.Exec(ctx, `ALTER TABLE elitea_auth.admin_elevations ADD COLUMN later_column text`); err != nil {
		t.Fatalf("add a column: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET later_column = 'x' WHERE id = $1`,
		elevation.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE of a column added later was not refused: %v", err)
	}
}

// A request row that has not been decided can be decided once; after that its
// level and approver are frozen, and a denied request can never start.
func TestElevationDecisionIsWrittenOnce(t *testing.T) {
	pool := newElevationDatabase(t)
	ctx := t.Context()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, path, reason_code, reason, requester_user_id, requested_at)
VALUES (7, 41, 'approved', 'incident', 'the provider is down', 7, now())
RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert a request: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'denied', decided_at = requested_at + interval '1 minute', approver_user_id = 8
WHERE id = $1`, id); err != nil {
		t.Fatalf("deny the request: %v", err)
	}
	for name, assignment := range map[string]string{
		"approve after a denial": `decision = 'approved', level = 'admin'`,
		"another denier":         `approver_user_id = 9`,
		"start a denied request": `started_at = decided_at, expires_at = decided_at + interval '1 hour'`,
	} {
		if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET `+assignment+` WHERE id = $1`, id); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if state := readElevation(t, pool, id).State(time.Now()); state != adminaccess.ElevationStateDenied {
		t.Fatalf("state %q, want denied", state)
	}
}

// The constraints hold the rules of decision 5 below the Go checks.
func TestElevationLedgerConstraints(t *testing.T) {
	pool := newElevationDatabase(t)
	const columns = `(user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id,
        approver_user_id, requested_at, decision, decided_at, started_at, expires_at, review_state)`
	// The control: the same statement with valid values is accepted, so each
	// refusal below is the rule it names.
	for _, values := range []string{
		`(7, 1041, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '480 minutes', NULL)`,
		`(7, 1042, 'super_admin', 'sealed', 'identity_outage', 'r', 7, NULL, now(), 'approved', now(), now(), now() + interval '240 minutes', NULL)`,
		`(7, 1043, 'admin', 'self', 'recovery', 'r', 7, NULL, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
	} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO elitea_auth.admin_elevations `+columns+` VALUES `+values); err != nil {
			t.Fatalf("the control row %s was refused: %v", values, err)
		}
	}
	for name, values := range map[string]string{
		"no session reference":     `(7, 0, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"an unknown level":         `(7, 41, 'owner', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"an unknown path":          `(7, 41, 'admin', 'manual', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"an unknown reason code":   `(7, 41, 'admin', 'approved', 'curiosity', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"a blank reason":           `(7, 41, 'admin', 'approved', 'incident', ' ', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"the own approval":         `(7, 41, 'admin', 'approved', 'incident', 'r', 7, 7, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"approved, no approver":    `(7, 41, 'admin', 'approved', 'incident', 'r', 7, NULL, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"self with an approver":    `(7, 41, 'admin', 'self', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"self at super_admin":      `(7, 41, 'super_admin', 'self', 'incident', 'r', 7, NULL, now(), 'approved', now(), now(), now() + interval '1 hour', NULL)`,
		"started undecided":        `(7, 41, NULL, 'approved', 'incident', 'r', 7, NULL, now(), NULL, NULL, now(), now() + interval '1 hour', NULL)`,
		"a level with no approval": `(7, 41, 'admin', 'approved', 'incident', 'r', 7, NULL, now(), NULL, NULL, NULL, NULL, NULL)`,
		"no expiry":                `(7, 41, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), NULL, NULL)`,
		"above the online ceiling": `(7, 41, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '481 minutes', NULL)`,
		"above the sealed ceiling": `(7, 41, 'admin', 'sealed', 'incident', 'r', 7, NULL, now(), 'approved', now(), now(), now() + interval '241 minutes', NULL)`,
		"inserted reviewed":        `(7, 41, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour', 'reviewed')`,
	} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO elitea_auth.admin_elevations `+columns+` VALUES `+values); err == nil {
			t.Errorf("the ledger accepted %s", name)
		}
	}
}

// Start writes one row bound to the given session reference; a second start
// on a session with an active elevation is refused, another session is not.
func TestStartElevationWritesOneRowBoundToTheSession(t *testing.T) {
	pool := newElevationDatabase(t)
	start := seededStart(t, pool)
	start.Ticket = " INC-1 "
	start.RequestedAt = elevationClock.Add(-20 * time.Minute)
	start.DecidedAt = elevationClock.Add(-10 * time.Minute)
	elevation := startElevation(t, pool, start)

	var rows int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM elitea_auth.admin_elevations WHERE browser_session_ref = $1`,
		start.BrowserSessionRef).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for the session, want 1", rows)
	}
	want := adminaccess.ElevationRecord{
		ID:                elevation.ID,
		UserID:            start.UserID,
		BrowserSessionRef: start.BrowserSessionRef,
		Level:             adminaccess.ElevationLevelAdmin,
		Path:              adminaccess.ElevationPathApproved,
		ReasonCode:        adminaccess.ElevationReasonIncident,
		Reason:            start.Reason,
		Ticket:            "INC-1",
		RequesterUserID:   start.UserID,
		ApproverUserID:    start.ApproverUserID,
		RequestedAt:       start.RequestedAt,
		Decision:          adminaccess.ElevationDecisionApproved,
		DecidedAt:         start.DecidedAt,
		StartedAt:         elevationClock,
		ExpiresAt:         elevationClock.Add(time.Hour),
	}
	if got := normalized(readElevation(t, pool, elevation.ID)); got != want {
		t.Fatalf("the row:\n got %+v\nwant %+v", got, want)
	}
	if normalized(elevation) != want {
		t.Fatalf("StartElevation returned %+v, want %+v", elevation, want)
	}
	if state := elevation.State(elevationClock.Add(time.Minute)); state != adminaccess.ElevationStateActive {
		t.Fatalf("state %q, want active", state)
	}

	// The same session again, while the first is active: no extension.
	again := start
	again.RequestedAt, again.DecidedAt = time.Time{}, time.Time{}
	again.At = elevationClock.Add(30 * time.Minute)
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.StartElevation(t.Context(), tx, auditAppender(t), again)
		return err
	}); !errors.Is(err, adminaccess.ErrSessionAlreadyElevated) {
		t.Fatalf("a second start on the elevated session: %v, want ErrSessionAlreadyElevated", err)
	}
	// Another session of the same person is a separate elevation.
	other := again
	other.BrowserSessionRef = seedBrowserSession(t, pool, start.UserID)
	startElevation(t, pool, other)
	// After the expiry, the first session can start again.
	later := again
	later.At = elevationClock.Add(time.Hour)
	startElevation(t, pool, later)

	// The trigger holds the same rule for a writer that is not the ledger.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id,
     approver_user_id, requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, 'admin', 'approved', 'incident', 'r', $1, $3, $4::timestamptz, 'approved', $4::timestamptz, $4::timestamptz, $4::timestamptz + interval '1 minute')`,
		start.UserID, start.BrowserSessionRef, start.ApproverUserID, later.At.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "already holds an open elevation") {
		t.Fatalf("a second open elevation written by hand was not refused by the trigger: %v", err)
	}
}

// Start binds an elevation only to a live session of the person, refuses a
// suspended person, and gives the super_admin level only when the approver
// holds it through a standing role.
func TestStartElevationChecksThePersonTheSessionAndTheApprover(t *testing.T) {
	pool := newElevationDatabase(t)
	ctx := t.Context()
	base := seededStart(t, pool)
	stranger := seedPerson(t, pool)
	strangerSession := seedBrowserSession(t, pool, stranger)

	revoked := seedBrowserSession(t, pool, base.UserID)
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.browser_sessions SET revoked_at = $2 WHERE ref = $1`,
		revoked, elevationClock.Add(-time.Minute)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	expired := seedBrowserSession(t, pool, base.UserID)
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.browser_sessions SET expires_at = $2 WHERE ref = $1`,
		expired, elevationClock); err != nil {
		t.Fatalf("expire: %v", err)
	}
	suspended := seedPerson(t, pool)
	suspendedSession := seedBrowserSession(t, pool, suspended)
	if _, err := pool.Exec(ctx, `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	superAdmin := seedPerson(t, pool, adminaccess.RoleSuperAdmin)
	plainAdmin := seedPerson(t, pool, adminaccess.RoleAdmin)

	for name, test := range map[string]struct {
		change func(*adminaccess.ElevationStart)
		want   error
	}{
		"another person's session": {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = strangerSession }, adminaccess.ErrElevationSession},
		"no such session":          {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = 987654321 }, adminaccess.ErrElevationSession},
		"a revoked session":        {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = revoked }, adminaccess.ErrElevationSession},
		"an expired session":       {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = expired }, adminaccess.ErrElevationSession},
		"a suspended person": {func(s *adminaccess.ElevationStart) {
			s.UserID, s.BrowserSessionRef = suspended, suspendedSession
		}, adminaccess.ErrElevationPerson},
		"super_admin from an admin approver": {func(s *adminaccess.ElevationStart) {
			s.Level, s.ApproverUserID = adminaccess.ElevationLevelSuperAdmin, plainAdmin
		}, adminaccess.ErrElevationApproverLevel},
		"super_admin from an approver with no role": {func(s *adminaccess.ElevationStart) {
			s.Level, s.ApproverUserID = adminaccess.ElevationLevelSuperAdmin, stranger
		}, adminaccess.ErrElevationApproverLevel},
	} {
		t.Run(name, func(t *testing.T) {
			start := base
			test.change(&start)
			err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				_, err := adminaccess.StartElevation(t.Context(), tx, auditAppender(t), start)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("StartElevation: %v, want %v", err, test.want)
			}
		})
	}

	// The controls: a super_admin approver gives the super_admin level, and a
	// Form session of the person is a session too.
	approved := base
	approved.Level, approved.ApproverUserID = adminaccess.ElevationLevelSuperAdmin, superAdmin
	if got := startElevation(t, pool, approved); got.Level != adminaccess.ElevationLevelSuperAdmin {
		t.Fatalf("the super_admin-approved elevation has level %q", got.Level)
	}
	form := base
	form.BrowserSessionRef = seedFormSession(t, pool, base.UserID)
	startElevation(t, pool, form)
}

// The reason `expired` before the expiry is refused: the ledger would record
// a clean timeout for an elevation that something else ended.
func TestEndElevationRefusesExpiredBeforeTheExpiry(t *testing.T) {
	pool := newElevationDatabase(t)
	elevation := startElevation(t, pool, seededStart(t, pool))
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, _, err := adminaccess.EndElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationEnd{
			ID: elevation.ID, Reason: adminaccess.ElevationEndExpired, At: elevationClock.Add(10 * time.Minute),
		})
		return err
	}); !errors.Is(err, adminaccess.ErrElevationNotExpired) {
		t.Fatalf("an `expired` end before the expiry: %v, want ErrElevationNotExpired", err)
	}
	if after := readElevation(t, pool, elevation.ID); !after.EndedAt.IsZero() {
		t.Fatalf("the refused end wrote %+v", after)
	}
	ended, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndExpired, At: elevation.ExpiresAt,
	})
	if !changed || ended.EndReason != adminaccess.ElevationEndExpired || !ended.EndedAt.Equal(elevation.ExpiresAt) {
		t.Fatalf("an `expired` end at the expiry: changed %v, %+v", changed, ended)
	}
}

// End sets the end time and the end reason once and moves the elevation to
// review; a second end changes nothing.
func TestEndElevationEndsOnce(t *testing.T) {
	pool := newElevationDatabase(t)
	elevation := startElevation(t, pool, seededStart(t, pool))

	first, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: elevationClock.Add(10 * time.Minute),
	})
	if !changed {
		t.Fatal("the first end wrote nothing")
	}
	if !first.EndedAt.Equal(elevationClock.Add(10*time.Minute)) || first.EndReason != adminaccess.ElevationEndHolder ||
		first.ReviewState != adminaccess.ElevationReviewPending {
		t.Fatalf("the first end: %+v", first)
	}
	if first.ActiveAt(elevationClock.Add(11 * time.Minute)) {
		t.Fatal("the ended elevation is still active")
	}

	second, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndApprover, At: elevationClock.Add(20 * time.Minute),
		ActorUserID: elevation.ApproverUserID,
	})
	if changed {
		t.Fatal("the second end reported a change")
	}
	if after := readElevation(t, pool, elevation.ID); normalized(after) != normalized(first) || normalized(second) != normalized(first) {
		t.Fatalf("the second end changed the row:\nfirst  %+v\nafter  %+v\nreturn %+v", first, after, second)
	}

	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, _, err := adminaccess.EndElevation(t.Context(), tx, auditAppender(t), adminaccess.ElevationEnd{
			ID: uuid.New(), Reason: adminaccess.ElevationEndHolder, At: elevationClock,
		})
		return err
	}); !errors.Is(err, adminaccess.ErrElevationNotFound) {
		t.Fatalf("ending an unknown elevation: %v, want ErrElevationNotFound", err)
	}
}

// An end that comes after the expiry records the expiry: access had already
// stopped then, with no job running.
func TestEndElevationAfterTheExpiryRecordsTheExpiry(t *testing.T) {
	pool := newElevationDatabase(t)
	elevation := startElevation(t, pool, seededStart(t, pool))
	if elevation.ActiveAt(elevation.ExpiresAt) {
		t.Fatal("the elevation is active at its expiry")
	}
	ended, changed := endElevation(t, pool, adminaccess.ElevationEnd{
		ID: elevation.ID, Reason: adminaccess.ElevationEndLogout, At: elevation.ExpiresAt.Add(time.Hour),
	})
	if !changed || !ended.EndedAt.Equal(elevation.ExpiresAt) || ended.EndReason != adminaccess.ElevationEndExpired {
		t.Fatalf("an end after the expiry: changed %v, %+v", changed, ended)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// seedPerson creates an account holding the given administration roles.
func seedPerson(t *testing.T, pool *pgxpool.Pool, roles ...string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Elevation test') RETURNING id`,
		"elevation-"+uuid.NewString()+"@autotest.local").Scan(&id); err != nil {
		t.Fatalf("seed a person: %v", err)
	}
	for _, role := range roles {
		tag, err := pool.Exec(t.Context(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, id, role)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("give %s: %v (%d rows)", role, err, tag.RowsAffected())
		}
	}
	return id
}

// seedBrowserSession creates a live browser session of the person, valid for
// a day from the test clock, and returns its reference.
func seedBrowserSession(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, created_at, last_seen_at, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', $3::timestamptz, $3::timestamptz, $3::timestamptz + interval '1 day', 0)
RETURNING ref`, sessionID(), userID, elevationClock).Scan(&ref); err != nil {
		t.Fatalf("seed a browser session: %v", err)
	}
	return ref
}

// seedFormSession creates a live Form session of the person and returns its
// reference (from the same sequence as browser_sessions.ref, shared 0266).
func seedFormSession(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.form_sessions (id_hash, record, expires_at)
VALUES (encode(sha256(convert_to($1, 'UTF8')), 'hex'),
        convert_to(json_build_object('schema_version', 1, 'done', true, 'user_id', $2::bigint)::text, 'UTF8'),
        $3::timestamptz + interval '1 day')
RETURNING ref`, sessionID(), userID, elevationClock).Scan(&ref); err != nil {
		t.Fatalf("seed a Form session: %v", err)
	}
	return ref
}

func sessionID() string {
	return strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
}

// seededStart is a valid approved admin-level start of a real person, in a
// live session of theirs, approved by another real person.
func seededStart(t *testing.T, pool *pgxpool.Pool) adminaccess.ElevationStart {
	t.Helper()
	start := validElevationStart()
	start.UserID = seedPerson(t, pool)
	start.ApproverUserID = seedPerson(t, pool, adminaccess.RoleAdmin)
	start.BrowserSessionRef = seedBrowserSession(t, pool, start.UserID)
	return start
}

func auditAppender(t *testing.T) securityaudit.Appender {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/elevation-test")
	if err != nil {
		t.Fatalf("appender: %v", err)
	}
	return appender
}

func startElevation(t *testing.T, pool *pgxpool.Pool, start adminaccess.ElevationStart) adminaccess.ElevationRecord {
	t.Helper()
	var elevation adminaccess.ElevationRecord
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		elevation, err = adminaccess.StartElevation(t.Context(), tx, auditAppender(t), start)
		return err
	}); err != nil {
		t.Fatalf("start an elevation: %v", err)
	}
	return elevation
}

func endElevation(t *testing.T, pool *pgxpool.Pool, end adminaccess.ElevationEnd) (adminaccess.ElevationRecord, bool) {
	t.Helper()
	var elevation adminaccess.ElevationRecord
	var changed bool
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		elevation, changed, err = adminaccess.EndElevation(t.Context(), tx, auditAppender(t), end)
		return err
	}); err != nil {
		t.Fatalf("end an elevation: %v", err)
	}
	return elevation, changed
}

func readElevation(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) adminaccess.ElevationRecord {
	t.Helper()
	elevation, err := adminaccess.ReadElevation(t.Context(), pool, id)
	if err != nil {
		t.Fatalf("read the elevation: %v", err)
	}
	return elevation
}

// normalized puts every time in UTC, so rows compare with ==.
func normalized(e adminaccess.ElevationRecord) adminaccess.ElevationRecord {
	for _, field := range []*time.Time{&e.RequestedAt, &e.DecidedAt, &e.StartedAt, &e.ExpiresAt, &e.EndedAt} {
		if !field.IsZero() {
			*field = field.UTC()
		}
	}
	return e
}

// newElevationDatabase is an isolated database with 001_initial.sql and the
// shared corpus applied.
func newElevationDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_elevations_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s: the DROP queues behind the CREATE DATABASE calls of every
		// package `go test ./...` runs at the same time (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile("../infra/db/migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus: %v", err)
	}
	return pool
}
