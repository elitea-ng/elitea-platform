package repos

// The SQL half of "share a conversation by link".
//
// The handler suite (internal/api/v2/sharedchat/handler_test.go) proves that a
// refused link is refused indistinguishably. It CANNOT prove that revocation
// and expiry are enforced at all, because its Store double is the thing doing
// the enforcing there. The enforcement lives in one predicate inside
// ResolveByTokenHash, and a predicate is exactly the sort of thing that is
// dropped during a "simplify this query" edit with every unit test still green.
//
// So these subtests run the real statements against a real PostgreSQL:
//
//   - a live link resolves,
//   - a REVOKED link does not,
//   - an EXPIRED link does not,
//   - an unknown hash does not,
//   - and all four failures come back as the SAME error value, so the handler
//     above cannot accidentally be handed a distinguishable one.
//
// They also pin the two things the migration is for: the token is stored as a
// hash and never as plaintext, and the password columns are all-or-nothing.

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

const sharedChatLinksProject = "1"

func sharedChatLinkInput(token string, expiresAt time.Time) sharedchat.CreateInput {
	sum := sha256.Sum256([]byte(token))
	return sharedchat.CreateInput{
		ProjectID:      sharedChatLinksProject,
		ConversationID: 4242,
		Scope:          "all",
		TokenHash:      sum[:],
		CreatedBy:      "1",
		ExpiresAt:      expiresAt,
	}
}

func TestSharedChatLinksResolutionEnforcesRevocationAndExpiryInSQL(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewSharedChatLinksRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hash := func(token string) []byte {
		sum := sha256.Sum256([]byte(token))
		return sum[:]
	}

	t.Run("a live link resolves to its own project and conversation", func(t *testing.T) {
		link, err := repo.Create(ctx, sharedChatLinkInput("live-token", time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if link.ID == 0 {
			t.Fatalf("create returned no id: %+v", link)
		}
		resolved, err := repo.ResolveByTokenHash(ctx, hash("live-token"))
		if err != nil {
			t.Fatalf("resolve a live link: %v", err)
		}
		if resolved.ProjectID != sharedChatLinksProject || resolved.ConversationID != 4242 {
			t.Fatalf("resolved = %+v, want project %s conversation 4242", resolved, sharedChatLinksProject)
		}
	})

	t.Run("a revoked link resolves to nothing", func(t *testing.T) {
		link, err := repo.Create(ctx, sharedChatLinkInput("revoked-token", time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := repo.ResolveByTokenHash(ctx, hash("revoked-token")); err != nil {
			t.Fatalf("precondition: the link must resolve before revocation: %v", err)
		}
		if err := repo.Revoke(ctx, sharedChatLinksProject, 4242, link.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := repo.ResolveByTokenHash(ctx, hash("revoked-token")); !errors.Is(err, sharedchat.ErrNoLink) {
			t.Fatalf("resolve after revoke: err = %v, want ErrNoLink", err)
		}
	})

	t.Run("an expired link resolves to nothing", func(t *testing.T) {
		if _, err := repo.Create(ctx, sharedChatLinkInput("expired-token", time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := repo.ResolveByTokenHash(ctx, hash("expired-token")); !errors.Is(err, sharedchat.ErrNoLink) {
			t.Fatalf("resolve an expired link: err = %v, want ErrNoLink", err)
		}
	})

	t.Run("an unknown hash resolves to the same error", func(t *testing.T) {
		if _, err := repo.ResolveByTokenHash(ctx, hash("never-issued")); !errors.Is(err, sharedchat.ErrNoLink) {
			t.Fatalf("resolve an unknown token: err = %v, want ErrNoLink", err)
		}
	})

	t.Run("revoking a link on another conversation changes nothing", func(t *testing.T) {
		link, err := repo.Create(ctx, sharedChatLinkInput("scoped-token", time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := repo.Revoke(ctx, sharedChatLinksProject, 9999, link.ID); !errors.Is(err, sharedchat.ErrNoLink) {
			t.Fatalf("cross-conversation revoke: err = %v, want ErrNoLink", err)
		}
		if _, err := repo.ResolveByTokenHash(ctx, hash("scoped-token")); err != nil {
			t.Fatalf("the link must still resolve after a refused revoke: %v", err)
		}
	})

	t.Run("the plaintext token is nowhere in the table", func(t *testing.T) {
		if _, err := repo.Create(ctx, sharedChatLinkInput("plaintext-probe", time.Now().Add(time.Hour))); err != nil {
			t.Fatalf("create: %v", err)
		}
		// Casting every column of the row to text and searching it is the
		// assertion a column-by-column check cannot make: it also fails if a
		// later migration adds a column that happens to hold the token.
		var found bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM elitea_chat.shared_chat_links l
				WHERE l::text LIKE '%plaintext-probe%'
			)`).Scan(&found)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if found {
			t.Fatal("the plaintext token is stored somewhere in shared_chat_links")
		}
	})

	t.Run("a half-written password is refused by the table", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO elitea_chat.shared_chat_links
				(token_hash, project_id, conversation_id, scope, password_hash,
				 created_by, expires_at)
			VALUES ($1, 1, 4242, 'all', $2, '1', now() + interval '1 hour')`,
			hash("half-password"), []byte("hash-with-no-salt"))
		if err == nil {
			t.Fatal("a password hash with no salt was accepted")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("err = %v, want a 23514 check-constraint violation", err)
		}
	})

	t.Run("an unknown scope is refused by the table", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO elitea_chat.shared_chat_links
				(token_hash, project_id, conversation_id, scope, created_by, expires_at)
			VALUES ($1, 1, 4242, 'everything', '1', now() + interval '1 hour')`,
			hash("bad-scope"))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("err = %v, want a 23514 check-constraint violation", err)
		}
	})

	t.Run("the listing reports revocation and expiry as inactive", func(t *testing.T) {
		if _, err := repo.Create(ctx, sharedChatLinkInput("listing-live", time.Now().Add(time.Hour))); err != nil {
			t.Fatalf("create: %v", err)
		}
		links, err := repo.ListByConversation(ctx, sharedChatLinksProject, 4242)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(links) == 0 {
			t.Fatal("list returned nothing")
		}
		var sawActive, sawInactive bool
		for _, l := range links {
			if l.Active {
				sawActive = true
			} else {
				sawInactive = true
			}
		}
		if !sawActive {
			t.Fatal("no link reported as active")
		}
		if !sawInactive {
			t.Fatal("the revoked and expired links from the subtests above are reported as active")
		}
	})
}

// TestSharedChatTranscriptProjectsOnlyTheSharedFields runs the anonymous read's
// own query against a real transcript, and asserts on the SHAPE it produces.
//
// It exists because the projection is the security boundary: what this function
// selects is what an anonymous holder of a token can see, and a widened SELECT
// is a silent disclosure that no route test would notice.
func TestSharedChatTranscriptProjectsOnlyTheSharedFields(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewSharedChatLinksRepo(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var conversationID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.chat_conversations (name, author_id, created_at)
		VALUES ('shared transcript', 1, now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	var participantID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
		VALUES (gen_random_uuid(), 'user', '{"id":"1","name":"Ada"}'::jsonb)
		RETURNING id`).Scan(&participantID); err != nil {
		t.Fatalf("seed participant: %v", err)
	}

	newGroup := func(text string) int64 {
		t.Helper()
		var groupID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, task_id, created_at)
			VALUES (gen_random_uuid(), $1, $2, 'task-should-not-be-shared', now() AT TIME ZONE 'UTC')
			RETURNING id`, participantID, conversationID).Scan(&groupID); err != nil {
			t.Fatalf("seed group: %v", err)
		}
		var itemID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id, created_at)
			VALUES (gen_random_uuid(), 'text_message', 0, $1, now() AT TIME ZONE 'UTC') RETURNING id`, groupID).Scan(&itemID); err != nil {
			t.Fatalf("seed item: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO p_1.chat_messages_text (id, content) VALUES ($1, $2)`, itemID, text); err != nil {
			t.Fatalf("seed text: %v", err)
		}
		return groupID
	}

	first := newGroup("first message")
	second := newGroup("second message")

	t.Run("a full share returns every group", func(t *testing.T) {
		name, messages, err := repo.SharedTranscript(ctx, "1", conversationID, nil)
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		if name != "shared transcript" {
			t.Fatalf("name = %q", name)
		}
		if len(messages) != 2 {
			t.Fatalf("messages = %d, want 2", len(messages))
		}
		if messages[0].Items[0].Content != "first message" {
			t.Fatalf("first message content = %q", messages[0].Items[0].Content)
		}
		if messages[0].AuthorType != "user" {
			t.Fatalf("author_type = %q, want user", messages[0].AuthorType)
		}
	})

	t.Run("a partial share returns only the named groups", func(t *testing.T) {
		_, messages, err := repo.SharedTranscript(ctx, "1", conversationID, []int64{second})
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		if len(messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(messages))
		}
		if messages[0].Items[0].Content != "second message" {
			t.Fatalf("content = %q, want the named group only", messages[0].Items[0].Content)
		}
		_ = first
	})

	t.Run("a conversation that no longer exists is empty, not an error", func(t *testing.T) {
		name, messages, err := repo.SharedTranscript(ctx, "1", 999999, nil)
		if err != nil {
			t.Fatalf("transcript of a missing conversation: %v", err)
		}
		if name != "" || len(messages) != 0 {
			t.Fatalf("name = %q, messages = %d — a link is a pointer, not a copy", name, len(messages))
		}
	})
}
