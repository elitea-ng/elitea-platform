package pgvector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPGXDropRealPostgres drops what Provision made, twice, and a schema-mode
// project's schema, against a real server (#1211).
func TestPGXDropRealPostgres(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the real PgVector drop test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var vectorAvailable bool
	if err := admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector')`,
	).Scan(&vectorAvailable); err != nil {
		t.Fatal(err)
	}
	if !vectorAvailable {
		t.Skip("the ELITEA_TEST_DATABASE_URL server does not provide the vector extension")
	}

	projectID := int64(1_000_000_000 + (time.Now().UnixNano() % 400_000_000))
	database, role := ProjectDatabaseName(projectID), ProjectRoleName(projectID)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
		_, _ = admin.Exec(c, "DROP SCHEMA IF EXISTS "+pgx.Identifier{database}.Sanitize()+" CASCADE")
	})

	connector, err := NewPGXConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatal(err)
	}
	adminConnection := AdminConnection{
		User: config.User, Password: config.Password, Host: config.Host, Port: config.Port, Database: config.Database,
	}
	present := func(query, name string) bool {
		var ok bool
		if err := admin.QueryRow(ctx, query, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	dbExists := func() bool { return present(databaseExistsSQL, database) }
	roleExists := func() bool { return present(roleExistsSQL, role) }

	if _, err := provisioner.Provision(ctx, Request{ProjectID: projectID, Admin: adminConnection}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !dbExists() || !roleExists() {
		t.Fatal("premise: provisioning left no database or role")
	}

	// Exists sees the database and the role, read-only.
	if found, err := provisioner.Exists(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection}); err != nil || !found {
		t.Fatalf("Exists after provisioning = %v, %v; want true", found, err)
	}

	// A connected worker must not block the drop (WITH FORCE).
	project, err := pgx.Connect(ctx, databaseURLFor(databaseURL, database))
	if err == nil {
		t.Cleanup(func() { _ = project.Close(context.Background()) })
	}

	first, err := provisioner.Drop(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection})
	if err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if !first.DatabaseDropped || !first.RoleDropped || dbExists() || roleExists() {
		t.Fatalf("first drop = %+v, database exists=%v role exists=%v", first, dbExists(), roleExists())
	}
	if found, err := provisioner.Exists(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection}); err != nil || found {
		t.Fatalf("Exists after the drop = %v, %v; want false", found, err)
	}
	second, err := provisioner.Drop(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection})
	if err != nil || second != (DropResult{}) {
		t.Fatalf("second drop = %+v, %v; want a clean no-op", second, err)
	}
	// Only the database gone (role orphan) is also fine.
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if found, err := provisioner.Exists(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection}); err != nil || !found {
		t.Fatalf("Exists with only the role = %v, %v; want true", found, err)
	}
	third, err := provisioner.Drop(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection})
	if err != nil || third.DatabaseDropped || !third.RoleDropped || roleExists() {
		t.Fatalf("role-only drop = %+v, %v", third, err)
	}

	// Schema mode: only the project's schema goes; the source database stays.
	if _, err := provisioner.Provision(ctx, Request{ProjectID: projectID, Admin: adminConnection, Mode: ModeSchema}); err != nil {
		t.Fatalf("Provision schema: %v", err)
	}
	if !present(schemaExistsSQL, database) {
		t.Fatal("premise: schema mode created no schema")
	}
	sch, err := provisioner.Drop(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection, Mode: ModeSchema})
	if err != nil || !sch.SchemaDropped || present(schemaExistsSQL, database) {
		t.Fatalf("schema drop = %+v, %v", sch, err)
	}
	if !present(databaseExistsSQL, config.Database) {
		t.Fatal("the schema-mode drop removed the source database")
	}
}

func databaseURLFor(base, database string) string {
	config, err := pgx.ParseConfig(base)
	if err != nil {
		return base
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s", config.User, config.Password, config.Host, config.Port, database)
}

// TestPGXDropAsANonSuperuserAdminWithAConnectedProjectSession is the managed
// Postgres shape (#1211): the bootstrap admin is a plain CREATEROLE CREATEDB
// role, not a superuser, and the project role has a live session. On PG16+ such
// an admin cannot terminate that session unless it is a member of the role, so
// a bare DROP DATABASE ... WITH (FORCE) fails.
func TestPGXDropAsANonSuperuserAdminWithAConnectedProjectSession(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the real PgVector drop test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	super, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = super.Close(context.Background()) })

	suffix := time.Now().UnixNano() % 400_000_000
	projectID := 1_400_000_000 + suffix
	adminRole := fmt.Sprintf("bootstrap_admin_%d", projectID)
	database, role := ProjectDatabaseName(projectID), ProjectRoleName(projectID)
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	t.Cleanup(func() {
		c := context.Background()
		_, _ = super.Exec(c, "DROP DATABASE IF EXISTS "+quote(database)+" WITH (FORCE)")
		_, _ = super.Exec(c, "DROP ROLE IF EXISTS "+quote(role))
		_, _ = super.Exec(c, "DROP ROLE IF EXISTS "+quote(adminRole))
	})

	const adminPassword = "admin-pw"
	if _, err := super.Exec(ctx, "CREATE ROLE "+quote(adminRole)+" LOGIN CREATEROLE CREATEDB PASSWORD '"+adminPassword+"'"); err != nil {
		t.Fatal(err)
	}
	var isSuper bool
	if err := super.QueryRow(ctx, `SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = $1`, adminRole).Scan(&isSuper); err != nil || isSuper {
		t.Fatalf("premise: the bootstrap admin must not be a superuser (%v, %v)", isSuper, err)
	}

	// Create the project's pair AS the non-superuser admin, the way production does.
	adminConfig := config.Copy()
	adminConfig.User, adminConfig.Password = adminRole, adminPassword
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quote(role)+" LOGIN PASSWORD 'project-pw'"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(database)); err != nil {
		t.Fatal(err)
	}

	projectConfig := config.Copy()
	projectConfig.User, projectConfig.Password, projectConfig.Database = role, "project-pw", database
	session, err := pgx.ConnectConfig(ctx, projectConfig)
	if err != nil {
		t.Fatalf("premise: the project role could not connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	connector, err := NewPGXConnector(adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provisioner.Drop(ctx, DropRequest{
		ProjectID: projectID,
		Admin: AdminConnection{
			User: adminRole, Password: adminPassword, Host: config.Host, Port: config.Port, Database: config.Database,
		},
	})
	if err != nil {
		t.Fatalf("Drop as a non-superuser admin: %v", err)
	}
	if !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("result = %+v", result)
	}
	var left int
	if err := super.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM pg_catalog.pg_database WHERE datname = $1)
		      + (SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname = $2)`, database, role).Scan(&left); err != nil || left != 0 {
		t.Fatalf("database or role left behind: %d, %v", left, err)
	}
}

// TestPGXDropLockTimeoutAgainstARealLockHolder: another session holds the
// project's advisory lock (a provision in flight). The drop waits in the server
// under lock_timeout, gets SQLSTATE 55P03, and answers ErrDropLockTimeout with
// nothing dropped; once the holder lets go, the same drop goes through.
func TestPGXDropLockTimeoutAgainstARealLockHolder(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the real PgVector drop test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	projectID := int64(1_500_000_000 + (time.Now().UnixNano() % 400_000_000))
	database, role := ProjectDatabaseName(projectID), ProjectRoleName(projectID)
	for _, statement := range []string{
		"CREATE ROLE " + pgx.Identifier{role}.Sanitize(),
		"CREATE DATABASE " + pgx.Identifier{database}.Sanitize(),
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})

	holder, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	if _, err := holder.Exec(ctx, acquireProjectLockSQL, projectLockNamespace, int32(projectID)); err != nil {
		t.Fatal(err)
	}

	previous := dropLockTimeout
	dropLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { dropLockTimeout = previous })

	connector, err := NewPGXConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatal(err)
	}
	request := DropRequest{ProjectID: projectID, Admin: AdminConnection{Database: config.Database}}

	started := time.Now()
	if _, err := provisioner.Drop(ctx, request); !errors.Is(err, ErrDropLockTimeout) {
		t.Fatalf("Drop() under a held lock = %v, want ErrDropLockTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the bounded wait took %s", elapsed)
	}
	var databaseLeft bool
	if err := admin.QueryRow(ctx, databaseExistsSQL, database).Scan(&databaseLeft); err != nil || !databaseLeft {
		t.Fatalf("a timed-out drop removed the database (exists=%v, err=%v)", databaseLeft, err)
	}

	if _, err := holder.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock($1, $2)`, projectLockNamespace, int32(projectID)); err != nil {
		t.Fatal(err)
	}
	result, err := provisioner.Drop(ctx, request)
	if err != nil || !result.DatabaseDropped || !result.RoleDropped {
		t.Fatalf("Drop() after the lock was released = %+v, %v", result, err)
	}
}
