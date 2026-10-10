package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestComputeOrphansKeepsOnlyExactProjectNamesWithoutARow(t *testing.T) {
	t.Parallel()

	got := computeOrphans(
		map[int64]struct{}{1: {}},
		[]string{"project_1", "project_2", "project_007", "postgres", "project_3", "project_2_user", "project_abc"},
		[]string{"project_1_user", "project_2_user", "project_4_user", "project_5", "project_3_users", "postgres"},
	)
	want := "2:project_2,project_2_user;3:project_3;4:project_4_user"
	var parts []string
	for _, o := range got {
		parts = append(parts, fmt.Sprintf("%d:%s", o.ProjectID, strings.Join(o.names(), ",")))
	}
	if joined := strings.Join(parts, ";"); joined != want {
		t.Fatalf("orphans = %s, want %s", joined, want)
	}
}

func TestSelectConfirmed(t *testing.T) {
	t.Parallel()

	orphans := computeOrphans(nil, []string{"project_2", "project_3"}, []string{"project_2_user"})

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
	platformURL := strings.TrimSuffix(url, "/postgres") + "/" + platformDB
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
