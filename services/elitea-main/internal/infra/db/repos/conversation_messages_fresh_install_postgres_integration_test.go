package repos

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// A FRESH INSTALL, not the corpus.
//
// Every other transcript test in this package reaches its database through
// db.RunMigrations or the dbtest template, and both of those build p_1 from the
// pylon-era schema PLUS the ledgered tenant history. That combination hides the
// defect these tests exist for: `migrations/tenant/0123` and `0127` create
// chat_messages_text and chat_messages_attachment, so a corpus-built tenant has
// them no matter what 001_initial.sql does.
//
// The stack the journeys E2E suite runs is NOT that stack. It applies
// 001_initial.sql with psql and then invokes `/elitea-migrate` with NO FLAGS
// (apps/elitea-web/scripts/e2e-stack.sh), which runs Bootstrap and ApplyShared
// only — the tenant history never executes there, and
// deploy/docker-compose.e2e-standalone.yml has no migrate service to run it
// either. So in that deployment p_1 is exactly and only what
// `create_tenant_schema` builds, and every table the tenant history would have
// added is absent.
//
// newFreshInstallPool reproduces that state precisely: a private empty
// database, then migrate.Bootstrap with the embedded pylon-era schema, which is
// the same call cmd/elitea-migrate makes on a first install. Nothing else runs.
func newFreshInstallPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	applied, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial)
	if err != nil {
		t.Fatalf("bootstrap the pylon-era schema: %v", err)
	}
	if !applied {
		t.Fatal("Bootstrap reported nothing to do on an empty database")
	}

	// Proven, not assumed: this must be the tenant a fresh install has, built
	// by 001_initial.sql's own `SELECT create_tenant_schema('p_1')`. If some
	// future change made the histories run here too, these tests would silently
	// stop discriminating and become a second copy of the corpus suite.
	var baselineTable, ledgerRows any
	if err := pool.QueryRow(ctx, `SELECT to_regclass('p_1.configuration')`).Scan(&baselineTable); err != nil {
		t.Fatalf("probe the baseline tenant schema: %v", err)
	}
	if baselineTable == nil {
		t.Fatal("p_1 was not built by create_tenant_schema; the fresh-install state is not what this test claims")
	}
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.schema_migrations')`).Scan(&ledgerRows); err != nil {
		t.Fatalf("probe the migration ledger: %v", err)
	}
	if ledgerRows != nil {
		var applied int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM elitea_runtime.schema_migrations`).Scan(&applied); err != nil {
			t.Fatalf("count ledger rows: %v", err)
		}
		if applied != 0 {
			t.Fatalf("the ledgered histories ran on this database (%d rows); this is not a bare fresh install", applied)
		}
	}
	return pool
}

// seedFreshInstallTranscript writes ONE conversation carrying ONE question, the
// way the runtime writer does it: a chat_conversations row, a `user`
// participant mapped into the conversation, a chat_message_group, an
// order-0 `text_message` chat_message_items row, and the chat_messages_text
// payload that hangs off it — the exact statement sequence
// internal/api/v2/conversations/predict.go:333-358 executes for a user turn.
//
// It returns the conversation's UUID, which is what the web client puts in the
// route (a uuid or a numeric id both resolve, see resolveConversationID).
func seedFreshInstallTranscript(t *testing.T, pool *pgxpool.Pool, content string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, is_private, author_id, created_at)
VALUES (gen_random_uuid(), 'fresh install transcript', TRUE, 1, now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	var participantID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
VALUES (gen_random_uuid(), 'user', '{"id": 1}'::jsonb)
RETURNING id`).Scan(&participantID); err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
VALUES ($1, $2, now() AT TIME ZONE 'UTC')`, conversationID, participantID); err != nil {
		t.Fatalf("map participant into conversation: %v", err)
	}

	var groupID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, reply_to_id, meta, is_streaming, created_at)
VALUES (gen_random_uuid(), $1, $2, NULL, NULL, '{}'::jsonb, false, now() AT TIME ZONE 'UTC')
RETURNING id`, participantID, conversationID).Scan(&groupID); err != nil {
		t.Fatalf("insert message group: %v", err)
	}

	var itemID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_items
    (uuid, item_type, order_index, meta, message_group_id, created_at)
VALUES (gen_random_uuid(), 'text_message', 0, '{}'::jsonb, $1, now() AT TIME ZONE 'UTC')
RETURNING id`, groupID).Scan(&itemID); err != nil {
		t.Fatalf("insert message item: %v", err)
	}

	// The statement the defect is about. On an unfixed baseline this is the
	// first thing that fails, with 42P01 on p_1.chat_messages_text, and it
	// fails for the WRITER as well as the reader — so a fresh-install stack
	// cannot even record a turn, let alone read one back.
	if _, err := pool.Exec(ctx,
		`INSERT INTO p_1.chat_messages_text (id, content) VALUES ($1, $2)`, itemID, content); err != nil {
		t.Fatalf("insert message text: %v", err)
	}
	return conversationUUID
}

// GET /elitea_core/messages/prompt_lib/{projectID}/{conversationID} on a tenant
// that only 001_initial.sql ever touched.
//
// ListMessages joins chat_messages_text for the group's content and then calls
// attachmentItemsByGroup, which joins chat_messages_attachment; both propagate
// their errors rather than answering an empty transcript (#599). Neither table
// was created by `create_tenant_schema`, so on the journeys stack this route
// answered 500 for EVERY conversation, and journey J11 only stayed green
// because the page survives the failed request.
func TestListMessagesAnswersOnAFreshInstallTenant(t *testing.T) {
	pool := newFreshInstallPool(t)
	conversationUUID := seedFreshInstallTranscript(t, pool, "what does a fresh install answer?")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	response, err := NewConversationsRepo(pool).ListMessages(ctx, "1", conversationUUID, conversations.MessagesQuery{})
	if err != nil {
		t.Fatalf("ListMessages on a fresh-install tenant: %v", err)
	}
	if response.Total != 1 || len(response.Items) != 1 {
		t.Fatalf("total=%d items=%d, want one of each", response.Total, len(response.Items))
	}
	if got := response.Items[0].Content; got != "what does a fresh install answer?" {
		t.Errorf("content=%q, want the seeded question", got)
	}
	if response.Items[0].Role != "user" {
		t.Errorf("role=%q, want user", response.Items[0].Role)
	}
}

// The other projection of the same transcript, on the same tenant. The details
// route reads through ListMessageGroups, which LEFT JOINs chat_messages_text
// and chat_messages_attachment in one statement — so a baseline missing either
// one fails the conversation-details read as well as the transcript read, and
// fixing only the table the first 42P01 named would leave this one broken.
func TestListMessageGroupsAnswersOnAFreshInstallTenant(t *testing.T) {
	pool := newFreshInstallPool(t)
	conversationUUID := seedFreshInstallTranscript(t, pool, "and what do the details say?")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	groups, err := NewConversationsRepo(pool).ListMessageGroups(ctx, "1", conversationUUID, 50, "asc")
	if err != nil {
		t.Fatalf("ListMessageGroups on a fresh-install tenant: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups=%d, want 1", len(groups))
	}
	items, _ := groups[0]["message_items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("message_items=%d, want the one text item that was seeded", len(items))
	}
}

// THE SHAPE, read from the corpus rather than restated here.
//
// The baseline and the tenant history now both create these four tables, and
// whichever runs first wins: `create_tenant_schema` on a fresh install, 0123
// and 0127 on a database the baseline never reached. Both are CREATE TABLE IF
// NOT EXISTS, so the loser changes nothing — which means a disagreement between
// them is not an error anywhere, it is two deployments quietly carrying
// different columns for the same table.
//
// So this compares them. It applies the corpus files 0123 and 0127 verbatim
// into a schema of their own, and asserts the tables they build are
// column-for-column what the baseline built in p_1. Transcribing the expected
// columns into this file instead would stop discriminating the day either
// definition changed.
func TestFreshInstallChatTablesMatchTheTenantCorpusShape(t *testing.T) {
	pool := newFreshInstallPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const corpusSchema = "corpus_shape"
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+corpusSchema); err != nil {
		t.Fatalf("create the comparison schema: %v", err)
	}
	// 0123 creates the whole chat graph itself and references nothing outside
	// it; 0127 is guarded on chat_message_items, which 0123 has just created.
	// Both are written unqualified, so a transaction-local search_path is what
	// puts them in the comparison schema — the same mechanism migrate.Runner
	// uses for a tenant.
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(ctx,
		"SELECT set_config('search_path', $1, true)", corpusSchema); err != nil {
		t.Fatalf("set the comparison search path: %v", err)
	}
	for _, path := range []string{
		"tenant/0123_agent_chat_message_tables.sql",
		"tenant/0127_chat_message_attachment_items.sql",
	} {
		body, err := platformmigrations.Files.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := transaction.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s into the comparison schema: %v", path, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("commit the comparison schema: %v", err)
	}

	for _, table := range []string{
		"chat_messages_text",
		"chat_messages_context",
		"chat_messages_attachment",
		"chat_message_trace_step",
	} {
		baseline := readColumns(ctx, t, pool, "p_1", table)
		corpus := readColumns(ctx, t, pool, corpusSchema, table)
		if len(baseline) == 0 {
			t.Errorf("p_1.%s does not exist; create_tenant_schema does not build it", table)
			continue
		}
		if len(corpus) == 0 {
			t.Errorf("%s.%s does not exist; the corpus files no longer create it", corpusSchema, table)
			continue
		}
		if len(baseline) != len(corpus) {
			t.Errorf("%s: baseline has %d columns, the corpus has %d\n baseline: %v\n corpus:   %v",
				table, len(baseline), len(corpus), baseline, corpus)
			continue
		}
		for i := range baseline {
			if baseline[i] != corpus[i] {
				t.Errorf("%s column %d: baseline %v, corpus %v", table, i+1, baseline[i], corpus[i])
			}
		}
	}
}

// columnShape is one column as both definitions must declare it. The default is
// reduced to a boolean because a serial's default names its own sequence, and
// the sequence carries the schema it was created in — a difference that says
// nothing about the shape.
type columnShape struct {
	Name       string
	Type       string
	MaxLength  int
	NotNull    bool
	HasDefault bool
}

func (c columnShape) String() string {
	return fmt.Sprintf("%s %s(%d) not_null=%t default=%t", c.Name, c.Type, c.MaxLength, c.NotNull, c.HasDefault)
}

func readColumns(ctx context.Context, t *testing.T, pool *pgxpool.Pool, schemaName, table string) []columnShape {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT column_name,
       data_type,
       COALESCE(character_maximum_length, 0),
       is_nullable = 'NO',
       column_default IS NOT NULL
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`, schemaName, table)
	if err != nil {
		t.Fatalf("read %s.%s columns: %v", schemaName, table, err)
	}
	defer rows.Close()

	shapes := []columnShape{}
	for rows.Next() {
		var shape columnShape
		if err := rows.Scan(&shape.Name, &shape.Type, &shape.MaxLength, &shape.NotNull, &shape.HasDefault); err != nil {
			t.Fatalf("scan %s.%s column: %v", schemaName, table, err)
		}
		shapes = append(shapes, shape)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s.%s columns: %v", schemaName, table, err)
	}
	return shapes
}
