package migrations_test

// shared/0249 renames the proposal source hr_agent to workforce_manager. 0236
// and 0237 are released and immutable, so the rename is a NEW file that has to
// do two things a fresh install never exercises: rewrite the rows an upgraded
// database already holds, and replace the CHECK constraints that name the old
// value. A fresh corpus passes either way, so the upgrade path is replayed here:
// restore the 0237 constraints, store an hr_agent row, apply the file.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationCorpusRenamesTheHRAgentProposalSource(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const insert = `INSERT INTO elitea_lifecycle.agent_workforce_proposals
		(project_id, application_id, kind, stage, source, proposer_application_id, execution_id,
		 rationale, evidence_digest)
		VALUES (1, $1, 'dismiss', 'watch', $2, 900, 'exec-1', 'slow', repeat('a', 64))`

	t.Run("the fresh corpus admits workforce_manager and refuses hr_agent", func(t *testing.T) {
		if _, err := pool.Exec(ctx, insert, 101, "workforce_manager"); err != nil {
			t.Fatalf("the corpus refuses a workforce_manager proposal: %v", err)
		}
		_, err := pool.Exec(ctx, insert, 102, "hr_agent")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("want a 23514 check violation for the retired value, got %v", err)
		}
	})

	t.Run("an upgraded database has its hr_agent rows rewritten", func(t *testing.T) {
		for _, stmt := range []string{
			`ALTER TABLE elitea_lifecycle.agent_workforce_proposals DROP CONSTRAINT agent_workforce_proposals_source_known`,
			`ALTER TABLE elitea_lifecycle.agent_workforce_proposals DROP CONSTRAINT agent_workforce_proposals_proposer_v2`,
			`UPDATE elitea_lifecycle.agent_workforce_proposals SET source = 'hr_agent' WHERE application_id = 101`,
			`ALTER TABLE elitea_lifecycle.agent_workforce_proposals ADD CONSTRAINT agent_workforce_proposals_source_known
				CHECK (source IN ('person', 'hr_agent', 'policy'))`,
		} {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("restore the 0237 shape: %v", err)
			}
		}

		body, err := platformmigrations.Files.ReadFile("shared/0249_agent_workforce_manager_source.sql")
		if err != nil {
			t.Fatalf("read the migration: %v", err)
		}
		// Twice: the file must be safe to apply again.
		for range 2 {
			if _, err := pool.Exec(ctx, string(body)); err != nil {
				t.Fatalf("apply 0249 over the 0237 shape: %v", err)
			}
		}

		var source string
		if err := pool.QueryRow(ctx, `SELECT source FROM elitea_lifecycle.agent_workforce_proposals
			WHERE application_id = 101`).Scan(&source); err != nil || source != "workforce_manager" {
			t.Fatalf("the old row reads %q (%v), want workforce_manager", source, err)
		}
		var definition string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conname = 'agent_workforce_proposals_proposer_v2'
			  AND conrelid = 'elitea_lifecycle.agent_workforce_proposals'::regclass`).Scan(&definition); err != nil {
			t.Fatalf("read the proposer constraint: %v", err)
		}
		if strings.Contains(definition, "hr_agent") || !strings.Contains(definition, "workforce_manager") {
			t.Fatalf("the proposer constraint still names the old value: %s", definition)
		}
		if _, err := pool.Exec(ctx, insert, 103, "hr_agent"); err == nil {
			t.Fatal("hr_agent is still accepted after the rename")
		}
	})
}
