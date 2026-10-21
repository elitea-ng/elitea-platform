package repos

// Issue 1062: the per-file attachment delete refuses a file a message
// carries. AttachmentReferenced is that check, against the real schema.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

func TestAttachmentReferencedSeesAnyMessageThatNamesTheObject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	sent := conversationUUID + "/sent.pdf"
	seedAttachmentPayloadItem(t, repo, groupID, 0, sent, "chat-attachments", "document", "")
	ctx := context.Background()

	for _, tc := range []struct {
		bucket, name string
		want         bool
	}{
		{"chat-attachments", sent, true},
		{"chat-attachments", conversationUUID + "/unsent.pdf", false},
		{"other-bucket", sent, false},
	} {
		got, err := repo.AttachmentReferenced(ctx, "1", tc.bucket, tc.name)
		if err != nil {
			t.Fatalf("AttachmentReferenced(%s, %s): %v", tc.bucket, tc.name, err)
		}
		if got != tc.want {
			t.Errorf("AttachmentReferenced(%s, %s) = %v, want %v", tc.bucket, tc.name, got, tc.want)
		}
	}
}
