package runtimecomposition_test

// TestCurrentArtifactsConfigurationCreatePersistsThroughGenericMutationService
// proves issue #251 item 2 end to end: the legacy elitea_core s3_credentials
// and bucket_permissions modules have no dedicated Go route because they are
// ported onto the existing generic Configurations model (types "s3" and
// "s3_api_credentials", validated by configurations.CurrentArtifactsDataNormalizer)
// — this asserts a real create through CurrentConfigurationMutationService
// actually persists the normalized row in Postgres, not just that the
// normalizer unit-validates data in isolation (#128 pattern).
//
// This lives in its own external test package (not internal/infra/db/repos,
// where the rest of this suite's harness helpers are defined) because
// runtimecomposition already imports repos: an internal repos test file
// importing runtimecomposition back would be a real import cycle, not just a
// style choice. Duplicating the minimal schema bootstrap here follows this
// codebase's existing per-file harness convention (see e.g. the social and
// configuration_validation postgres integration tests).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentArtifactsConfigurationCreatePersistsThroughGenericMutationService(t *testing.T) {
	pool := newCurrentArtifactsConfigurationPostgresPool(t)
	applyCurrentArtifactsConfigurationMigrations(t, pool)
	prepareCurrentArtifactsConfigurationVault(t, pool)

	runtime, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Destroy)
	service, err := runtime.NewMutationService(currentArtifactsSDKValidatorStub{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("s3 credentials persist", func(t *testing.T) {
		created, err := service.Create(ctx, configurationapp.CurrentConfigurationCreateRequest{
			ProjectID:   1,
			AuthorID:    42,
			EliteaTitle: "primary-bucket-credentials",
			Type:        "s3",
			Data: map[string]any{
				"access_key":             "AKIA-integration",
				"secret_access_key":      "s3-secret",
				"region_name":            "us-east-1",
				"use_compatible_storage": false,
				"storage_url":            "https://s3.us-east-1.amazonaws.com",
			},
		})
		if err != nil {
			t.Fatalf("create s3 configuration: %v", err)
		}
		if created.ID <= 0 {
			t.Fatalf("created configuration=%#v", created)
		}

		var storedType, storedTitle string
		var storedData map[string]any
		if err := pool.QueryRow(ctx,
			`SELECT type, elitea_title, data FROM p_1.configuration WHERE uuid = $1`,
			created.UUID,
		).Scan(&storedType, &storedTitle, &storedData); err != nil {
			t.Fatalf("read persisted s3 configuration: %v", err)
		}
		if storedType != "s3" || storedTitle != "primary-bucket-credentials" {
			t.Fatalf("persisted type=%q title=%q", storedType, storedTitle)
		}
		if storedData["region_name"] != "us-east-1" || storedData["storage_url"] != "https://s3.us-east-1.amazonaws.com" {
			t.Fatalf("persisted data=%#v", storedData)
		}
	})

	t.Run("s3_api_credentials with bucket_permissions persist", func(t *testing.T) {
		created, err := service.Create(ctx, configurationapp.CurrentConfigurationCreateRequest{
			ProjectID:   1,
			AuthorID:    42,
			EliteaTitle: "scoped-bucket-grant",
			Type:        "s3_api_credentials",
			Data: map[string]any{
				"access_key_id": "grant-access-key",
				"user_id":       7,
				"permissions":   []any{"read", "write"},
				"bucket_permissions": map[string]any{
					"reports": []any{"read"},
					"uploads": []any{"read", "write"},
				},
			},
		})
		if err != nil {
			t.Fatalf("create s3_api_credentials configuration: %v", err)
		}

		var storedData map[string]any
		if err := pool.QueryRow(ctx,
			`SELECT data FROM p_1.configuration WHERE uuid = $1`,
			created.UUID,
		).Scan(&storedData); err != nil {
			t.Fatalf("read persisted s3_api_credentials configuration: %v", err)
		}
		bucketPermissions, ok := storedData["bucket_permissions"].(map[string]any)
		if !ok {
			t.Fatalf("persisted data missing bucket_permissions: %#v", storedData)
		}
		uploads, ok := bucketPermissions["uploads"].([]any)
		if !ok || len(uploads) != 2 || uploads[0] != "read" || uploads[1] != "write" {
			t.Fatalf("persisted bucket_permissions.uploads=%#v", bucketPermissions["uploads"])
		}
	})

	t.Run("invalid s3_api_credentials rejected before any write", func(t *testing.T) {
		_, err := service.Create(ctx, configurationapp.CurrentConfigurationCreateRequest{
			ProjectID:   1,
			AuthorID:    42,
			EliteaTitle: "invalid-grant",
			Type:        "s3_api_credentials",
			Data: map[string]any{
				"access_key_id": "grant-access-key",
				// user_id is required by normalizeCurrentArtifactsS3APICredentials.
			},
		})
		if err == nil {
			t.Fatal("expected validation error for missing user_id")
		}
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM p_1.configuration WHERE elitea_title = 'invalid-grant'`,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rejected configuration was persisted anyway: count=%d", count)
		}
	})
}

type currentArtifactsSDKValidatorStub struct{}

func (currentArtifactsSDKValidatorStub) ValidateCurrentSDKConfiguration(
	context.Context,
	configurationapp.CurrentSDKConfigurationValidationRequest,
) error {
	return nil
}

func newCurrentArtifactsConfigurationPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_artifacts_config_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	var serverVersion int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&serverVersion); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("read PostgreSQL server version: %v", err)
	}
	if serverVersion < 160000 || serverVersion >= 190000 {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("service-integration gate requires PostgreSQL 16 through 18, got server_version_num=%d", serverVersion)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func applyCurrentArtifactsConfigurationMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, create_success, suspended)
VALUES (1, TRUE, FALSE);
CREATE SCHEMA p_1;
CREATE TABLE p_1.application_versions (
    id SERIAL PRIMARY KEY,
    llm_settings JSONB NOT NULL DEFAULT '{}'::jsonb
);
-- The skills pair is part of the minimum project schema because the tenant
-- history alters it (tenant/0122_skill_publishing.sql, #249), the same way
-- application_versions above is here for 0121.
CREATE TABLE p_1.skills (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    owner_id INTEGER NOT NULL,
    author_id INTEGER NOT NULL,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.skill_versions (
    id SERIAL PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_1.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL DEFAULT 'base',
    instructions TEXT NOT NULL,
    author_id INTEGER NOT NULL,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.configuration (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE,
    type VARCHAR NOT NULL,
    section VARCHAR NOT NULL,
    data JSONB NOT NULL,
    meta JSONB NOT NULL,
    shared BOOLEAN NOT NULL,
    status_ok BOOLEAN NOT NULL,
    status_logs TEXT,
    source VARCHAR NOT NULL,
    author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);`); err != nil {
		t.Fatalf("preseed minimum legacy project schemas: %v", err)
	}

	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply embedded tenant migrations: %v", err)
	}
}

func prepareCurrentArtifactsConfigurationVault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE TABLE centry.secrets_key (
    id TEXT PRIMARY KEY,
    data BYTEA
);
CREATE TABLE centry.secrets_data (
    id TEXT PRIMARY KEY,
    data BYTEA
);`); err != nil {
		t.Fatalf("prepare current project vault schema: %v", err)
	}
	// Fixture key/token pair copied from
	// internal/infra/db/repos/secret_vault_test.go (test-only fernet
	// material, not a real secret) — this package can't import that
	// unexported constant across the package boundary.
	const projectKey = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	const vaultToken = "gAAAAABlU_EBsLGys7S1tre4ubq7vL2-vx1XMkCj-NAPVrz2qJjob7g8g2X5uZRKHkqRYf3PrTLUC8Q1IHnCMja09Xr6VixBNDJNqcJhTDidsE3D9XlcDpLfJ6e5zNz6DsTP67crLz-PvCJO0qwoNSpc2vwiLlTkf2xnyvlVOAMXlrmueSNrVxUoOGRzpK_fci7UQqhXtn2DDrjEgHLzW77baCUbY6nqH4w48HOBwzsCN7Y6dpkZkns7IK5pFKZs4WwYxYbAU6Q0"
	if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_key (id, data) VALUES ('project-1', $1)`, []byte(projectKey)); err != nil {
		t.Fatalf("prepare current project vault key: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_data (id, data) VALUES ('project-1', $1)`, []byte(vaultToken)); err != nil {
		t.Fatalf("prepare current project vault data: %v", err)
	}
}
