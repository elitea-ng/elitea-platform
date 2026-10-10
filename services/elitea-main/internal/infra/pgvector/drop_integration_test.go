package pgvector

import (
	"context"
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
	second, err := provisioner.Drop(ctx, DropRequest{ProjectID: projectID, Admin: adminConnection})
	if err != nil || second != (DropResult{}) {
		t.Fatalf("second drop = %+v, %v; want a clean no-op", second, err)
	}
	// Only the database gone (role orphan) is also fine.
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
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
