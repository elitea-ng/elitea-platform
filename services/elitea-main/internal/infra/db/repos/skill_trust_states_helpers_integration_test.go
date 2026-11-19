package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// applySkillTrustMigration adds tenant 0250 (skill_trust) to the baseline-migrated
// p_1, which is where this pool's skills tables come from.
func applySkillTrustMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "migrations", "tenant", "0250_skill_trust.sql"))
	if err != nil {
		t.Fatalf("read the skill trust migration: %v", err)
	}
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET search_path TO p_1`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET search_path`) }()
	if _, err := conn.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("apply the skill trust migration: %v", err)
	}
}
