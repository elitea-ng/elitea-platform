package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
)

func TestComputeOrphansKeepsOnlyExactProjectNamesWithoutARow(t *testing.T) {
	t.Parallel()

	got, owned := computeOrphans(
		map[int64]struct{}{1: {}},
		nil,
		[]string{"project_1", "project_2", "project_007", "postgres", "project_3", "project_2_user", "project_abc"},
		[]string{"project_1_user", "project_2_user", "project_4_user", "project_5", "project_3_users", "postgres"},
	)
	if len(owned) != 0 {
		t.Fatalf("journal-owned ids without a journal: %v", owned)
	}
	want := "2:project_2,project_2_user;3:project_3;4:project_4_user"
	var parts []string
	for _, o := range got {
		parts = append(parts, fmt.Sprintf("%d:%s", o.ProjectID, strings.Join(o.names(), ",")))
	}
	if joined := strings.Join(parts, ";"); joined != want {
		t.Fatalf("orphans = %s, want %s", joined, want)
	}
}

// An id whose delete still has cleanup pending in the journal is the journal's
// to drop: it is reported apart and never offered as an orphan.
func TestComputeOrphansLeavesJournalOwnedIDsToTheJournal(t *testing.T) {
	t.Parallel()

	orphans, owned := computeOrphans(
		map[int64]struct{}{1: {}},
		map[int64]struct{}{3: {}},
		[]string{"project_1", "project_2", "project_3"},
		[]string{"project_2_user", "project_3_user"},
	)
	if len(orphans) != 1 || orphans[0].ProjectID != 2 {
		t.Fatalf("orphans = %+v, want only project 2", orphans)
	}
	if len(owned) != 1 || owned[0] != 3 {
		t.Fatalf("journal-owned = %v, want [3]", owned)
	}
	if _, err := selectConfirmed(orphans, []string{"project_3", "project_3_user"}); err == nil {
		t.Fatal("a journal-owned id could be confirmed for a drop")
	}
}

func TestSelectConfirmed(t *testing.T) {
	t.Parallel()

	orphans, _ := computeOrphans(nil, nil, []string{"project_2", "project_3"}, []string{"project_2_user"})

	if _, err := selectConfirmed(orphans, nil); err == nil {
		t.Error("--drop without --confirm was accepted")
	}
	if _, err := selectConfirmed(orphans, []string{"project_9"}); err == nil {
		t.Error("a name that is not an orphan was accepted")
	}
	if _, err := selectConfirmed(orphans, []string{"postgres"}); err == nil {
		t.Error("a system name was accepted")
	}
	if _, err := selectConfirmed(orphans, []string{"project_2"}); err == nil {
		t.Error("a partial confirmation (database without its role) was accepted")
	}
	selected, err := selectConfirmed(orphans, []string{"project_2", "project_2_user"})
	if err != nil || len(selected) != 1 || selected[0].ProjectID != 2 {
		t.Fatalf("selected = %+v, %v; want only project 2", selected, err)
	}
}

func existsOnServer(ctx context.Context, t *testing.T, admin *pgx.Conn, database, role string) (bool, bool) {
	t.Helper()
	var d, r bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1), EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$2)`,
		database, role).Scan(&d, &r); err != nil {
		t.Fatal(err)
	}
	return d, r
}

func TestOrphanFractionCountsEveryProjectIDOnTheServer(t *testing.T) {
	t.Parallel()

	databases := []string{"project_1", "project_2", "project_3", "project_4", "postgres", "project_007"}
	roles := []string{"project_1_user", "project_5_user"}
	if got := serverProjectCount(databases, roles); got != 5 {
		t.Fatalf("serverProjectCount = %d, want 5 distinct canonical ids", got)
	}
	r := report{ServerProjects: 5, Orphans: []orphan{{ProjectID: 4}}}
	if got := r.orphanFraction(); got != 0.2 {
		t.Fatalf("orphanFraction = %v, want 0.2", got)
	}
	if got := (report{}).orphanFraction(); got != 0 {
		t.Fatalf("empty report fraction = %v", got)
	}
}

func TestRunRefusesAnOutOfRangeFraction(t *testing.T) {
	t.Parallel()

	lookup := func(string) (string, bool) { return "", false }
	var out, errOut bytes.Buffer
	args := []string{"--pgvector-url", "postgres://x", "--database-url", "postgres://y", "--max-orphan-fraction", "1.5"}
	if code := run(context.Background(), args, lookup, &out, &errOut); code != exitInvalidUsage {
		t.Errorf("exit = %d", code)
	}
}

func TestRunRefusesBadUsage(t *testing.T) {
	t.Parallel()

	lookup := func(string) (string, bool) { return "", false }
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, lookup, &out, &errOut); code != exitInvalidUsage {
		t.Errorf("no URLs: exit = %d", code)
	}
	if code := run(context.Background(),
		[]string{"--pgvector-url", "postgres://x", "--database-url", "postgres://y", "--confirm", "project_1"},
		lookup, &out, &errOut); code != exitInvalidUsage {
		t.Errorf("--confirm without --drop: exit = %d", code)
	}
}

// TestRunAgainstPostgres lists, refuses an unconfirmed drop, then drops.
func TestRunAgainstPostgres(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the orphan command against Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	// The platform side needs a centry.project table; use a scratch database.
	id := int64(1_900_000_000 + time.Now().UnixNano()%90_000_000)
	database, role := fmt.Sprintf("project_%d", id), fmt.Sprintf("project_%d_user", id)
	platformDB := fmt.Sprintf("orphans_platform_%d", id)
	for _, s := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN`, pgx.Identifier{role}.Sanitize()),
		fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{database}.Sanitize()),
		fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{platformDB}.Sanitize()),
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pgx.Identifier{database}.Sanitize()))
		_, _ = admin.Exec(c, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, pgx.Identifier{role}.Sanitize()))
		_, _ = admin.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pgx.Identifier{platformDB}.Sanitize()))
	})

	cfg, _ := pgx.ParseConfig(url)
	cfg.Database = platformDB
	platformURL := withDatabase(t, url, platformDB)
	platform, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.Close(context.Background()) }()
	if _, err := platform.Exec(ctx, `CREATE SCHEMA centry; CREATE TABLE centry.project (id integer PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	lookup := func(string) (string, bool) { return "", false }
	base := []string{"--pgvector-url", url, "--database-url", platformURL}

	// An empty platform database is refused outright: with no projects every
	// project_<id> database on the server would look orphaned.
	var emptyOut, emptyErr bytes.Buffer
	if code := run(ctx, base, lookup, &emptyOut, &emptyErr); code != exitFailure ||
		!strings.Contains(emptyErr.String(), "no projects") {
		t.Fatalf("empty platform database: exit %d, err %q", code, emptyErr.String())
	}
	if d, r := existsOnServer(ctx, t, admin, database, role); !d || !r {
		t.Fatal("the refused run dropped something")
	}
	// A bystander project that has a row, so the platform database is plausible.
	if _, err := platform.Exec(ctx, `INSERT INTO centry.project (id) VALUES (1), (7)`); err != nil {
		t.Fatal(err)
	}
	exists := func() (bool, bool) {
		var d, r bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1), EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$2)`,
			database, role).Scan(&d, &r); err != nil {
			t.Fatal(err)
		}
		return d, r
	}

	var out, errOut bytes.Buffer
	if code := run(ctx, base, lookup, &out, &errOut); code != exitOK || !strings.Contains(out.String(), database) {
		t.Fatalf("dry run: exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	if d, r := exists(); !d || !r {
		t.Fatal("the dry run dropped something")
	}

	// The header names the database being read and the project range.
	for _, want := range []string{"platform database: " + platformDB, "current_database() = " + platformDB, "2 project(s), id range 1..7"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("header lacks %q:\n%s", want, out.String())
		}
	}

	// The fraction guard: with a limit of 0 any orphan refuses --drop, and
	// nothing is dropped; --force-fraction overrides it.
	out.Reset()
	errOut.Reset()
	confirmBoth := database + "," + role
	if code := run(ctx, append(base, "--drop", "--confirm", confirmBoth, "--max-orphan-fraction", "0"), lookup, &out, &errOut); code != exitInvalidUsage ||
		!strings.Contains(errOut.String(), "--force-fraction") {
		t.Fatalf("fraction guard: exit %d, err %q", code, errOut.String())
	}
	if d, r := exists(); !d || !r {
		t.Fatal("the fraction guard let a drop through")
	}

	out.Reset()
	errOut.Reset()
	if code := run(ctx, append(base, "--drop"), lookup, &out, &errOut); code != exitInvalidUsage {
		t.Fatalf("--drop without --confirm: exit %d", code)
	}
	if d, r := exists(); !d || !r {
		t.Fatal("--drop without --confirm dropped something")
	}

	// A project that has a row is not an orphan and is not offered.
	if _, err := platform.Exec(ctx, `INSERT INTO centry.project (id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(ctx, base, lookup, &out, &errOut); code != exitOK || strings.Contains(out.String(), database) {
		t.Fatalf("a live project was listed as an orphan: %q", out.String())
	}
	if _, err := platform.Exec(ctx, `DELETE FROM centry.project WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	confirm := database + "," + role
	if code := run(ctx, append(base, "--drop", "--confirm", confirm, "--force-fraction"), lookup, &out, &errOut); code != exitOK {
		t.Fatalf("confirmed drop: exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	if d, r := exists(); d || r {
		t.Fatalf("after the confirmed drop: database=%v role=%v", d, r)
	}
}

// redact: every sentinel maps to its fixed text, wrapped or not; a raw Postgres
// error is reduced to its SQLSTATE; nothing else leaks.
func TestRedactMapsKnownSentinelsAndKeepsTheSQLState(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"lock timeout":       {pgvector.ErrDropLockTimeout, "timed out waiting for the project's PgVector advisory lock"},
		"wrapped lock":       {fmt.Errorf("drop: %w", pgvector.ErrDropLockTimeout), "timed out waiting for the project's PgVector advisory lock"},
		"invalid target":     {fmt.Errorf("%w: role", pgvector.ErrInvalidDropTarget), "refusing to drop a name that is not the project's own"},
		"no projects":        {errNoProjects, "the platform database has no projects"},
		"orphan fraction":    {errOrphanFraction, "--force-fraction"},
		"parse platform URL": {errParsePlatformURL, "cannot parse --database-url"},
		"parse admin URL":    {errParseAdminURL, "cannot parse --pgvector-url"},
		"connect platform":   {errConnectPlatform, "cannot connect to the platform database"},
		"connect admin":      {errConnectAdmin, "cannot connect to the PgVector admin database"},
		"invalid request":    {fmt.Errorf("%w: the admin connection names no database", pgvector.ErrInvalidRequest), "check that --pgvector-url names the admin database"},
		"invalid connector":  {pgvector.ErrInvalidConnector, "the PgVector admin connection is not usable"},
		"provisioning":       {fmt.Errorf("%w: drop project database", pgvector.ErrProvisioning), "a statement on the PgVector server failed"},
		"raw postgres error": {fmt.Errorf("exec: %w", &pgconn.PgError{Code: "42501", Message: `permission denied for database "project_7"`}), "postgres error 42501"},
		"unknown":            {errors.New("dial tcp 10.1.2.3:5432: password=hunter2"), "operation failed"},
		"context deadline":   {context.DeadlineExceeded, "context deadline exceeded"},
	} {
		got := redact(tc.err).Error()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: redact = %q, want it to contain %q", name, got, tc.want)
		}
		for _, leak := range []string{"hunter2", "10.1.2.3", "project_7", "permission denied"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: redact leaks %q: %q", name, leak, got)
			}
		}
	}
	if redact(nil) != nil {
		t.Error("redact(nil) must be nil")
	}
}

// A run against URLs that cannot be parsed or reached names which one, and
// never prints either URL or its credentials.
func TestRunNamesTheFailingConnectionWithoutEchoingTheURL(t *testing.T) {
	t.Parallel()

	lookup := func(string) (string, bool) { return "", false }
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"admin URL unparsable": {
			[]string{"--pgvector-url", "postgres://admin:hunter2@[bad", "--database-url", "postgres://u:hunter2@127.0.0.1:1/db"},
			"cannot parse --pgvector-url",
		},
		"platform URL unparsable": {
			[]string{"--pgvector-url", "postgres://admin:hunter2@127.0.0.1:1/db", "--database-url", "postgres://u:hunter2@[bad"},
			"cannot parse --database-url",
		},
		"platform unreachable": {
			[]string{"--pgvector-url", "postgres://admin:hunter2@127.0.0.1:1/db", "--database-url", "postgres://u:hunter2@127.0.0.1:1/db"},
			"cannot connect to the platform database",
		},
	} {
		var out, errOut bytes.Buffer
		code := run(context.Background(), tc.args, lookup, &out, &errOut)
		if code == exitOK {
			t.Errorf("%s: exit = %d", name, code)
		}
		if !strings.Contains(errOut.String(), tc.want) {
			t.Errorf("%s: stderr = %q, want %q", name, errOut.String(), tc.want)
		}
		for _, leak := range []string{"hunter2", "127.0.0.1", "[bad"} {
			if strings.Contains(errOut.String()+out.String(), leak) {
				t.Errorf("%s: output leaks %q: %s%s", name, leak, errOut.String(), out.String())
			}
		}
	}
}

// TestRunRechecksEachIDBeforeItsDropAndLeavesTheJournalItsIDs is the drop-time
// safety net (#1211): the listing is old by the time a drop runs. An id that
// has a project row by then, or whose delete has cleanup pending in the
// journal, is skipped; an id the journal owned at listing time is never offered
// at all; the rest is dropped. The admin URL names no database, so the tool
// connects to `postgres`.
func TestRunRechecksEachIDBeforeItsDropAndLeavesTheJournalItsIDs(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the orphan command against Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	base := int64(1_700_000_000 + time.Now().UnixNano()%90_000_000)
	recreated, journalLate, journalOwned, dropped := base, base+1, base+2, base+3
	ids := []int64{recreated, journalLate, journalOwned, dropped}
	platformDB := fmt.Sprintf("orphans_recheck_%d", base)
	statements := []string{fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{platformDB}.Sanitize())}
	for _, id := range ids {
		statements = append(statements,
			fmt.Sprintf(`CREATE ROLE %s`, pgx.Identifier{fmt.Sprintf("project_%d_user", id)}.Sanitize()),
			fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{fmt.Sprintf("project_%d", id)}.Sanitize()))
	}
	for _, s := range statements {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, id := range ids {
			_, _ = admin.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pgx.Identifier{fmt.Sprintf("project_%d", id)}.Sanitize()))
			_, _ = admin.Exec(c, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, pgx.Identifier{fmt.Sprintf("project_%d_user", id)}.Sanitize()))
		}
		_, _ = admin.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, pgx.Identifier{platformDB}.Sanitize()))
	})

	cfg, _ := pgx.ParseConfig(url)
	cfg.Database = platformDB
	platform, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.Close(context.Background()) }()
	if _, err := platform.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (id integer PRIMARY KEY);
CREATE TABLE centry.project_deletions (project_id bigint PRIMARY KEY, completed_at timestamptz);
INSERT INTO centry.project (id) VALUES (1), (2);`); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Exec(ctx,
		`INSERT INTO centry.project_deletions (project_id, completed_at) VALUES ($1, NULL), (1, now())`, journalOwned); err != nil {
		t.Fatal(err)
	}

	beforeDrop = func(o orphan) {
		var statement string
		switch o.ProjectID {
		case recreated:
			statement = `INSERT INTO centry.project (id) VALUES ($1)`
		case journalLate:
			statement = `INSERT INTO centry.project_deletions (project_id) VALUES ($1)`
		default:
			return
		}
		if _, err := platform.Exec(ctx, statement, o.ProjectID); err != nil {
			t.Errorf("hook for %d: %v", o.ProjectID, err)
		}
	}
	t.Cleanup(func() { beforeDrop = func(orphan) {} })

	adminURL := withDatabase(t, url, "")
	platformURL := withDatabase(t, url, platformDB)
	lookup := func(string) (string, bool) { return "", false }

	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"--pgvector-url", adminURL, "--database-url", platformURL}, lookup, &out, &errOut); code != exitOK {
		t.Fatalf("dry run: exit %d, err %q", code, errOut.String())
	}
	if strings.Contains(out.String(), fmt.Sprintf("project_%d,", journalOwned)) ||
		!strings.Contains(out.String(), fmt.Sprintf("cleanup journal (not orphans): [%d]", journalOwned)) {
		t.Fatalf("the journal-owned id is offered as an orphan or not reported apart:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "admin database postgres") {
		t.Fatalf("an admin URL with no database did not fall back to postgres:\n%s", out.String())
	}

	var confirm []string
	for _, id := range []int64{recreated, journalLate, dropped} {
		confirm = append(confirm, fmt.Sprintf("project_%d", id), fmt.Sprintf("project_%d_user", id))
	}
	out.Reset()
	errOut.Reset()
	args := []string{"--pgvector-url", adminURL, "--database-url", platformURL,
		"--drop", "--force-fraction", "--confirm", strings.Join(confirm, ",")}
	if code := run(ctx, args, lookup, &out, &errOut); code != exitOK {
		t.Fatalf("drop: exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	for id, wantKept := range map[int64]bool{recreated: true, journalLate: true, journalOwned: true, dropped: false} {
		d, r := existsOnServer(ctx, t, admin, fmt.Sprintf("project_%d", id), fmt.Sprintf("project_%d_user", id))
		if d != wantKept || r != wantKept {
			t.Errorf("project %d: database=%v role=%v, want kept=%v\n%s", id, d, r, wantKept, out.String())
		}
	}
	for _, want := range []string{
		fmt.Sprintf("project %d: skipped, it has a centry.project row now", recreated),
		fmt.Sprintf("project %d: skipped, its delete has cleanup pending", journalLate),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// withDatabase returns raw with its database (the URL path) replaced, keeping
// the query string (sslmode, ...). An empty database yields a URL that names
// none. Test URLs differ between local runs and CI, so no suffix is assumed.
func withDatabase(t *testing.T, raw, database string) string {
	t.Helper()
	u, err := neturl.Parse(raw)
	if err != nil {
		t.Fatalf("parse ELITEA_TEST_DATABASE_URL: %v", err)
	}
	if database == "" {
		u.Path = ""
	} else {
		u.Path = "/" + database
	}
	return u.String()
}
