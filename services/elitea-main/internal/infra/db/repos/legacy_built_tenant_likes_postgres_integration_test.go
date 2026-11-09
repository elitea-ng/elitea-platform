package repos

// The application list over a project schema the legacy runtime built and the
// production upgrade path then migrated.
//
// Start: p_1 has no social_likes table (internal/infra/db/dbtest/legacyshape).
// The database is upgraded as Main starts up: Bootstrap, shared history, tenant
// history for project 1 (tenant migration 0164 creates the table).
//
// Asserts: the "liked by me" filter and the like-count sort both run, and the
// filter returns the one application the actor liked.

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest/legacyshape"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

var legacyBuiltTenantDBSeq atomic.Uint64

func newUpgradedLegacyBuiltPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the legacy-built tenant tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	name := fmt.Sprintf("elitea_legacytenant_repos_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), legacyBuiltTenantDBSeq.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	if _, err := pool.Exec(ctx, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the legacy-era schema: %v", err)
	}
	if _, err := pool.Exec(ctx, legacyshape.BuiltSQL); err != nil {
		t.Fatalf("build the legacy-built shape: %v", err)
	}
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("ApplyTenant(1): %v", err)
	}
	return pool
}

func TestAnUpgradedLegacyBuiltProjectListsApplicationsLikedByTheActor(t *testing.T) {
	pool := newUpgradedLegacyBuiltPool(t)
	repo := NewApplicationsRepo(pool)
	ctx := testContext(t)
	// The Auth service owns the user table, so the legacy-built fixture has
	// none; the list joins it for author names. A minimal one stands in.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE public.auth_core__user (id integer PRIMARY KEY, email text, name text);
		INSERT INTO public.auth_core__user VALUES (1, 'first@example.com', 'First')`); err != nil {
		t.Fatalf("create the Auth user table stand-in: %v", err)
	}
	liked := createTestApplication(t, repo, "Liked", 1, &applications.Version{Name: "base", AgentType: "openai"})
	createTestApplication(t, repo, "Not liked", 1, &applications.Version{Name: "base", AgentType: "openai"})
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.social_likes (entity_name, entity_id, user_id, created_at)
		VALUES ('application', $1, 1, now() AT TIME ZONE 'UTC')`, liked.ID); err != nil {
		t.Fatalf("insert a like into the upgraded schema: %v", err)
	}

	mine, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", MyLiked: true, SortBy: "name", SortOrder: "asc"})
	if err != nil {
		t.Fatalf("list with the liked-by-me filter: %v", err)
	}
	if mine.Total != 1 || len(mine.Rows) != 1 || mine.Rows[0].ID != liked.ID {
		t.Fatalf("liked-by-me list = total %d, rows %+v; want only %q", mine.Total, mine.Rows, "Liked")
	}

	byLikes, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", SortBy: "likes", SortOrder: "desc"})
	if err != nil {
		t.Fatalf("list sorted by like count: %v", err)
	}
	if byLikes.Total != 2 || byLikes.Rows[0].ID != liked.ID {
		t.Fatalf("like-count sort = total %d, first %q; want the liked application first", byLikes.Total, byLikes.Rows[0].ID)
	}
}
