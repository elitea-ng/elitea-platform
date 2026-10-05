package migrations_test

// tenant/0143: the plaintext `webhook_secret` the agent editor's dead model
// setting wrote into `application_versions.llm_settings` (legacy issue 6656,
// review finding).
//
// The cases run the ledgered corpus, write rows the way a database that
// predates 0143 holds them, and then run the migration's own SQL again in the
// tenant schema. Re-running the real file is the point: a copy of the SQL here
// would measure the copy.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const webhookSecretMigration = "tenant/0143_strip_llm_settings_webhook_secret.sql"

func applyWebhookSecretMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(webhookSecretMigration)
	if err != nil {
		t.Fatalf("read %s: %v", webhookSecretMigration, err)
	}
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, "SET search_path TO p_1; "+string(body)); err != nil {
		t.Fatalf("apply %s: %v", webhookSecretMigration, err)
	}
}

func TestTheStoredWebhookSecretIsRemovedAndNothingElse(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()

	var applicationID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('agent', '', 1) RETURNING id`,
	).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	for name, settings := range map[string]string{
		"with-secret":    `{"temperature": 0.2, "webhook_secret": "reused-github-secret"}`,
		"without-secret": `{"temperature": 0.5}`,
		"array":          `["webhook_secret"]`,
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type, llm_settings)
VALUES ($1, $2, 'published', 1, 'openai', $3::jsonb)`, applicationID, name, settings); err != nil {
			t.Fatalf("seed version %s: %v", name, err)
		}
	}

	// Twice: the migration is idempotent.
	applyWebhookSecretMigration(t, pool)
	applyWebhookSecretMigration(t, pool)

	want := map[string]string{
		"with-secret":    `{"temperature": 0.2}`,
		"without-secret": `{"temperature": 0.5}`,
		// Not an object: left alone rather than reinterpreted.
		"array": `["webhook_secret"]`,
	}
	for name, settings := range want {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT llm_settings::text FROM p_1.application_versions WHERE name = $1`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != settings {
			t.Fatalf("%s: llm_settings = %s, want %s", name, got, settings)
		}
	}
}
