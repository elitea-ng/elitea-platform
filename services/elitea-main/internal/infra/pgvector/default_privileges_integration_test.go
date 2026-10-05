package pgvector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// langGraphCheckpointMigrationsDDL is the first statement LangGraph's
// PostgresSaver.setup() runs. #6642: when the administrator ran it, the table
// had no grant for the project role and every prediction failed with
// "permission denied for table checkpoint_migrations".
const langGraphCheckpointMigrationsDDL = `CREATE TABLE IF NOT EXISTS checkpoint_migrations (
    v INTEGER PRIMARY KEY
)`

// TestPGXProvisionerGrantsAdministratorCreatedCheckpointTables proves both
// halves of the #6642 fix against a real server:
//
//  1. a table the administrator creates AFTER provisioning is read/write for
//     the project role (default privileges);
//  2. a table the administrator created while no default privilege covered it
//     becomes read/write once the project is provisioned again (re-grant).
func TestPGXProvisionerGrantsAdministratorCreatedCheckpointTables(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the real PgVector provisioning test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if config.Port == 0 {
		t.Fatalf("test database port is outside the current connection contract")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var vectorAvailable bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector'
)`).Scan(&vectorAvailable); err != nil {
		t.Fatal(err)
	}
	if !vectorAvailable {
		t.Skip("the ELITEA_TEST_DATABASE_URL server does not provide the vector extension")
	}

	projectID := int64(1_000_000_000 + (time.Now().UnixNano() % 500_000_000))
	database := fmt.Sprintf("project_%d", projectID)
	role := database + "_user"
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	quotedRole := pgx.Identifier{role}.Sanitize()
	cleanup := func(cleanupContext context.Context) error {
		// The default-privilege entries live in the project database, so
		// dropping it first is what lets the role drop succeed.
		if _, cleanupErr := admin.Exec(cleanupContext, "DROP DATABASE IF EXISTS "+quotedDatabase+" WITH (FORCE)"); cleanupErr != nil {
			return cleanupErr
		}
		_, cleanupErr := admin.Exec(cleanupContext, "DROP ROLE IF EXISTS "+quotedRole)
		return cleanupErr
	}
	if err := cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if cleanupErr := cleanup(cleanupContext); cleanupErr != nil {
			t.Errorf("clean PgVector integration resources: %v", cleanupErr)
		}
	})

	connector, err := NewPGXConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewProvisioner(connector)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		ProjectID: projectID,
		Admin: AdminConnection{
			User:     config.User,
			Password: config.Password,
			Host:     config.Host,
			Port:     uint16(config.Port),
			Database: config.Database,
		},
	}
	created, err := provisioner.Provision(ctx, request)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	adminProjectConfig := config.Copy()
	adminProjectConfig.Database = database
	adminProject, err := pgx.ConnectConfig(ctx, adminProjectConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminProject.Close(context.Background()) })

	// 1. Created by the administrator after provisioning.
	if _, err := adminProject.Exec(ctx, langGraphCheckpointMigrationsDDL); err != nil {
		t.Fatal(err)
	}
	assertTableOwner(ctx, t, adminProject, "checkpoint_migrations", config.User)
	projectURL := strings.Replace(created.ConnectionString, "postgresql+psycopg://", "postgresql://", 1)
	assertProjectRoleReadWrite(ctx, t, projectURL, role, "checkpoint_migrations")

	// 2. Created while no default privilege covered the role: the state every
	// project provisioned before this fix is in.
	if _, err := adminProject.Exec(ctx,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM "+quotedRole); err != nil {
		t.Fatal(err)
	}
	if _, err := adminProject.Exec(ctx, `CREATE TABLE checkpoint_writes (v INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	projectConfig, err := pgx.ParseConfig(projectURL)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := pgx.ConnectConfig(ctx, projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, deniedErr := denied.Exec(ctx, `SELECT count(*) FROM checkpoint_writes`)
	_ = denied.Close(ctx)
	var pgErr *pgconn.PgError
	if deniedErr == nil || !errors.As(deniedErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("precondition: the legacy-state table must be refused to the project role, got %v", deniedErr)
	}

	// A second Provisioner.Provision converges the grants. Production has no
	// caller that does this for an existing project (see grantPublicSchema),
	// so this proves the SQL, not a repair path; existing deployments run the
	// same statements by hand.
	request.Password = created.Password
	if _, err := provisioner.Provision(ctx, request); err != nil {
		t.Fatalf("reprovision error = %v", err)
	}
	assertProjectRoleReadWrite(ctx, t, projectURL, role, "checkpoint_writes")

	// The reprovision also restored the default privilege.
	if _, err := adminProject.Exec(ctx, `CREATE TABLE checkpoints (v INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	assertProjectRoleReadWrite(ctx, t, projectURL, role, "checkpoints")
}

func assertTableOwner(ctx context.Context, t *testing.T, connection *pgx.Conn, table string, want string) {
	t.Helper()
	var owner string
	if err := connection.QueryRow(ctx,
		`SELECT tableowner FROM pg_catalog.pg_tables WHERE schemaname = 'public' AND tablename = $1`,
		table,
	).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != want {
		t.Fatalf("%s owner = %q, want the administrator %q", table, owner, want)
	}
}

func assertProjectRoleReadWrite(ctx context.Context, t *testing.T, projectURL string, role string, table string) {
	t.Helper()
	config, err := pgx.ParseConfig(projectURL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect as the project role: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var currentUser string
	if err := connection.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		t.Fatal(err)
	}
	if currentUser != role {
		t.Fatalf("connected as %q, want the project role %q", currentUser, role)
	}
	quotedTable := pgx.Identifier{table}.Sanitize()
	if _, err := connection.Exec(ctx, "INSERT INTO "+quotedTable+" (v) VALUES (1)"); err != nil {
		t.Fatalf("project role cannot write %s: %v", table, err)
	}
	var count int
	if err := connection.QueryRow(ctx, "SELECT count(*) FROM "+quotedTable).Scan(&count); err != nil {
		t.Fatalf("project role cannot read %s: %v", table, err)
	}
	if count != 1 {
		t.Fatalf("%s rows = %d, want 1", table, count)
	}
}
