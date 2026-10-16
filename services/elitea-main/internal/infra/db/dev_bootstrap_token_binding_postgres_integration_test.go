package db

// The dev-bootstrap gap, and why a shape assertion could not see it.
//
// RunMigrations is the ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA path. It applies
// internal/infra/db/migrations/*.sql plus a short exempt list, and it never
// applies the ledgered shared corpus that elitea-migrate applies. So a database
// built by that flag alone holds only what this function names.
//
// internal/db/queries/auth_pat.sql's GetActivePATPrincipalByUUID — the single
// query authsvc.LocalValidator runs for EVERY credential — now LEFT JOINs
// elitea_identity.token_project_binding. On a database missing that table the
// query does not degrade: PostgreSQL answers 42P01 and every token-authenticated
// request fails. Migration 0067 hit the same class of problem from the other
// direction (issue #306) and was fixed in this same function; 0071 follows it.
//
// gateway_migrations_test.go asserts the SHAPE of a migration file. This test
// asserts the OUTCOME of the bootstrap: it runs RunMigrations against a real
// empty database and then executes the generated validator query itself. A file
// that exists but is never applied passes every shape assertion and fails here.

import (
	"context"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

const devBootstrapTokenUUID = "9d7c1f1e-2b3a-4c5d-8e9f-0a1b2c3d4e5f"

func TestDevBootstrapBuildsTokenProjectBindingForPATValidation(t *testing.T) {
	pool := newDevBootstrapPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("dev bootstrap RunMigrations: %v", err)
	}

	// to_regclass answers NULL for a name that does not resolve, and answers it
	// without raising — so a missing table is a readable failure here rather
	// than an aborted transaction.
	var regclass *string
	if err := pool.QueryRow(ctx,
		"SELECT to_regclass('elitea_identity.token_project_binding')::text").Scan(&regclass); err != nil {
		t.Fatalf("to_regclass(elitea_identity.token_project_binding): %v", err)
	}
	if regclass == nil {
		t.Fatal("elitea_identity.token_project_binding does not exist after the dev bootstrap. " +
			"GetActivePATPrincipalByUUID LEFT JOINs it on the hot path, so every PAT " +
			"validation on this database answers 42P01. Add the file to " +
			"dumpGuardExemptMigrations in migrate.go, the way 0067 is.")
	}

	var userID, tokenID int32
	if err := pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name) VALUES ('dev-bootstrap@autotest.local', 'Bootstrap')
RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO auth_core__token (uuid, user_id, name) VALUES ($1, $2, 'pat')
RETURNING id`, devBootstrapTokenUUID, userID).Scan(&tokenID); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	queries := sqlcgen.New(pool)

	t.Run("an unbound token validates and resolves no project", func(t *testing.T) {
		principal, err := queries.GetActivePATPrincipalByUUID(ctx, devBootstrapTokenUUID)
		if err != nil {
			t.Fatalf("PAT validation on a dev-bootstrapped database: %v", err)
		}
		if principal.UserID != userID || principal.Email != "dev-bootstrap@autotest.local" {
			t.Fatalf("principal = %+v, want user %d", principal, userID)
		}
		// An unbound token is the default and must stay unbound (spec §3.3).
		if principal.ProjectID != nil {
			t.Errorf("project_id = %d for a token with no binding row, want NULL", *principal.ProjectID)
		}
	})

	t.Run("a bound token resolves its project on the same round trip", func(t *testing.T) {
		const boundProject = int32(77)
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`,
			tokenID, boundProject); err != nil {
			t.Fatalf("insert binding: %v", err)
		}

		principal, err := queries.GetActivePATPrincipalByUUID(ctx, devBootstrapTokenUUID)
		if err != nil {
			t.Fatalf("PAT validation for a bound token: %v", err)
		}
		if principal.ProjectID == nil || *principal.ProjectID != boundProject {
			t.Fatalf("project_id = %v, want %d", principal.ProjectID, boundProject)
		}
	})

	// The SQL half of the suspension refusal. authsvc maps the column to
	// auth.User.TokenProjectActive, and the edge refuses a bound token whose
	// project is not active. Only a real database answers the expression
	// `(bound_project.suspended IS FALSE AND bound_project.create_success IS
	// TRUE)`, so a unit test with a stubbed row cannot cover it.
	t.Run("the bound project lifecycle state rides on the same row", func(t *testing.T) {
		var projectID int32
		if err := pool.QueryRow(ctx, `
INSERT INTO centry.project (name, owner_id, create_success, suspended)
VALUES ('bound-project', $1, true, false)
RETURNING id`, userID).Scan(&projectID); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE elitea_identity.token_project_binding SET project_id = $1 WHERE token_id = $2`,
			projectID, tokenID); err != nil {
			t.Fatalf("rebind token: %v", err)
		}

		for _, lifecycle := range []struct {
			name          string
			suspended     bool
			createSuccess bool
			wantActive    bool
		}{
			{name: "live", suspended: false, createSuccess: true, wantActive: true},
			{name: "suspended", suspended: true, createSuccess: true, wantActive: false},
			{name: "creation never completed", suspended: false, createSuccess: false, wantActive: false},
		} {
			t.Run(lifecycle.name, func(t *testing.T) {
				if _, err := pool.Exec(ctx, `
UPDATE centry.project SET suspended = $1, create_success = $2 WHERE id = $3`,
					lifecycle.suspended, lifecycle.createSuccess, projectID); err != nil {
					t.Fatalf("set project lifecycle: %v", err)
				}
				principal, err := queries.GetActivePATPrincipalByUUID(ctx, devBootstrapTokenUUID)
				if err != nil {
					t.Fatalf("PAT validation for a bound token: %v", err)
				}
				if principal.ProjectID == nil || *principal.ProjectID != projectID {
					t.Fatalf("project_id = %v, want %d", principal.ProjectID, projectID)
				}
				if principal.BoundProjectActive != lifecycle.wantActive {
					t.Fatalf("bound_project_active = %t, want %t: the edge reads this "+
						"column to refuse a token bound to an inactive project",
						principal.BoundProjectActive, lifecycle.wantActive)
				}
			})
		}
	})

	t.Run("the guarded foreign key resolved, so deleting a token cascades", func(t *testing.T) {
		// 0071 adds the foreign key only when to_regclass finds
		// public.auth_core__token. 001_initial.sql creates it, so on this path
		// the constraint MUST be there — and ON DELETE CASCADE is the whole
		// reason no application code deletes a binding when a token goes.
		if _, err := pool.Exec(ctx, `DELETE FROM auth_core__token WHERE id = $1`, tokenID); err != nil {
			t.Fatalf("delete token: %v", err)
		}
		var remaining int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM elitea_identity.token_project_binding WHERE token_id = $1`,
			tokenID).Scan(&remaining); err != nil {
			t.Fatalf("count bindings: %v", err)
		}
		if remaining != 0 {
			t.Errorf("%d binding rows survived the token they belong to — the guarded "+
				"foreign key did not resolve on the dev bootstrap path", remaining)
		}
	})

	t.Run("re-running the bootstrap is inert", func(t *testing.T) {
		// The dev flag runs on every start, and the exempt files land with no
		// ledger row, so RunMigrations applies them over an existing schema on
		// each boot. That must not error.
		if err := RunMigrations(ctx, pool); err != nil {
			t.Fatalf("second RunMigrations over an existing schema: %v", err)
		}
	})
}

// newDevBootstrapPool opens a throwaway database. RunMigrations must build
// everything else itself — nothing is created here by hand, which is the point.
func newDevBootstrapPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL dev-bootstrap integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_dev_bootstrap_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		if _, err := dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	return pool
}
