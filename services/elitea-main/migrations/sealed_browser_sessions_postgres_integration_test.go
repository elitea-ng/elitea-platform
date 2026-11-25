package migrations_test

// Shared 0272 (issue 2521, ADR-0059 R2.7a): the provider CHECK of
// elitea_auth.browser_sessions gains `sealed`. It applies with the corpus and
// on the legacy RBAC matrix fixture, twice, with no change the second time and
// the rows kept; on a database without the table it does nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const sealedBrowserSessionsMigration = "shared/0272_sealed_browser_sessions.sql"

func TestSealedBrowserSessionsAppliesTwiceWithNoChange(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"bootstrap and corpus": newMigratedPool,
		"legacy matrix": func(t *testing.T) *pgxpool.Pool {
			pool := newEmptyDatabase(t)
			applyBootstrapSchema(t, pool)
			loadLegacyRBACMatrix(t, pool)
			applySharedCorpus(t, pool)
			return pool
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := tableShape(t, pool, "browser_sessions", "")
			if !strings.Contains(before, "'sealed'::text") {
				t.Fatalf("0272 did not widen the provider CHECK:\n%s", before)
			}
			insert := func(id, provider string) error {
				_, err := pool.Exec(context.Background(), `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, expires_at, idle_timeout_seconds)
VALUES ($1, 7, $2, now() + interval '1 hour', 0)`, id, provider)
				return err
			}
			for _, provider := range []string{"oidc", "saml", "form", "sealed"} {
				if err := insert(strings.Repeat(provider, 10), provider); err != nil {
					t.Fatalf("a %s session is refused: %v", provider, err)
				}
			}
			if err := insert(strings.Repeat("x", 40), "bogus"); err == nil ||
				!strings.Contains(err.Error(), "browser_sessions_provider_known") {
				t.Fatalf("an unknown provider is admitted: %v", err)
			}

			execMigrationFile(t, pool, sealedBrowserSessionsMigration)

			if after := tableShape(t, pool, "browser_sessions", ""); after != before {
				t.Fatalf("re-applying 0272 changed the table:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			var rows int
			queryRow(t, pool, `SELECT count(*) FROM elitea_auth.browser_sessions`, &rows)
			if rows != 4 {
				t.Fatalf("re-applying 0272 left %d rows, want 4", rows)
			}
		})
	}
	t.Run("no browser_sessions", func(t *testing.T) {
		pool := newEmptyDatabase(t)
		execMigrationFile(t, pool, sealedBrowserSessionsMigration)
		var present bool
		queryRow(t, pool, `SELECT to_regclass('elitea_auth.browser_sessions') IS NOT NULL`, &present)
		if present {
			t.Fatal("0272 created elitea_auth.browser_sessions")
		}
	})
}
