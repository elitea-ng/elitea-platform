package main

// `elitea-main legacy-pgvector-orphans` against a real PostgreSQL that is both
// the platform server and the legacy one. The platform database is a scratch
// database with the two tables the command reads; the legacy names are
// created server-wide for random high ids, and the command is scoped to them
// (runLegacyOrphans' scope) so another package's concurrent test databases
// are neither counted nor dropped.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacypgvector"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
)

func TestLegacyPgvectorOrphansListsThenDropsOnlyOrphans(t *testing.T) {
	const environment = "ELITEA_TEST_DATABASE_URL"
	root := testpg.DSN(os.Getenv(environment))
	if root == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	mustExec := func(conn *pgx.Conn, statement string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	platformName := fmt.Sprintf("elitea_legacy_orphans_%d_%d", os.Getpid(), time.Now().UnixNano())
	mustExec(admin, "CREATE DATABASE "+pgx.Identifier{platformName}.Sanitize())
	base := 1_000_000 + rand.Int64N(1_000_000_000)
	// orphan: no project, no journal row. live: a project. pending: an open
	// journal row (the journal drops it). finished: a completed journal row,
	// an orphan as well.
	orphan, live, pending, finished := base, base+1, base+2, base+3
	ids := []int64{orphan, live, pending, finished}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		conn, err := pgx.Connect(cleanupCtx, root)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		for _, id := range ids {
			_, _ = conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{legacypgvector.DatabaseName(id)}.Sanitize()+" WITH (FORCE)")
			_, _ = conn.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+pgx.Identifier{legacypgvector.RoleName(id)}.Sanitize())
		}
		_, _ = conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{platformName}.Sanitize()+" WITH (FORCE)")
	})
	for _, id := range ids {
		role := pgx.Identifier{legacypgvector.RoleName(id)}.Sanitize()
		mustExec(admin, "CREATE ROLE "+role)
		mustExec(admin, "CREATE DATABASE "+pgx.Identifier{legacypgvector.DatabaseName(id)}.Sanitize()+" OWNER "+role)
	}

	parsed, err := url.Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + platformName
	platformURL := parsed.String()
	platform, err := pgx.Connect(ctx, platformURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.Close(context.Background()) }()
	mustExec(platform, `CREATE SCHEMA centry`)
	mustExec(platform, `CREATE TABLE centry.project (id integer PRIMARY KEY)`)
	mustExec(platform, `CREATE TABLE centry.project_deletions (project_id integer PRIMARY KEY, completed_at timestamptz)`)
	mustExec(platform, `INSERT INTO centry.project (id) VALUES ($1)`, live)
	mustExec(platform, `INSERT INTO centry.project_deletions (project_id, completed_at) VALUES ($1, NULL), ($2, now())`, pending, finished)

	env := map[string]string{legacyPgvectorAdminURLEnv: root, "DATABASE_URL": platformURL}
	lookup := func(key string) (string, bool) { value, ok := env[key]; return value, ok }
	scope := func(id int64) bool { return slices.Contains(ids, id) }
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runLegacyOrphans(ctx, args, lookup, &stdout, &stderr, scope)
		return code, stdout.String(), stderr.String()
	}
	present := func(id int64) bool {
		var exists bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
			legacypgvector.DatabaseName(id)).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}

	// Dry run: the two orphans are listed, nothing is dropped.
	code, stdout, stderr := run()
	if code != legacyOrphansExitOK || !strings.Contains(stdout, "4 project id(s) with a legacy database or role, 2 orphaned") {
		t.Fatalf("dry run: code %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, id := range []int64{orphan, finished} {
		if !strings.Contains(stdout, "project "+strconv.FormatInt(id, 10)+":") {
			t.Fatalf("dry run does not list orphan %d:\n%s", id, stdout)
		}
	}
	for _, id := range []int64{live, pending} {
		if strings.Contains(stdout, "project "+strconv.FormatInt(id, 10)+":") {
			t.Fatalf("dry run lists owned id %d:\n%s", id, stdout)
		}
	}
	for _, id := range ids {
		if !present(id) {
			t.Fatalf("the dry run dropped project_%d", id)
		}
	}

	// --drop without the right count, or over the fraction limit, is refused.
	if code, _, _ := run("--drop", "--confirm-count", "3", "--force-fraction"); code != legacyOrphansExitInvalidUsage {
		t.Fatalf("a wrong count was accepted: code %d", code)
	}
	if code, _, stderr := run("--drop", "--confirm-count", "2"); code != legacyOrphansExitInvalidUsage || !strings.Contains(stderr, "orphans") {
		t.Fatalf("50%% orphans passed the 20%% limit: code %d, %s", code, stderr)
	}
	for _, id := range ids {
		if !present(id) {
			t.Fatalf("a refused drop dropped project_%d", id)
		}
	}

	code, stdout, stderr = run("--drop", "--confirm-count", "2", "--force-fraction")
	if code != legacyOrphansExitOK {
		t.Fatalf("drop: code %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if present(orphan) || present(finished) {
		t.Fatalf("an orphan survived the drop:\n%s", stdout)
	}
	if !present(live) || !present(pending) {
		t.Fatalf("an owned database was dropped:\n%s", stdout)
	}
	var roles int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)`,
		[]string{legacypgvector.RoleName(orphan), legacypgvector.RoleName(finished)}).Scan(&roles); err != nil || roles != 0 {
		t.Fatalf("orphan roles left: %d (%v)", roles, err)
	}

	// A platform database with no projects is refused outright.
	mustExec(platform, `DELETE FROM centry.project`)
	if code, _, stderr := run(); code != legacyOrphansExitFailure || !strings.Contains(stderr, "no projects") {
		t.Fatalf("an empty platform database was accepted: code %d, %s", code, stderr)
	}
}
