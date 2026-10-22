package repos

// ConversationsRepo.Delete removed the whole message graph and reported
// nothing about the files that graph carried, so the handler had nothing to
// clean up and the bytes outlived the conversation until the retention sweeper
// expired them.
//
// This is the conversation-level half of what DeleteMessage already does: the
// refs are read INSIDE the transaction, because chat_messages_attachment
// cascades from chat_message_items (tenant migration 0127) and the item delete
// below takes those rows with it. After the commit nothing in the database
// names the stored files at all.
//
// WHAT THE RED RUN SHOWS. Against the unchanged previous code these tests do
// not compile — `Delete` returned `error` alone — and once they do, the
// attachment assertions fail with an empty slice: the delete removed the rows
// that named the files and reported no ref for any of them.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"sort"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// attachmentKeys renders reported refs as "bucket/name" for comparison,
// sorted so the assertion does not depend on row order.
func attachmentKeys(refs []conversations.AttachmentRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Bucket+"/"+ref.Name)
	}
	sort.Strings(out)
	return out
}

// Deleting a conversation reports every attachment its messages carried, so
// the handler can remove the bytes once the delete is a fact.
func TestDeleteConversationReportsTheAttachmentsItRemoved(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID, groupUUIDs := seedConversationWithParticipant(t, repo, "question", "answer")
	seedAttachmentItem(t, repo, groupUUIDs[0], "chat-attachments", "conv-uuid/question.png")
	seedAttachmentItem(t, repo, groupUUIDs[1], "legacy-attachments", "conv-uuid/report.pdf")

	refs, err := repo.Delete(ctx, "1", conversationUUID)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}

	got := attachmentKeys(refs)
	want := []string{"chat-attachments/conv-uuid/question.png", "legacy-attachments/conv-uuid/report.pdf"}
	if len(got) != len(want) {
		t.Fatalf("reported attachments %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reported attachments %v, want %v", got, want)
		}
	}

	// And the whole graph is really gone, attachment rows included — nothing
	// left in the database names those files, which is why reporting them was
	// the only way the bytes could ever be reached.
	convRows, groups, items, texts, mappings := countConversationRows(t, repo)
	if convRows != 0 || groups != 0 || items != 0 || texts != 0 || mappings != 0 {
		t.Fatalf("after deleting the conversation: %d conversations, %d groups, %d items, %d texts, %d mappings — want all zero",
			convRows, groups, items, texts, mappings)
	}
	var attachments int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_messages_attachment`).Scan(&attachments); err != nil {
		t.Fatalf("count attachment rows: %v", err)
	}
	if attachments != 0 {
		t.Fatalf("%d attachment rows survived, want 0", attachments)
	}
}

// The numeric-id form of the identifier reaches the same rows and reports the
// same refs — #599's resolver runs before the collection, not after it.
func TestDeleteConversationReportsAttachmentsForTheNumericID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	numericID, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	seedAttachmentItem(t, repo, groupUUIDs[0], "chat-attachments", "conv-uuid/question.png")

	refs, err := repo.Delete(context.Background(), "1", numericID)
	if err != nil {
		t.Fatalf("delete conversation by numeric id: %v", err)
	}
	if got := attachmentKeys(refs); len(got) != 1 || got[0] != "chat-attachments/conv-uuid/question.png" {
		t.Fatalf("reported attachments %v, want the one seeded attachment", got)
	}
}

// Only THIS conversation's attachments. The tenant schema holds every
// conversation in the project, so a collection that forgot to scope by
// conversation would hand the handler someone else's files to destroy.
func TestDeleteConversationReportsOnlyItsOwnAttachments(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	_, doomedUUID, doomedGroups := seedConversationWithParticipant(t, repo, "question")
	_, _, bystanderGroups := seedConversationWithParticipant(t, repo, "question")
	seedAttachmentItem(t, repo, doomedGroups[0], "chat-attachments", "conv-uuid/mine.pdf")
	seedAttachmentItem(t, repo, bystanderGroups[0], "chat-attachments", "other-conv/theirs.pdf")

	refs, err := repo.Delete(context.Background(), "1", doomedUUID)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if got := attachmentKeys(refs); len(got) != 1 || got[0] != "chat-attachments/conv-uuid/mine.pdf" {
		t.Fatalf("reported attachments %v, want only this conversation's", got)
	}

	var attachments int
	if err := repo.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.chat_messages_attachment`).Scan(&attachments); err != nil {
		t.Fatalf("count attachment rows: %v", err)
	}
	if attachments != 1 {
		t.Fatalf("%d attachment rows left, want the bystander conversation's one", attachments)
	}
}

// A conversation with no attachments reports none — nothing for the handler to
// clean up, not a nil-vs-empty distinction it has to care about.
func TestDeleteConversationReportsNoAttachmentsWhenThereAreNone(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	_, conversationUUID, _ := seedConversationWithParticipant(t, repo, "question", "answer")

	refs, err := repo.Delete(context.Background(), "1", conversationUUID)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("reported %v attachments for a conversation that had none", refs)
	}
}

// A REFUSED delete reports nothing, so the handler destroys nothing. The legacy
// runtime deletes the bytes before its guards run; here the collection is
// inside the transaction and the refs only reach the caller once the commit
// succeeded.
func TestDeleteConversationReportsNoAttachmentsWhenItRefuses(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	seedAttachmentItem(t, repo, groupUUIDs[0], "chat-attachments", "conv-uuid/question.png")

	for _, identifier := range []string{
		"2147483000",
		"e0ac9d1e-06e4-4d3f-9e39-1f3a1c7f6d55",
		"not-an-identifier",
	} {
		refs, err := repo.Delete(context.Background(), "1", identifier)
		if err == nil {
			t.Errorf("deleting conversation %q succeeded, want an error", identifier)
		}
		if len(refs) != 0 {
			t.Errorf("a refused delete of %q reported %v attachments to destroy", identifier, refs)
		}
	}

	var attachments int
	if err := repo.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.chat_messages_attachment`).Scan(&attachments); err != nil {
		t.Fatalf("count attachment rows: %v", err)
	}
	if attachments != 1 {
		t.Fatalf("%d attachment rows left after three refused deletes, want 1", attachments)
	}
}
