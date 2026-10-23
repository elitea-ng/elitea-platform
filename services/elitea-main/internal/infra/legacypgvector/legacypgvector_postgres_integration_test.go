package legacypgvector

// Drop and List against a real PostgreSQL: the server-wide catalogue, the
// FORCE that disconnects a live session, and the managed-PostgreSQL admin (a
// CREATEROLE/CREATEDB role that is not a superuser), whose right to terminate
// the project role's sessions is the grant Drop gives itself on PG16+.

import (
	"context"
	"math/rand/v2"
	"net/url"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

func superuserURL(t *testing.T) string {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	return databaseURL
}

func exec(ctx context.Context, t *testing.T, databaseURL string, statements ...string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// randomID draws an id no other test uses: database and role names are
// server-wide.
func randomID() int64 { return 1_000_000 + rand.Int64N(1_000_000_000) }

// withUser returns databaseURL with its credentials replaced.
func withUser(t *testing.T, databaseURL, user, password string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String()
}

func TestDropRemovesTheDatabaseAndRoleUnderANonSuperuserAdmin(t *testing.T) {
	root := superuserURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The admin the old store ran as on managed PostgreSQL: it created the
	// project role and database, and is no superuser.
	admin := "legacy_admin_" + strconv.FormatInt(randomID(), 10)
	id := randomID()
	database, role := pgx.Identifier{DatabaseName(id)}.Sanitize(), pgx.Identifier{RoleName(id)}.Sanitize()
	exec(ctx, t, root, "CREATE ROLE "+pgx.Identifier{admin}.Sanitize()+" LOGIN PASSWORD 'pw' CREATEROLE CREATEDB")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		conn, err := pgx.Connect(cleanupCtx, root)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		_, _ = conn.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+role)
		_, _ = conn.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+pgx.Identifier{admin}.Sanitize())
	})
	adminURL := withUser(t, root, admin, "pw")
	exec(ctx, t, adminURL,
		"CREATE ROLE "+role+" LOGIN PASSWORD 'project'",
		"CREATE DATABASE "+database,
	)

	// A live session of the project role, as a worker that was attached.
	projectURL := withUser(t, root, RoleName(id), "project")
	parsed, _ := url.Parse(projectURL)
	parsed.Path = "/" + DatabaseName(id)
	session, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatalf("open the project role's session: %v", err)
	}
	defer func() { _ = session.Close(context.Background()) }()

	dropper, err := NewDropper(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := dropper.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(listed, Leftover{ProjectID: id, Database: DatabaseName(id), Role: RoleName(id)}) {
		t.Fatalf("List = %v, want project %d with its database and role", listed, id)
	}
	if present, err := dropper.Exists(ctx, id); err != nil || !present {
		t.Fatalf("Exists = %v, %v", present, err)
	}

	dropped, err := dropper.Drop(ctx, id)
	if err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if !dropped.DatabaseDropped || !dropped.RoleDropped {
		t.Fatalf("Drop = %+v, want both dropped", dropped)
	}
	if present, err := dropper.Exists(ctx, id); err != nil || present {
		t.Fatalf("after Drop: Exists = %v, %v", present, err)
	}
	// Idempotent: a second drop is a no-op, not an error.
	again, err := dropper.Drop(ctx, id)
	if err != nil || again.DatabaseDropped || again.RoleDropped {
		t.Fatalf("second Drop = %+v, %v; want a no-op", again, err)
	}
}

// A role whose database is already gone (or the reverse) is dropped alone, and
// List reports the half that is there. Lookalike names are never listed.
func TestListReportsHalvesAndIgnoresLookalikes(t *testing.T) {
	root := superuserURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := randomID()
	lookalike := "project_0" + strconv.FormatInt(id, 10)
	exec(ctx, t, root,
		"CREATE ROLE "+pgx.Identifier{RoleName(id)}.Sanitize(),
		"CREATE ROLE "+pgx.Identifier{lookalike}.Sanitize(),
	)
	t.Cleanup(func() {
		exec(context.Background(), t, root,
			"DROP ROLE IF EXISTS "+pgx.Identifier{RoleName(id)}.Sanitize(),
			"DROP ROLE IF EXISTS "+pgx.Identifier{lookalike}.Sanitize())
	})
	dropper, err := NewDropper(root)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := dropper.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(listed, Leftover{ProjectID: id, Role: RoleName(id)}) {
		t.Fatalf("List = %v, want project %d with its role only", listed, id)
	}
	for _, leftover := range listed {
		if leftover.Role == lookalike || leftover.Database == lookalike {
			t.Fatalf("List read the lookalike %s", lookalike)
		}
	}
	dropped, err := dropper.Drop(ctx, id)
	if err != nil || dropped.DatabaseDropped || !dropped.RoleDropped {
		t.Fatalf("Drop = %+v, %v; want the role alone dropped", dropped, err)
	}
}
