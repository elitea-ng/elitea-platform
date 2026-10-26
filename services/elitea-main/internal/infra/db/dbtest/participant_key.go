package dbtest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// ParticipantKeyMigration is the tenant migration that makes
// chat_participant_mapping.conversation_id cascade (#1429).
const ParticipantKeyMigration = "tenant/0157_chat_participant_mapping_conversation_cascade.sql"

// UseShippedParticipantKey gives schema's chat_participant_mapping →
// chat_conversations key the shape the ledgered migrations produce: the key
// as tenant 0123 declared it (no ON DELETE action), then
// [ParticipantKeyMigration] applied to it under the schema's search_path, as
// the migration runner does.
//
// Tests that bootstrap from 001_initial.sql need it: that file has always
// declared the key ON DELETE CASCADE, so a test built on it passed against a
// key no deployment had until 0157 (#1429). Running the real 0157 over the
// 0123 key keeps such a test on what deployments run.
func UseShippedParticipantKey(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	migration, err := platformmigrations.Files.ReadFile(ParticipantKeyMigration)
	if err != nil {
		return fmt.Errorf("read %s: %w", ParticipantKeyMigration, err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
ALTER TABLE %[1]s.chat_participant_mapping
    DROP CONSTRAINT IF EXISTS chat_participant_mapping_conversation_id_fkey,
    ADD CONSTRAINT chat_participant_mapping_conversation_id_fkey
        FOREIGN KEY (conversation_id) REFERENCES %[1]s.chat_conversations(id)`, quoted)); err != nil {
		return fmt.Errorf("declare the 0123 participant key: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", quoted); err != nil {
		return fmt.Errorf("set search path: %w", err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		return fmt.Errorf("apply %s: %w", ParticipantKeyMigration, err)
	}
	return tx.Commit(ctx)
}
