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
)

// TestPGXProvisionerRealPgVector crosses direct pgx connections, PostgreSQL
// catalogs, session advisory locking, database/role DDL and the vector
// extension. It does not exercise configuration lookup, the encrypted vault,
// lifecycle outbox delivery, HTTP/gRPC, the command bus, or a Python worker.
func TestPGXProvisionerRealPgVector(t *testing.T) {
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

	projectID := int64(1_500_000_000 + (time.Now().UnixNano() % 500_000_000))
	database := fmt.Sprintf("project_%d", projectID)
	role := database + "_user"
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	quotedRole := pgx.Identifier{role}.Sanitize()
	cleanup := func(cleanupContext context.Context) error {
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

	start := make(chan struct{})
	outcomes := make(chan provisionOutcome, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			<-start
			result, provisionErr := provisioner.Provision(ctx, request)
			outcomes <- provisionOutcome{result: result, err: provisionErr}
		}()
	}
	close(start)

	var created Result
	createdCount := 0
	passwordRequiredCount := 0
	for resultIndex := 0; resultIndex < 2; resultIndex++ {
		outcome := <-outcomes
		if outcome.err == nil {
			created = outcome.result
			createdCount++
			continue
		}
		var passwordRequired *PasswordRequiredError
		if errors.As(outcome.err, &passwordRequired) {
			passwordRequiredCount++
			continue
		}
		t.Fatalf("concurrent Provision() error = %v", outcome.err)
	}
	if createdCount != 1 || passwordRequiredCount != 1 {
		t.Fatalf("concurrent results: created=%d password_required=%d", createdCount, passwordRequiredCount)
	}

	projectURL := strings.Replace(created.ConnectionString, "postgresql+psycopg://", "postgresql://", 1)
	projectConfig, err := pgx.ParseConfig(projectURL)
	if err != nil {
		t.Fatal(err)
	}
	projectConnection, err := pgx.ConnectConfig(ctx, projectConfig)
	if err != nil {
		t.Fatalf("connect with provisioned credentials: %v", err)
	}
	var currentUser string
	var currentDatabase string
	var extensionInstalled bool
	if err := projectConnection.QueryRow(ctx, `SELECT
current_user,
current_database(),
EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'vector')`).Scan(
		&currentUser,
		&currentDatabase,
		&extensionInstalled,
	); err != nil {
		_ = projectConnection.Close(context.Background())
		t.Fatal(err)
	}
	if err := projectConnection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if currentUser != role || currentDatabase != database || !extensionInstalled {
		t.Fatalf("provisioned target: user=%q database=%q vector=%t", currentUser, currentDatabase, extensionInstalled)
	}

	request.Password = created.Password
	repeated, err := provisioner.Provision(ctx, request)
	if err != nil {
		t.Fatalf("idempotent Provision() error = %v", err)
	}
	if repeated.Status != "password reset" || repeated.ConnectionString != created.ConnectionString {
		t.Fatalf("idempotent result = %+v", repeated)
	}
}

type provisionOutcome struct {
	result Result
	err    error
}
