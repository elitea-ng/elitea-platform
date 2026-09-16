package repos

// The first test in this package that can execute a folder or a
// selected-conversation statement at all.
//
// # What was untestable, and why
//
// newMigratedPostgresIntegrationPool builds its template from
// `dbtest.Spec{Files: platformmigrations.Files, Seed: postgresIntegrationSeedSQL}`
// — the LEDGERED migration corpus and a hand-written legacy seed, never
// internal/infra/db/migrations/001_initial.sql. Until tenant/0126 the corpus
// declared `chat_conversations` (tenant/0123:53-66) as a strict subset of the
// table every deployment actually runs, and created neither
// `chat_conversation_folders` nor `chat_selected_conversations` at all. So in
// this package `folder_id` did not exist, and every one of FoldersRepo's five
// methods, ConversationsRepo.Get/Update, and Select/DeselectConversation raised
// 42P01 or 42703 against the only migrated pool the package has.
//
// Nothing else could see it. internal/api/v2/folders/handler_test.go uses a
// mock repository and no pool. internal/api/production_router_test.go and
// tests/deployedge/edge_root_routes_test.go assert route surface against a
// zero-value `&pgxpool.Pool{}`. router_elitea_core_project_scope_test.go checks
// permission gates with an empty embedded Repository. Not one of them runs a
// statement. And index_activity_postgres_integration_test.go:513-521 hand-creates
// `folder_id` for p_2 with a comment calling it a deployed-legacy extra "which
// no query in this repository reads" — four call sites read it.
//
// # Why these assertions
//
// The subtests below execute the repository's OWN statements rather than
// comparing information_schema to a transcription of the migration. A shape
// assertion would pass against a column of the wrong type or an FK with the
// wrong delete action; these fail the way the route fails.
//
// Two of them are specifically about clauses that are absent on purpose, which
// is the part of the shape a future "obvious tidy-up" is most likely to break:
//
//   - `folder_id`'s FK carries NO ON DELETE clause, because pylon nulls each
//     member conversation's folder_id in application code before deleting the
//     folder (legacy/plugins/elitea_core/api/v2/folder.py:846-865). So deleting
//     a folder that still holds a conversation must be REFUSED. Write
//     ON DELETE CASCADE instead and "folder delete does not take the
//     conversation with it" fails — with the conversation gone, which is the
//     data loss this pins.
//   - `chat_selected_conversations` has NO unique on `user_id`, which is why
//     SelectConversation is a DELETE-then-INSERT rather than an upsert. Add the
//     unique that looks like it belongs and "reselecting replaces" still passes,
//     so that subtest additionally selects the same conversation twice and
//     counts rows.
//
// Revert tenant/0126 and every subtest here fails.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// chatFoldersCorpusProject is the tenant the shared template migrates
// (postgresIntegrationTenant), so its schema is p_1.
const chatFoldersCorpusProject = "1"

func TestCorpusOnlySchemaSupportsFoldersAndSelectedConversations(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)

	foldersRepo := NewFoldersRepo(pool)
	conversationsRepo := NewConversationsRepo(pool)

	t.Run("the corpus alone creates the folder objects", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		// FoldersRepo.List swallows its query error and answers an empty slice
		// (folders.go:36-38), so it cannot tell "no folders" from "no table".
		// Create is the discriminating call: it returns the 42P01.
		created, err := foldersRepo.Create(ctx, chatFoldersCorpusProject, folders.Folder{Name: "corpus-folder"})
		if err != nil {
			t.Fatalf("create folder on a corpus-only database: %v", err)
		}
		if created.ID == "" {
			t.Fatalf("create folder returned no id: %+v", created)
		}
		if created.Position == nil || *created.Position <= 0 {
			t.Fatalf("create folder must place the row at the top of the sidebar, got position %v", created.Position)
		}

		listed, err := foldersRepo.List(ctx, chatFoldersCorpusProject)
		if err != nil {
			t.Fatalf("list folders: %v", err)
		}
		var found bool
		for _, f := range listed {
			if f.ID == created.ID {
				found = true
				if f.Name != "corpus-folder" {
					t.Errorf("listed folder name = %q, want %q", f.Name, "corpus-folder")
				}
			}
		}
		if !found {
			t.Fatalf("folder %s missing from List(%d folders)", created.ID, len(listed))
		}

		if err := foldersRepo.Delete(ctx, chatFoldersCorpusProject, created.ID); err != nil {
			t.Fatalf("delete folder: %v", err)
		}
	})

	t.Run("a conversation carries folder_id through Update and Get", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		folder, err := foldersRepo.Create(ctx, chatFoldersCorpusProject, folders.Folder{Name: "assignment-target"})
		if err != nil {
			t.Fatalf("create folder: %v", err)
		}
		conv, err := conversationsRepo.Create(ctx, chatFoldersCorpusProject, conversations.Conversation{Name: "corpus-conversation"})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}

		// Get reads `c.folder_id` (conversations.go:147); before 0126 this was
		// 42703, i.e. a 500 on GET /conversation/prompt_lib/{p}/{id}.
		fetched, err := conversationsRepo.Get(ctx, chatFoldersCorpusProject, conv.ID)
		if err != nil {
			t.Fatalf("get conversation: %v", err)
		}
		if fetched.FolderID != nil {
			t.Errorf("a new conversation must be unfiled, got folder_id %q", *fetched.FolderID)
		}

		// Update writes and returns it (conversations.go:256,276).
		updated, err := conversationsRepo.Update(ctx, chatFoldersCorpusProject, conv.ID, conversations.Conversation{FolderID: &folder.ID})
		if err != nil {
			t.Fatalf("assign conversation to folder: %v", err)
		}
		if updated.FolderID == nil || *updated.FolderID != folder.ID {
			t.Fatalf("update answered folder_id %v, want %q", updated.FolderID, folder.ID)
		}
		refetched, err := conversationsRepo.Get(ctx, chatFoldersCorpusProject, conv.ID)
		if err != nil {
			t.Fatalf("re-get conversation: %v", err)
		}
		if refetched.FolderID == nil || *refetched.FolderID != folder.ID {
			t.Fatalf("get answered folder_id %v, want %q", refetched.FolderID, folder.ID)
		}

		// The FK has no ON DELETE, so this must be refused rather than take the
		// conversation (or silently orphan it) with the folder.
		err = foldersRepo.Delete(ctx, chatFoldersCorpusProject, folder.ID)
		if err == nil {
			t.Fatalf("deleting a folder that still holds a conversation must be refused")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("folder delete error = %v, want foreign_key_violation (23503)", err)
		}
		if _, err := conversationsRepo.Get(ctx, chatFoldersCorpusProject, conv.ID); err != nil {
			t.Fatalf("the conversation must survive the refused folder delete: %v", err)
		}

		// What pylon does instead: unfile, then delete.
		empty := ""
		if _, err := conversationsRepo.Update(ctx, chatFoldersCorpusProject, conv.ID, conversations.Conversation{FolderID: &empty}); err != nil {
			t.Fatalf("unfile conversation: %v", err)
		}
		if err := foldersRepo.Delete(ctx, chatFoldersCorpusProject, folder.ID); err != nil {
			t.Fatalf("delete folder after unfiling: %v", err)
		}
		if _, err := conversationsRepo.Get(ctx, chatFoldersCorpusProject, conv.ID); err != nil {
			t.Fatalf("the conversation must survive its folder's deletion: %v", err)
		}
	})

	t.Run("selecting a conversation round-trips and reselecting replaces", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		first, err := conversationsRepo.Create(ctx, chatFoldersCorpusProject, conversations.Conversation{Name: "selected-one"})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}
		second, err := conversationsRepo.Create(ctx, chatFoldersCorpusProject, conversations.Conversation{Name: "selected-two"})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}

		const userID = "4242"
		if err := conversationsRepo.SelectConversation(ctx, chatFoldersCorpusProject, first.ID, userID); err != nil {
			t.Fatalf("select conversation on a corpus-only database: %v", err)
		}
		// Repeating the same selection is the DELETE-then-INSERT's own path;
		// with a unique on user_id it would still work, so the row count below
		// is what actually pins the absent constraint.
		if err := conversationsRepo.SelectConversation(ctx, chatFoldersCorpusProject, second.ID, userID); err != nil {
			t.Fatalf("reselect conversation: %v", err)
		}

		var rows int
		var selected int64
		if err := pool.QueryRow(ctx, `
SELECT count(*), max(conversation_id) FROM p_1.chat_selected_conversations WHERE user_id = $1`, userID).
			Scan(&rows, &selected); err != nil {
			t.Fatalf("read selected conversations: %v", err)
		}
		if rows != 1 {
			t.Fatalf("user %s holds %d selections, want exactly 1", userID, rows)
		}
		if got := strconv.FormatInt(selected, 10); got != second.ID {
			t.Fatalf("selected conversation = %s, want %s", got, second.ID)
		}

		if err := conversationsRepo.DeselectConversation(ctx, chatFoldersCorpusProject, userID); err != nil {
			t.Fatalf("deselect conversation: %v", err)
		}
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.chat_selected_conversations WHERE user_id = $1`, userID).Scan(&rows); err != nil {
			t.Fatalf("re-read selected conversations: %v", err)
		}
		if rows != 0 {
			t.Fatalf("deselect left %d rows", rows)
		}

		// Deleting the conversation must take its selection with it: that FK is
		// the one place ON DELETE CASCADE is correct (models/all.py:218-223).
		if err := conversationsRepo.SelectConversation(ctx, chatFoldersCorpusProject, first.ID, userID); err != nil {
			t.Fatalf("reselect before cascade check: %v", err)
		}
		if _, err := conversationsRepo.Delete(ctx, chatFoldersCorpusProject, first.ID); err != nil {
			t.Fatalf("delete conversation: %v", err)
		}
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.chat_selected_conversations WHERE user_id = $1`, userID).Scan(&rows); err != nil {
			t.Fatalf("read selected conversations after cascade: %v", err)
		}
		if rows != 0 {
			t.Fatalf("deleting the conversation left %d selection rows behind", rows)
		}
	})

	t.Run("attachment_participant_id exists and references chat_participants", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		conv, err := conversationsRepo.Create(ctx, chatFoldersCorpusProject, conversations.Conversation{Name: "attachment-owner"})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}

		var participantID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'attachment-store', '{}'::jsonb, '{}'::json)
RETURNING id`).Scan(&participantID); err != nil {
			t.Fatalf("create participant: %v", err)
		}

		if _, err := pool.Exec(ctx, `
UPDATE p_1.chat_conversations SET attachment_participant_id = $1 WHERE id::text = $2`,
			participantID, conv.ID); err != nil {
			t.Fatalf("set attachment_participant_id: %v", err)
		}

		// The FK must be real: an id no participant carries has to be refused.
		_, err = pool.Exec(ctx, `
UPDATE p_1.chat_conversations SET attachment_participant_id = $1 WHERE id::text = $2`,
			participantID+9_000_000, conv.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("attachment_participant_id accepted a dangling participant: err = %v", err)
		}
	})
}
