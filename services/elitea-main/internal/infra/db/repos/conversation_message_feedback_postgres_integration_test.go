package repos

// Message like/dislike feedback (#880) — persistence, upsert, and the
// conversation export carrying it.
//
// AGAINST A REAL, MIGRATED DATABASE: the question this file answers is
// whether tenant/0135_chat_message_feedback.sql's UNIQUE(message_group_uuid,
// user_id) constraint really makes SetMessageFeedback's ON CONFLICT upsert
// replace a row rather than reject or duplicate it — a fact a mocked
// repository could not prove either way.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// countFeedbackRows reports how many chat_message_feedback rows exist for one
// message — the discriminator between "upserted" and "duplicated".
func countFeedbackRows(t *testing.T, repo *ConversationsRepo, messageUUID string) int {
	t.Helper()
	var count int
	if err := repo.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.chat_message_feedback WHERE message_group_uuid = $1::uuid`, messageUUID,
	).Scan(&count); err != nil {
		t.Fatalf("count feedback rows: %v", err)
	}
	return count
}

const (
	feedbackUserA = "101"
	feedbackUserB = "102"
)

// A like persists and reads back with the caller's own vote highlighted.
func TestSetMessageFeedback_PersistsAndReadsBack(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question", "answer")
	messageUUID := groupUUIDs[1]

	summary, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserA, 1, "very helpful")
	if err != nil {
		t.Fatalf("set feedback: %v", err)
	}
	if summary.Likes != 1 || summary.Dislikes != 0 {
		t.Errorf("summary is %+v, want 1 like and 0 dislikes", summary)
	}
	if summary.Mine == nil || summary.Mine.Rating != 1 || summary.Mine.Comment != "very helpful" {
		t.Errorf("mine is %+v, want rating=1 comment=%q", summary.Mine, "very helpful")
	}

	// A SEPARATE read answers the same thing — proving the row is really
	// stored, not just echoed back from the write's own response.
	readBack, err := repo.GetMessageFeedback(ctx, "1", messageUUID, feedbackUserA)
	if err != nil {
		t.Fatalf("get feedback: %v", err)
	}
	if readBack.Likes != 1 || readBack.Mine == nil || readBack.Mine.Rating != 1 {
		t.Errorf("read-back summary is %+v, want the same like this test just set", readBack)
	}
	if countFeedbackRows(t, repo, messageUUID) != 1 {
		t.Fatalf("expected exactly 1 stored row after one vote")
	}
}

// The UNIQUE(message_group_uuid, user_id) constraint is what makes a second
// vote from the SAME user REPLACE the first rather than add a second row —
// the defect this table's own migration header explains social_feedbacks has
// (no such constraint, so a repeat CreateFeedback call there duplicates).
func TestSetMessageFeedback_SecondVoteFromSameUserUpsertsNotDuplicates(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	messageUUID := groupUUIDs[0]

	if _, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserA, 1, "first take"); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	// Changed their mind: like -> dislike, with a different comment.
	summary, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserA, -1, "actually no")
	if err != nil {
		t.Fatalf("second vote: %v", err)
	}

	if summary.Likes != 0 || summary.Dislikes != 1 {
		t.Errorf("summary after the changed vote is %+v, want 0 likes and 1 dislike", summary)
	}
	if summary.Mine == nil || summary.Mine.Rating != -1 || summary.Mine.Comment != "actually no" {
		t.Errorf("mine after the changed vote is %+v, want rating=-1 comment=%q", summary.Mine, "actually no")
	}
	if got := countFeedbackRows(t, repo, messageUUID); got != 1 {
		t.Fatalf("stored %d rows for one user's two votes, want exactly 1 (upsert, not duplicate)", got)
	}
}

// Two different users' votes both count in the aggregate, and each sees only
// their OWN vote as "mine" — not the other's.
func TestSetMessageFeedback_TwoUsersAggregateIndependently(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	messageUUID := groupUUIDs[0]

	if _, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserA, 1, ""); err != nil {
		t.Fatalf("user A vote: %v", err)
	}
	if _, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserB, -1, ""); err != nil {
		t.Fatalf("user B vote: %v", err)
	}

	summary, err := repo.GetMessageFeedback(ctx, "1", messageUUID, feedbackUserA)
	if err != nil {
		t.Fatalf("get feedback as user A: %v", err)
	}
	if summary.Likes != 1 || summary.Dislikes != 1 {
		t.Errorf("aggregate is %+v, want 1 like and 1 dislike from the two users", summary)
	}
	if summary.Mine == nil || summary.Mine.Rating != 1 {
		t.Errorf("user A's mine is %+v, want rating=1 (their own vote, not user B's)", summary.Mine)
	}

	asUserB, err := repo.GetMessageFeedback(ctx, "1", messageUUID, feedbackUserB)
	if err != nil {
		t.Fatalf("get feedback as user B: %v", err)
	}
	if asUserB.Mine == nil || asUserB.Mine.Rating != -1 {
		t.Errorf("user B's mine is %+v, want rating=-1", asUserB.Mine)
	}
	if countFeedbackRows(t, repo, messageUUID) != 2 {
		t.Fatalf("expected 2 stored rows for 2 distinct users")
	}
}

// Retracting removes only the CALLER's own row — a sibling's vote on the same
// message is untouched.
func TestDeleteMessageFeedback_RetractsOnlyCallersOwn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	messageUUID := groupUUIDs[0]

	if _, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserA, 1, ""); err != nil {
		t.Fatalf("user A vote: %v", err)
	}
	if _, err := repo.SetMessageFeedback(ctx, "1", messageUUID, feedbackUserB, 1, ""); err != nil {
		t.Fatalf("user B vote: %v", err)
	}

	summary, err := repo.DeleteMessageFeedback(ctx, "1", messageUUID, feedbackUserA)
	if err != nil {
		t.Fatalf("delete user A's feedback: %v", err)
	}
	if summary.Likes != 1 {
		t.Errorf("summary after retracting user A's like is %+v, want 1 like (user B's) remaining", summary)
	}
	if summary.Mine != nil {
		t.Errorf("mine after retracting is %+v, want nil (this caller has no vote left)", summary.Mine)
	}

	// Retracting again is a no-op, not an error — DELETE's usual idempotent
	// shape.
	if _, err := repo.DeleteMessageFeedback(ctx, "1", messageUUID, feedbackUserA); err != nil {
		t.Fatalf("retracting a second time: %v", err)
	}
	if countFeedbackRows(t, repo, messageUUID) != 1 {
		t.Fatalf("expected user B's row to survive user A's retraction")
	}
}

// A message this project never had is a 404, on all three verbs — not a
// silent zero.
func TestMessageFeedback_UnknownMessageIsNotFound(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	seedConversationWithParticipant(t, repo, "question") // some other message exists
	unknown := "e0ac9d1e-06e4-4d3f-9e39-1f3a1c7f6d55"

	if _, err := repo.GetMessageFeedback(ctx, "1", unknown, feedbackUserA); err == nil {
		t.Error("GetMessageFeedback on an unknown message succeeded, want an error")
	}
	if _, err := repo.SetMessageFeedback(ctx, "1", unknown, feedbackUserA, 1, ""); err == nil {
		t.Error("SetMessageFeedback on an unknown message succeeded, want an error")
	}
	// Deleting is idempotent even for an unknown MESSAGE — but this is an
	// unknown message uuid, and a delete guarded only by uuid.Parse would not
	// notice; deliberately NOT asserted here (DeleteMessageFeedback has no
	// existence check by design — see its own doc comment on idempotency).
}

// Cross-project isolation: a message-group uuid that exists in ONE project's
// schema must not be readable as feedback through a DIFFERENT project's
// schema — the existence check queries p_{project_id}.chat_message_group,
// not any schema.
func TestMessageFeedback_ScopedToItsOwnProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question")
	messageUUID := groupUUIDs[0]

	if _, err := repo.GetMessageFeedback(ctx, "999", messageUUID, feedbackUserA); err == nil {
		t.Error("reading feedback for project 1's message through project 999 succeeded, want a schema/not-found error")
	}
}

// The conversation export (#851/export.go) carries each message's feedback
// aggregate — this is the one assertion that goes through the HTTP handler
// rather than the repository directly, because "the export carries it" is a
// statement about the ROUTE, not about SetMessageFeedback alone.
func TestConversationExport_CarriesMessageFeedback(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	router := newConversationExportRouter(t, pool)
	ctx := context.Background()
	conversationNumericID, _, groupUUIDs := seedConversationWithParticipant(t, repo, "question", "answer")
	ratedMessage := groupUUIDs[1]

	if _, err := repo.SetMessageFeedback(ctx, "1", ratedMessage, feedbackUserA, 1, "great answer"); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	if _, err := repo.SetMessageFeedback(ctx, "1", ratedMessage, feedbackUserB, -1, ""); err != nil {
		t.Fatalf("seed second feedback: %v", err)
	}

	response := callExportAs(t, router, "/conversation_export/prompt_lib/1/"+conversationNumericID+"?format=json", "7")
	if response.Code != http.StatusOK {
		t.Fatalf("export answered %d: %s", response.Code, response.Body.String())
	}

	var document struct {
		Messages []struct {
			UUID     string `json:"uid"`
			Content  string `json:"content"`
			Feedback *struct {
				Likes    int `json:"likes"`
				Dislikes int `json:"dislikes"`
			} `json:"feedback,omitempty"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode export: %v\n%s", err, response.Body.String())
	}

	var found bool
	for _, message := range document.Messages {
		if message.UUID != ratedMessage {
			// The unrated message must carry NO feedback field at all —
			// "absence means unrated", not a zeroed-out object.
			if message.Feedback != nil {
				t.Errorf("unrated message %q carries a feedback field: %+v", message.UUID, message.Feedback)
			}
			continue
		}
		found = true
		if message.Feedback == nil {
			t.Fatalf("rated message %q carries no feedback field in the export", ratedMessage)
		}
		if message.Feedback.Likes != 1 || message.Feedback.Dislikes != 1 {
			t.Errorf("exported feedback is %+v, want 1 like and 1 dislike", message.Feedback)
		}
	}
	if !found {
		t.Fatalf("the rated message %q was not in the export at all", ratedMessage)
	}
}

// Compile-time assertion that ConversationsRepo really implements the
// feedback methods conversations.Repository declares — the same shape
// conversation_delete_postgres_integration_test.go's file establishes for
// DeleteMessage, kept here so a signature drift fails at build time rather
// than only inside cmd/elitea-main's composition root.
var _ conversations.Repository = (*ConversationsRepo)(nil)
