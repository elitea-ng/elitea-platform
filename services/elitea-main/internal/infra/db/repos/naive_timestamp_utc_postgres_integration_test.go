package repos

// The tenant chat tables keep `created_at`/`updated_at` as `timestamp`
// WITHOUT time zone, and their meaning is the UTC wall clock: pylon wrote them
// with server_default now() in UTC containers, and the web client appends `Z`
// to a naive timestamp before it parses one. Postgres converts a timestamptz
// (now(), clock_timestamp(), a DEFAULT now()) into such a column through the
// SESSION time zone, so a writer is right only if it says AT TIME ZONE 'UTC'.
//
// Every test connection in this module runs in a non-UTC session
// (internal/testpg). The tests here pin that, and pin that
// the writers and the conversation activity trigger (tenant/0150) store UTC
// whatever the session says.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
)

// naiveUTCSkew bounds how far a stored stamp may sit from the UTC clock read
// around the write. A wrong zone is off by whole hours.
const naiveUTCSkew = 5 * time.Minute

// The guard is only a guard while the session is not UTC: a testpg.DSN
// change that silently stopped applying would make every test in this
// package pass on a UTC CI server again.
func TestPostgresIntegrationSessionTimeZoneIsNotUTC(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var zone string
	var offsetSeconds float64
	if err := pool.QueryRow(ctx, `
SELECT current_setting('TimeZone'),
       extract(epoch FROM (now() AT TIME ZONE current_setting('TimeZone')) - (now() AT TIME ZONE 'UTC'))`).
		Scan(&zone, &offsetSeconds); err != nil {
		t.Fatal(err)
	}
	if zone != testpg.TimeZone() {
		t.Fatalf("session TimeZone = %q, want %q", zone, testpg.TimeZone())
	}
	if offsetSeconds == 0 && os.Getenv(testpg.TimeZoneEnv) == "" {
		t.Fatalf("session time zone %q is UTC-equivalent; the naive-timestamp guard needs an offset", zone)
	}
}

func TestPostgresNaiveChatTimestampsAreUTCWallClock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
	defer cancel()

	before := time.Now().UTC()
	conversation, err := NewConversationsRepo(pool).Create(ctx, chatFoldersCorpusProject,
		conversations.Conversation{Name: "naive-utc"})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	folder, err := NewFoldersRepo(pool).Create(ctx, chatFoldersCorpusProject, folders.Folder{Name: "naive-utc"})
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}
	// A message group bumps its conversation's updated_at (tenant/0145's
	// trigger, rewritten by tenant/0150).
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
SELECT gen_random_uuid(), m.participant_id, m.conversation_id, '{}'::jsonb, FALSE, now() AT TIME ZONE 'UTC'
FROM p_1.chat_participant_mapping m WHERE m.conversation_id = $1::integer LIMIT 1`, conversation.ID); err != nil {
		t.Fatalf("insert message group: %v", err)
	}
	after := time.Now().UTC()

	stamps := []struct {
		name  string
		query string
		id    string
	}{
		{"chat_conversations.created_at", `SELECT created_at FROM p_1.chat_conversations WHERE id = $1::integer`, conversation.ID},
		{"chat_conversations.updated_at", `SELECT updated_at FROM p_1.chat_conversations WHERE id = $1::integer`, conversation.ID},
		{"chat_participant_mapping.created_at", `SELECT min(created_at) FROM p_1.chat_participant_mapping WHERE conversation_id = $1::integer`, conversation.ID},
		{"chat_conversation_folders.created_at", `SELECT created_at FROM p_1.chat_conversation_folders WHERE id = $1::integer`, folder.ID},
	}
	for _, stamp := range stamps {
		name := stamp.name
		var stored *time.Time
		if err := pool.QueryRow(ctx, stamp.query, stamp.id).Scan(&stored); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if stored == nil {
			t.Fatalf("%s is NULL", name)
		}
		// pgx returns a naive timestamp as that wall clock in time.UTC.
		if stored.Before(before.Add(-naiveUTCSkew)) || stored.After(after.Add(naiveUTCSkew)) {
			t.Errorf("%s = %v, want the UTC wall clock between %v and %v (session %s)",
				name, *stored, before, after, testpg.TimeZone())
		}
	}
	if !conversation.CreatedAt.IsZero() &&
		(conversation.CreatedAt.Before(before.Add(-naiveUTCSkew)) || conversation.CreatedAt.After(after.Add(naiveUTCSkew))) {
		t.Errorf("Create returned created_at %v, want the UTC wall clock", conversation.CreatedAt)
	}
}
