package main

// `elitea-main break-glass` (ADR-0059 R2.13, issue 2527, and R2.6a, issue
// 2520): the argument rules before any connection, and, against the full
// corpus (skipped without ELITEA_TEST_DATABASE_URL), `end --all`, an unknown
// id, and the sealed credentials' issue, list and revoke.

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
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
		"unknown sub-command": {[]string{"rotate", uuid.NewString()}, `unknown sub-command "rotate"`},
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

/* ── the sealed credentials: issue, list, revoke (R2.6a, issue 2520) ───── */

// The sealed credential sub-commands refuse a bad invocation before any
// connection, with exit 2: an issue with no person, an unknown level, an
// expiry beyond 365 days or in the past, and a stray argument.
func TestBreakGlassSealedRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"issue with no user":     {[]string{"issue"}, "issue needs --user"},
		"issue an unknown level": {[]string{"issue", "--user", "7", "--level", "owner"}, `--level "owner"`},
		"issue for 366 days":     {[]string{"issue", "--user", "7", "--expires", "366d"}, "at most 365 days"},
		"issue for 9000h":        {[]string{"issue", "--user", "7", "--expires", "9000h"}, "at most 365 days"},
		"issue until 2099":       {[]string{"issue", "--user", "7", "--expires", "2099-01-01"}, "at most 365 days"},
		"issue until the past":   {[]string{"issue", "--user", "7", "--expires", "2001-01-01"}, "not in the future"},
		"issue a bad expiry":     {[]string{"issue", "--user", "7", "--expires", "soon"}, "not a lifetime"},
		"issue a stray argument": {[]string{"issue", "--user", "7", "extra"}, "takes no positional argument"},
		"issue a control label":  {[]string{"issue", "--user", "7", "--label", "a\nb"}, "control character"},
		"list an argument":       {[]string{"list", "x"}, "takes no positional argument"},
		"revoke nothing":         {[]string{"revoke"}, "revoke needs a sealed credential id"},
		"revoke a bad id":        {[]string{"revoke", "42"}, `"42" is not a sealed credential id`},
		"revoke --all":           {[]string{"revoke", "--all"}, "flag provided but not defined"},
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

func TestParseSealedExpiry(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                     adminaccess.DefaultSealedCredentialLifetime,
		"90d":                  90 * 24 * time.Hour,
		"365d":                 365 * 24 * time.Hour,
		"720h":                 720 * time.Hour,
		"2027-01-24":           61*24*time.Hour - 12*time.Hour,
		"2026-11-25T12:00:00Z": 24 * time.Hour,
	} {
		got, err := parseSealedExpiry(value, now)
		if err != nil || got != want {
			t.Errorf("parseSealedExpiry(%q) = %s, %v; want %s", value, got, err, want)
		}
	}
	for _, value := range []string{"366d", "0d", "-1h", "8761h", "2027-11-25", "99999999999d", "d"} {
		if _, err := parseSealedExpiry(value, now); err == nil {
			t.Errorf("parseSealedExpiry(%q) was accepted", value)
		}
	}
}

// seedBreakGlassPerson creates an active account and answers its id and
// e-mail address.
func seedBreakGlassPerson(t *testing.T, pool *pgxpool.Pool) (int64, string) {
	t.Helper()
	email := "sealed-" + uuid.NewString() + "@autotest.local"
	var id int64
	if err := pool.QueryRow(t.Context(), `INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Sealed test') RETURNING id`,
		email).Scan(&id); err != nil {
		t.Fatalf("seed a person: %v", err)
	}
	return id, email
}

var credentialLine = regexp.MustCompile(`(?m)^bg_([0-9a-f]{24})\.([0-9a-f]{64})$`)

// issuedCredential is what one `issue` printed and stored.
type issuedCredential struct {
	run        breakGlassRun
	credential string
	verifier   string
	stored     adminaccess.SealedCredential
}

func issueThroughCommand(t *testing.T, pool *pgxpool.Pool, dsn string, args ...string) issuedCredential {
	t.Helper()
	run := runBreakGlassAgainst(t, dsn, append([]string{"issue"}, args...)...)
	if run.code != breakGlassExitOK {
		t.Fatalf("issue %v: exit %d\nstdout: %s\nstderr: %s", args, run.code, run.stdout, run.stderr)
	}
	idMatch := regexp.MustCompile(`issued sealed credential ([0-9a-f-]{36})`).FindStringSubmatch(run.stdout)
	if idMatch == nil {
		t.Fatalf("issue printed no credential id:\n%s", run.stdout)
	}
	stored, err := adminaccess.ReadSealedCredential(t.Context(), pool, uuid.MustParse(idMatch[1]))
	if err != nil {
		t.Fatal(err)
	}
	out := issuedCredential{run: run, stored: stored}
	if m := credentialLine.FindStringSubmatch(run.stdout); m != nil {
		out.credential, out.verifier = m[0], m[2]
	}
	return out
}

// The command output is the only place the secret appears: not in any table
// of the database, not in the security audit outbox, not on stderr.
func TestBreakGlassIssuePrintsTheSecretOnlyOnStdout(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	person, _ := seedBreakGlassPerson(t, pool)
	issued := issueThroughCommand(t, pool, dsn, "--user", strconv.FormatInt(person, 10), "--label", "envelope A")
	if issued.credential == "" || strings.Count(issued.run.stdout, issued.verifier) != 1 {
		t.Fatalf("stdout does not show the credential exactly once:\n%s", issued.run.stdout)
	}
	if strings.Contains(issued.run.stderr, issued.verifier) {
		t.Fatalf("stderr holds the verifier: %s", issued.run.stderr)
	}
	if hits := grepAllTables(t, pool, issued.verifier); len(hits) > 0 {
		t.Fatalf("the verifier is stored in %v", hits)
	}
	if _, err := adminaccess.VerifySealedCredential(t.Context(), pool, issued.credential, time.Now()); err != nil {
		t.Fatalf("the printed credential does not verify: %v", err)
	}
	// The list shows the credential and never a secret.
	list := runBreakGlassAgainst(t, dsn, "list")
	if list.code != breakGlassExitOK || !strings.Contains(list.stdout, issued.stored.ID.String()+" active level=admin") ||
		!strings.Contains(list.stdout, `label="envelope A"`) || strings.Contains(list.stdout, issued.verifier) ||
		strings.Contains(list.stdout, issued.stored.Selector) {
		t.Fatalf("list: exit %d\n%s\n%s", list.code, list.stdout, list.stderr)
	}
}

// grepAllTables answers every table of a user schema whose rows, as JSON,
// hold needle.
func grepAllTables(t *testing.T, pool *pgxpool.Pool, needle string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT format('%I.%I', table_schema, table_name) FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 50 {
		t.Fatalf("premise: the corpus has %d tables", len(tables))
	}
	var hits []string
	for _, table := range tables {
		var found bool
		if err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM `+table+` AS t WHERE strpos(lower(to_jsonb(t)::text), lower($1)) > 0)`,
			needle).Scan(&found); err != nil {
			t.Fatalf("grep %s: %v", table, err)
		}
		if found {
			hits = append(hits, table)
		}
	}
	return hits
}

// `issue` without --level stores the level admin; with --level super_admin
// it stores that level, and the issue event carries it.
func TestBreakGlassIssueStoresTheLevel(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	_, email := seedBreakGlassPerson(t, pool)
	plain := issueThroughCommand(t, pool, dsn, "--user", email)
	high := issueThroughCommand(t, pool, dsn, "--user", strings.ToUpper(email), "--level", "super_admin")
	for _, tc := range []struct {
		issued issuedCredential
		level  string
	}{{plain, adminaccess.ElevationLevelAdmin}, {high, adminaccess.ElevationLevelSuperAdmin}} {
		if tc.issued.stored.Level != tc.level {
			t.Errorf("credential %s stored the level %q, want %q", tc.issued.stored.ID, tc.issued.stored.Level, tc.level)
		}
		var level, source string
		if err := pool.QueryRow(t.Context(), `
SELECT event -> 'data' -> 'elevation' ->> 'level', event -> 'data' -> 'elevation' ->> 'source'
FROM elitea_runtime.security_audit_outbox
WHERE event -> 'data' ->> 'action' = 'admin.elevation.credential_issued'
  AND event -> 'data' -> 'elevation' ->> 'credential_id' = $1`, tc.issued.stored.ID.String()).Scan(&level, &source); err != nil {
			t.Fatalf("the issue event of %s: %v", tc.issued.stored.ID, err)
		}
		if level != tc.level || source != "operator" {
			t.Errorf("the issue event carries level %q, source %q; want %q, operator", level, source, tc.level)
		}
	}
}

// `issue --split` prints two halves that concatenate to the verifier, and
// the row is marked split; the whole credential is not printed.
func TestBreakGlassIssueSplitPrintsTwoHalves(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	person, _ := seedBreakGlassPerson(t, pool)
	issued := issueThroughCommand(t, pool, dsn, "--user", strconv.FormatInt(person, 10), "--split")
	if issued.credential != "" {
		t.Fatalf("a split issue printed the whole credential:\n%s", issued.run.stdout)
	}
	halves := regexp.MustCompile(`(?m)^half ([12]): ([0-9a-f]{32})$`).FindAllStringSubmatch(issued.run.stdout, -1)
	selector := regexp.MustCompile(`(?m)^selector: bg_([0-9a-f]{24})$`).FindStringSubmatch(issued.run.stdout)
	if len(halves) != 2 || selector == nil || halves[0][1] != "1" || halves[1][1] != "2" {
		t.Fatalf("a split issue printed:\n%s", issued.run.stdout)
	}
	verifier := halves[0][2] + halves[1][2]
	if !issued.stored.Split || selector[1] != issued.stored.Selector {
		t.Fatalf("the row: %+v", issued.stored)
	}
	if _, err := adminaccess.VerifySealedCredential(t.Context(), pool,
		adminaccess.FormatSealedCredential(selector[1], verifier), time.Now()); err != nil {
		t.Fatalf("the two halves do not make the verifier: %v", err)
	}
}

// Without --expires the expiry is 365 days after issue; with one beyond 365
// days the command is refused before it connects
// (TestBreakGlassSealedRefusesBadInvocationsBeforeConnecting), and against
// a database too.
func TestBreakGlassIssueExpiry(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	person, _ := seedBreakGlassPerson(t, pool)
	user := strconv.FormatInt(person, 10)
	plain := issueThroughCommand(t, pool, dsn, "--user", user)
	if got := plain.stored.ExpiresAt.Sub(plain.stored.IssuedAt); got != 365*24*time.Hour {
		t.Fatalf("the default lifetime is %s, want 365 days", got)
	}
	short := issueThroughCommand(t, pool, dsn, "--user", user, "--expires", "30d")
	if got := short.stored.ExpiresAt.Sub(short.stored.IssuedAt); got != 30*24*time.Hour {
		t.Fatalf("--expires 30d gave %s", got)
	}
	if run := runBreakGlassAgainst(t, dsn, "issue", "--user", user, "--expires", "400d"); run.code != breakGlassExitUsage {
		t.Fatalf("--expires 400d: exit %d", run.code)
	}
}

// An issue for a suspended or unknown person is refused and writes nothing;
// revoke revokes once, keeps the row, and refuses an unknown id beside a
// valid one without revoking either.
func TestBreakGlassIssueAndRevokeRefusals(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	person, _ := seedBreakGlassPerson(t, pool)
	suspended, _ := seedBreakGlassPerson(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{strconv.FormatInt(suspended, 10), "nobody@autotest.local"} {
		run := runBreakGlassAgainst(t, dsn, "issue", "--user", user)
		if run.code == breakGlassExitOK || credentialLine.MatchString(run.stdout) {
			t.Fatalf("issue for %s: exit %d\n%s", user, run.code, run.stdout)
		}
	}
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_auth.sealed_credentials`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("refused issues stored %d credentials (%v)", stored, err)
	}
	issued := issueThroughCommand(t, pool, dsn, "--user", strconv.FormatInt(person, 10))
	id := issued.stored.ID.String()

	refused := runBreakGlassAgainst(t, dsn, "revoke", id, uuid.NewString())
	if refused.code != breakGlassExitRefused || !strings.Contains(refused.stderr, "no sealed credential") {
		t.Fatalf("revoke beside an unknown id: exit %d, %s", refused.code, refused.stderr)
	}
	if current, _ := adminaccess.ReadSealedCredential(t.Context(), pool, issued.stored.ID); !current.RevokedAt.IsZero() {
		t.Fatal("the valid id was revoked beside an unknown one")
	}
	if run := runBreakGlassAgainst(t, dsn, "revoke", id); run.code != breakGlassExitOK ||
		!strings.Contains(run.stdout, "revoked sealed credential "+id) {
		t.Fatalf("revoke: exit %d, %s %s", run.code, run.stdout, run.stderr)
	}
	if run := runBreakGlassAgainst(t, dsn, "revoke", id); run.code != breakGlassExitRefused ||
		!strings.Contains(run.stderr, "already revoked") {
		t.Fatalf("a second revoke: exit %d, %s", run.code, run.stderr)
	}
	if _, err := adminaccess.VerifySealedCredential(t.Context(), pool, issued.credential, time.Now()); adminaccess.SealedRefusalOf(err) != adminaccess.SealedRefusalRevoked {
		t.Fatalf("a revoked credential verifies: %v", err)
	}
	list := runBreakGlassAgainst(t, dsn, "list")
	if !strings.Contains(list.stdout, id+" revoked") {
		t.Fatalf("the revoked row is not listed: %s", list.stdout)
	}
	var revokes int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event -> 'data' ->> 'action' = 'admin.elevation.credential_revoked'`).Scan(&revokes); err != nil {
		t.Fatal(err)
	}
	if revokes != 1 {
		t.Fatalf("%d revoke events, want 1", revokes)
	}
}
