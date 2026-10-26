package repos

// The steering messages of a desktop local turn (client contract 1.26)
// against a REAL database (ELITEA_TEST_DATABASE_URL): the commit stores them
// as user messages directly after the question, once.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

func TestPostgresLocalTurnCommitStoresSteeringMessagesOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const user int64 = 4601

	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'local steering', $1, 'elitea', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, user).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	participants := map[string]int64{}
	for _, participant := range []struct{ entity, meta string }{
		{"user", `{"id": ` + strconv.FormatInt(user, 10) + `}`}, {"dummy", `{}`},
	} {
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json) RETURNING id`, participant.entity, participant.meta).Scan(&id); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at) VALUES ($1, $2, now() AT TIME ZONE 'UTC')`,
			conversationID, id); err != nil {
			t.Fatalf("map participant: %v", err)
		}
		participants[participant.entity] = id
	}

	recorder := &localTurnAudit{}
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), &localTurnPolicy{allowed: true}, NewMemoriesRepo(pool), recorder,
		func() (string, error) { return strings.Repeat("0", 31) + "7", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	desktop := localturn.Credential{TokenID: "77", NativeClientID: "ai.elitea.desktop"}
	started, err := service.Start(ctx, localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: desktop.TokenID, NativeClientID: desktop.NativeClientID,
		ConversationUUID: conversationUUID, QuestionID: "7a1c2d3e-4a5b-4c6d-8e7f-901234567890",
		UserInput: "Fix the build", AuditRoute: "/start",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	commit := localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, Credential: desktop, ExecutionID: started.ExecutionID,
		UserMessage:      "Fix the build",
		SteeringMessages: []string{"Skip the tests.", "Only the build."},
		AssistantMessage: "The build passes.",
		AuditRoute:       "/commit",
	}
	turn, err := service.Commit(ctx, commit)
	if err != nil || !turn.Created {
		t.Fatalf("commit = %+v, %v; want created", turn, err)
	}

	type row struct {
		uuid, text, origin, answer string
		author                     int64
		sameTime                   bool
	}
	read := func() []row {
		t.Helper()
		rows, err := pool.Query(ctx, `
SELECT message.uuid::text, COALESCE(text.content, ''), message.author_participant_id,
       COALESCE(message.meta -> 'steering' ->> 'origin', ''),
       COALESCE(message.meta -> 'steering' ->> 'response_message_id', ''),
       message.created_at = question.created_at
FROM p_1.chat_message_group AS message
JOIN p_1.chat_message_group AS question ON question.uuid = $2::uuid
LEFT JOIN p_1.chat_message_items AS item ON item.message_group_id = message.id
LEFT JOIN p_1.chat_messages_text AS text ON text.id = item.id
WHERE message.conversation_id = $1
ORDER BY message.created_at, message.id`, conversationID, turn.QuestionMessageID)
		if err != nil {
			t.Fatalf("read messages: %v", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.uuid, &r.text, &r.author, &r.origin, &r.answer, &r.sameTime); err != nil {
				t.Fatalf("scan message: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read messages: %v", err)
		}
		return out
	}
	assertTranscript := func() {
		t.Helper()
		got := read()
		if len(got) != 4 {
			t.Fatalf("messages = %+v, want question, two steering messages, answer", got)
		}
		if got[0].uuid != turn.QuestionMessageID || got[3].uuid != turn.ResponseMessageID {
			t.Fatalf("order = %+v, want the question first and the answer last", got)
		}
		for i, want := range []string{"Skip the tests.", "Only the build."} {
			steer := got[i+1]
			if steer.text != want || steer.origin != "local" || steer.answer != turn.ResponseMessageID ||
				steer.author != participants["user"] || !steer.sameTime {
				t.Fatalf("steering message %d = %+v, want %q by the user at the question's time", i, steer, want)
			}
		}
	}
	assertTranscript()

	// A retry of the same body writes nothing more.
	replayed, err := service.Commit(ctx, commit)
	if err != nil || replayed.Created {
		t.Fatalf("replayed commit = %+v, %v; want not created", replayed, err)
	}
	assertTranscript()
	// Other steering messages are another body.
	changed := commit
	changed.SteeringMessages = []string{"Skip the tests."}
	if _, err := service.Commit(ctx, changed); !errors.Is(err, localturn.ErrAlreadyCommitted) {
		t.Fatalf("commit with other steering messages = %v, want ErrAlreadyCommitted", err)
	}

	// The audit row names the steering messages by digest, never by text.
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var action string
	for _, event := range recorder.events {
		if event.HTTPRoute == "/commit" {
			action = event.Action
		}
	}
	if !strings.Contains(action, "2 steering messages (content sha256 ") || strings.Contains(action, "Skip the tests") {
		t.Fatalf("commit audit action = %q", action)
	}
}
