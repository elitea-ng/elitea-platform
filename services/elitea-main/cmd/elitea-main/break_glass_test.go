package main

// `elitea-main break-glass end` (ADR-0059 R2.13, issue 2527): the argument
// rules before any connection, and, against the full corpus (skipped without
// ELITEA_TEST_DATABASE_URL), `end --all` and an unknown id.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type breakGlassRun struct {
	code           int
	stdout, stderr string
}

func runBreakGlassAgainst(t *testing.T, dsn string, args ...string) breakGlassRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runBreakGlassWith(context.Background(), args,
		func(key string) (string, bool) { return dsn, key == "DATABASE_URL" }, &stdout, &stderr, pgxpool.New, time.Now)
	return breakGlassRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// startTestElevation starts an approved admin-level elevation of a new person
// in a new live browser session of theirs, from the wall clock, for an hour.
func startTestElevation(t *testing.T, pool *pgxpool.Pool) adminaccess.ElevationRecord {
	t.Helper()
	ctx := t.Context()
	person := func(role string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Break-glass test') RETURNING id`,
			"break-glass-"+uuid.NewString()+"@autotest.local").Scan(&id); err != nil {
			t.Fatalf("seed a person: %v", err)
		}
		if role != "" {
			if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, id, role); err != nil {
				t.Fatalf("give %s: %v", role, err)
			}
		}
		return id
	}
	holder, approver := person(""), person(adminaccess.RoleAdmin)
	now := time.Now().UTC()
	var ref int64
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, created_at, last_seen_at, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', $3::timestamptz - interval '1 hour', $3::timestamptz, $3::timestamptz + interval '1 day', 0)
RETURNING ref`, strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), holder, now).Scan(&ref); err != nil {
		t.Fatalf("seed a browser session: %v", err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/break-glass-test")
	if err != nil {
		t.Fatal(err)
	}
	var elevation adminaccess.ElevationRecord
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		elevation, err = adminaccess.StartElevation(ctx, tx, appender, adminaccess.ElevationStart{
			UserID: holder, BrowserSessionRef: ref, Level: adminaccess.ElevationLevelAdmin,
			Path: adminaccess.ElevationPathApproved, ReasonCode: adminaccess.ElevationReasonIncident,
			Reason: "the console was taken", ApproverUserID: approver, At: now.Add(-time.Minute), Duration: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatalf("start an elevation: %v", err)
	}
	return elevation
}

// elevationOutboxActions answers the (action, source) pairs of an
// elevation's events, in order.
func elevationOutboxActions(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) [][2]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event -> 'data' -> 'elevation' ->> 'action', event -> 'data' -> 'elevation' ->> 'source'
FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event -> 'data' -> 'elevation' ->> 'elevation_id' = $2
ORDER BY seq`, string(securityaudit.TypeAdminElevation), id.String())
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return collectPairs(t, rows)
}

func collectPairs(t *testing.T, rows pgx.Rows) [][2]string {
	t.Helper()
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		err := row.Scan(&pair[0], &pair[1])
		return pair, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pairs
}

func countElevationEvents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeAdminElevation)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The argument rules are decided before any connection: a refused invocation
// never connects, and exits 2.
func TestBreakGlassRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no sub-command":      {nil, "usage:"},
		"unknown sub-command": {[]string{"revoke", uuid.NewString()}, `unknown sub-command "revoke"`},
		"end with nothing":    {[]string{"end"}, "needs an elevation ID or --all"},
		"end with both":       {[]string{"end", "--all", uuid.NewString()}, "either IDs or --all"},
		"end a bad id":        {[]string{"end", "42"}, `"42" is not an elevation id`},
		"an unknown flag":     {[]string{"end", "--force", uuid.NewString()}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			connected := false
			code := runBreakGlassWith(context.Background(), tc.args,
				func(string) (string, bool) { return "postgres://unused", true }, &stdout, &stderr,
				mustNotConnect(&connected), time.Now)
			if code != breakGlassExitUsage || connected {
				t.Fatalf("code = %d, connected = %v, stderr = %s", code, connected, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused invocation printed to stdout: %q", stdout.String())
			}
		})
	}
}

// `end --all` ends every active elevation, each with its own event.
func TestBreakGlassEndAllEndsEveryActiveElevation(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	first, second := startTestElevation(t, pool), startTestElevation(t, pool)

	run := runBreakGlassAgainst(t, dsn, "end", "--all")
	if run.code != breakGlassExitOK || !strings.Contains(run.stdout, first.ID.String()) ||
		!strings.Contains(run.stdout, second.ID.String()) {
		t.Fatalf("end --all: exit %d\nstdout: %s\nstderr: %s", run.code, run.stdout, run.stderr)
	}
	for _, elevation := range []adminaccess.ElevationRecord{first, second} {
		current, err := adminaccess.ReadElevation(t.Context(), pool, elevation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.EndReason != adminaccess.ElevationEndOperator || current.ReviewState != adminaccess.ElevationReviewPending {
			t.Errorf("elevation %s after end --all: %+v", elevation.ID, current)
		}
	}
	// Nothing is left to end.
	again := runBreakGlassAgainst(t, dsn, "end", "--all")
	if again.code != breakGlassExitOK || !strings.Contains(again.stdout, "no active elevation") {
		t.Fatalf("a second end --all: exit %d, %s %s", again.code, again.stdout, again.stderr)
	}
}

// `end <unknown id>` exits non-zero and writes nothing, even beside a valid
// id; so does an id that already ended.
func TestBreakGlassEndOfAnUnknownIDWritesNothing(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	elevation := startTestElevation(t, pool)
	before := countElevationEvents(t, pool)

	run := runBreakGlassAgainst(t, dsn, "end", elevation.ID.String(), uuid.NewString())
	if run.code != breakGlassExitRefused || !strings.Contains(run.stderr, "no elevation") ||
		!strings.Contains(run.stderr, "nothing was changed") {
		t.Fatalf("an unknown id: exit %d\nstdout: %s\nstderr: %s", run.code, run.stdout, run.stderr)
	}
	if current, err := adminaccess.ReadElevation(t.Context(), pool, elevation.ID); err != nil || !current.EndedAt.IsZero() {
		t.Fatalf("the valid id was ended beside an unknown one: %+v (%v)", current, err)
	}
	if after := countElevationEvents(t, pool); after != before {
		t.Fatalf("%d events written by a refused command", after-before)
	}

	if run := runBreakGlassAgainst(t, dsn, "end", elevation.ID.String()); run.code != breakGlassExitOK {
		t.Fatalf("end: exit %d, %s", run.code, run.stderr)
	}
	if run := runBreakGlassAgainst(t, dsn, "end", elevation.ID.String()); run.code != breakGlassExitRefused ||
		!strings.Contains(run.stderr, "already ended") {
		t.Fatalf("a second end: exit %d, %s", run.code, run.stderr)
	}
}
