package repos

// The wiki chat transcript, against a real tenant schema.
//
// These assertions cannot be made against a mock. What the feature turns on is
// whether TWO polls of the same invocation produce ONE answer, and the answer
// to that lives in the statements — an advisory lock plus a look-then-insert —
// not in the Go around them. A fake store that "already has this invocation"
// tests the fake.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/wikichat"
)

const wikiHistoryUser int64 = 4242

func newWikiHistoryRepo(t *testing.T) *DeepWikiHistoryRepo {
	t.Helper()
	repo, err := NewDeepWikiHistoryRepo(newMigratedPostgresIntegrationPool(t))
	if err != nil {
		t.Fatalf("build the wiki history repository: %v", err)
	}
	return repo
}

func wikiQuestion(chatKey, invocation, text string) wikichat.Question {
	return wikichat.Question{
		ProjectID:    postgresIntegrationTenant,
		UserID:       wikiHistoryUser,
		ChatKey:      chatKey,
		ToolkitID:    77,
		ToolkitName:  "Wikis",
		Capability:   "ask",
		Question:     text,
		InvocationID: invocation,
	}
}

func wikiAnswer(invocation, text string, isError bool) wikichat.Answer {
	return wikichat.Answer{
		ProjectID:    postgresIntegrationTenant,
		UserID:       wikiHistoryUser,
		InvocationID: invocation,
		Content:      text,
		IsError:      isError,
	}
}

// transcript reads one conversation's turns back in the order they were
// written, as `role: text`.
func transcript(t *testing.T, repo *DeepWikiHistoryRepo, chatKey string) []string {
	t.Helper()
	rows, err := repo.pool.Query(context.Background(), fmt.Sprintf(`
SELECT p.entity_name, mt.content
FROM p_%d.chat_conversations c
JOIN p_%d.chat_message_group mg ON mg.conversation_id = c.id
JOIN p_%d.chat_participants p ON p.id = mg.author_participant_id
JOIN p_%d.chat_message_items mi ON mi.message_group_id = mg.id
JOIN p_%d.chat_messages_text mt ON mt.id = mi.id
WHERE c.meta->>'wiki_chat_key' = $1
ORDER BY mg.id`,
		postgresIntegrationTenant, postgresIntegrationTenant, postgresIntegrationTenant,
		postgresIntegrationTenant, postgresIntegrationTenant), chatKey)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	defer rows.Close()

	var turns []string
	for rows.Next() {
		var author, content string
		if err := rows.Scan(&author, &content); err != nil {
			t.Fatalf("scan transcript: %v", err)
		}
		turns = append(turns, author+": "+content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return turns
}

func countConversations(t *testing.T, repo *DeepWikiHistoryRepo, chatKey string) int {
	t.Helper()
	var count int
	if err := repo.pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM p_%d.chat_conversations WHERE meta->>'wiki_chat_key' = $1`,
		postgresIntegrationTenant), chatKey).Scan(&count); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	return count
}

func TestRecordingATurnWritesTheQuestionAndThenTheAnswer(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()

	if err := repo.RecordQuestion(ctx, wikiQuestion("chat-a", "inv-1", "Where do the pages live?")); err != nil {
		t.Fatalf("record question: %v", err)
	}
	written, err := repo.RecordAnswer(ctx, wikiAnswer("inv-1", "In wiki_pages/.", false))
	if err != nil {
		t.Fatalf("record answer: %v", err)
	}
	if !written {
		t.Fatal("the first answer reported that it wrote nothing")
	}

	got := transcript(t, repo, "chat-a")
	want := []string{"user: Where do the pages live?", "toolkit: In wiki_pages/."}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("transcript = %v, want %v", got, want)
	}
}

// A conversation is opened once and then RESUMED. Two questions under one key
// must not become two conversations, or a reload would show half a chat.
func TestASecondQuestionResumesTheSameConversation(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()

	for index, invocation := range []string{"inv-1", "inv-2"} {
		if err := repo.RecordQuestion(ctx,
			wikiQuestion("chat-b", invocation, fmt.Sprintf("question %d", index))); err != nil {
			t.Fatalf("record question %s: %v", invocation, err)
		}
		if _, err := repo.RecordAnswer(ctx,
			wikiAnswer(invocation, fmt.Sprintf("answer %d", index), false)); err != nil {
			t.Fatalf("record answer %s: %v", invocation, err)
		}
	}

	if count := countConversations(t, repo, "chat-b"); count != 1 {
		t.Fatalf("two questions produced %d conversations, want 1", count)
	}
	if got := transcript(t, repo, "chat-b"); len(got) != 4 {
		t.Fatalf("transcript = %v, want four turns", got)
	}
}

// THE IDEMPOTENCY THE TEE DEPENDS ON. The browser polls on an interval, and
// nothing stops it draining the terminal payload more than once.
func TestRepeatedPollsWriteTheAnswerOnce(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()
	if err := repo.RecordQuestion(ctx, wikiQuestion("chat-c", "inv-1", "a question")); err != nil {
		t.Fatalf("record question: %v", err)
	}

	writes := 0
	for range 5 {
		written, err := repo.RecordAnswer(ctx, wikiAnswer("inv-1", "the answer", false))
		if err != nil {
			t.Fatalf("record answer: %v", err)
		}
		if written {
			writes++
		}
	}

	if writes != 1 {
		t.Fatalf("five polls reported %d writes, want 1", writes)
	}
	if got := transcript(t, repo, "chat-c"); len(got) != 2 {
		t.Fatalf("transcript = %v, want one question and one answer", got)
	}
}

// The same, CONCURRENTLY. A plain `WHERE NOT EXISTS` passes the test above and
// fails this one: two transactions in flight at once both find nothing.
func TestConcurrentPollsWriteTheAnswerOnce(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()
	if err := repo.RecordQuestion(ctx, wikiQuestion("chat-d", "inv-1", "a question")); err != nil {
		t.Fatalf("record question: %v", err)
	}

	const pollers = 8
	var waiting sync.WaitGroup
	results := make(chan bool, pollers)
	failures := make(chan error, pollers)
	for range pollers {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			written, err := repo.RecordAnswer(ctx, wikiAnswer("inv-1", "the answer", false))
			if err != nil {
				failures <- err
				return
			}
			results <- written
		}()
	}
	waiting.Wait()
	close(results)
	close(failures)

	for err := range failures {
		t.Fatalf("record answer: %v", err)
	}
	writes := 0
	for written := range results {
		if written {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("%d concurrent polls reported %d writes, want 1", pollers, writes)
	}
	if got := transcript(t, repo, "chat-d"); len(got) != 2 {
		t.Fatalf("transcript = %v, want one question and one answer", got)
	}
}

// Two questions asked at the same instant under a key that has no
// conversation yet must open ONE. Without the advisory lock both miss the
// lookup and the drawer ends up with two half-transcripts under one key.
func TestConcurrentFirstQuestionsOpenOneConversation(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()

	const askers = 6
	var waiting sync.WaitGroup
	failures := make(chan error, askers)
	for index := range askers {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			if err := repo.RecordQuestion(ctx, wikiQuestion(
				"chat-e", fmt.Sprintf("inv-%d", index), fmt.Sprintf("question %d", index))); err != nil {
				failures <- err
			}
		}()
	}
	waiting.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("record question: %v", err)
	}

	if count := countConversations(t, repo, "chat-e"); count != 1 {
		t.Fatalf("%d simultaneous first questions produced %d conversations, want 1", askers, count)
	}
}

// A wiki conversation must be HIDDEN and typed, or it surfaces in the user's
// ordinary chat list — and it must carry the `single_participant` shape the
// conversation listing filters on, or the drawer can never find it again.
func TestANewConversationIsHiddenTypedAndFilterable(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	if err := repo.RecordQuestion(context.Background(),
		wikiQuestion("chat-f", "inv-1", "a question")); err != nil {
		t.Fatalf("record question: %v", err)
	}

	var hidden, source, entityName, entityID, name string
	if err := repo.pool.QueryRow(context.Background(), fmt.Sprintf(`
SELECT c.meta->>'is_hidden', c.source,
       c.meta->'single_participant'->>'entity_name',
       c.meta->'single_participant'->'entity_meta'->>'id',
       c.name
FROM p_%d.chat_conversations c WHERE c.meta->>'wiki_chat_key' = $1`,
		postgresIntegrationTenant), "chat-f").
		Scan(&hidden, &source, &entityName, &entityID, &name); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if hidden != "true" {
		t.Errorf("is_hidden = %q, want true — the chat list would show it", hidden)
	}
	if source != DeepWikiSource {
		t.Errorf("source = %q, want %q", source, DeepWikiSource)
	}
	if entityName != DeepWikiParticipantEntity || entityID != "77" {
		t.Errorf("single_participant = %q/%q, want %q/77", entityName, entityID, DeepWikiParticipantEntity)
	}
	if name != "a question" {
		t.Errorf("name = %q, want the question that opened it", name)
	}
}

// An answer belongs to the caller who asked. Another user polling the same
// invocation writes nothing rather than replying into somebody else's chat.
func TestAnAnswerIsNotWrittenIntoAnotherCallersConversation(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()
	if err := repo.RecordQuestion(ctx, wikiQuestion("chat-g", "inv-1", "a question")); err != nil {
		t.Fatalf("record question: %v", err)
	}

	stranger := wikiAnswer("inv-1", "not yours", false)
	stranger.UserID = wikiHistoryUser + 1
	written, err := repo.RecordAnswer(ctx, stranger)
	if err != nil {
		t.Fatalf("record answer: %v", err)
	}
	if written {
		t.Fatal("another caller's poll wrote into this conversation")
	}
	if got := transcript(t, repo, "chat-g"); len(got) != 1 {
		t.Fatalf("transcript = %v, want the question alone", got)
	}
}

// A poll for an invocation this platform never recorded — an older
// conversation, or a request that carried no conversation key — is the
// ORDINARY case, not an error.
func TestAnAnswerWithNoRecordedQuestionIsNotAnError(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	written, err := repo.RecordAnswer(context.Background(),
		wikiAnswer("inv-unknown", "an answer to nothing", false))
	if err != nil {
		t.Fatalf("record answer: %v", err)
	}
	if written {
		t.Fatal("an answer was written for a question that was never recorded")
	}
}

// A failed run is recorded as a turn, marked. Dropping it would leave a
// question with no reply — indistinguishable from the tab-closed gap.
func TestAFailedRunIsRecordedAsAnErrorTurn(t *testing.T) {
	repo := newWikiHistoryRepo(t)
	ctx := context.Background()
	if err := repo.RecordQuestion(ctx, wikiQuestion("chat-h", "inv-1", "a question")); err != nil {
		t.Fatalf("record question: %v", err)
	}
	if _, err := repo.RecordAnswer(ctx, wikiAnswer("inv-1", "the clone failed", true)); err != nil {
		t.Fatalf("record answer: %v", err)
	}

	var isError bool
	if err := repo.pool.QueryRow(context.Background(), fmt.Sprintf(`
SELECT (mg.meta->>'is_error')::boolean
FROM p_%d.chat_conversations c
JOIN p_%d.chat_message_group mg ON mg.conversation_id = c.id
WHERE c.meta->>'wiki_chat_key' = $1 AND mg.reply_to_id IS NOT NULL`,
		postgresIntegrationTenant, postgresIntegrationTenant), "chat-h").Scan(&isError); err != nil {
		t.Fatalf("read answer meta: %v", err)
	}
	if !isError {
		t.Fatal("a failed run was recorded as an ordinary answer")
	}
}
