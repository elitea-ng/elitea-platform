package repos

// ADR-0050 decision 7a and 7b (issue 1605): typed participants and message
// authors, addressees, visibility and cause (tenant/0162), as the
// migration's triggers stamp them and every read serves them.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"slices"
	"testing"
)

func TestChatParticipantKindsStampedOnInsertAndServedByEveryRead(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()

	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'kinds', 7, 'agent', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatal(err)
	}
	participant := func(entityName string) int {
		t.Helper()
		var id int
		if err := pool.QueryRow(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), $1, '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
), mapping AS (
    INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
    SELECT $2, participant.id, '{}'::jsonb, now() AT TIME ZONE 'UTC' FROM participant
)
SELECT id FROM participant`, entityName, conversationID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	human, agent, toolkit := participant("user"), participant("application"), participant("toolkit")
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_participant_mapping SET persona = '{"display_name":"Mira","role":"CI lead"}'
WHERE participant_id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	group := func(author, sentTo, replyTo int) (int, string) {
		t.Helper()
		var id int
		var uuid string
		if err := pool.QueryRow(ctx, `
WITH grp AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, sent_to_id, reply_to_id, created_at)
    VALUES (gen_random_uuid(), $1, $2, NULLIF($3, 0), NULLIF($4, 0), now() AT TIME ZONE 'UTC')
    RETURNING id, uuid
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id, created_at)
    SELECT gen_random_uuid(), 'text_message', 0, grp.id, now() AT TIME ZONE 'UTC' FROM grp
    RETURNING id
), text AS (
    INSERT INTO p_1.chat_messages_text (id, content) SELECT item.id, 'text' FROM item
)
SELECT id, uuid::text FROM grp`, author, conversationID, sentTo, replyTo).Scan(&id, &uuid); err != nil {
			t.Fatal(err)
		}
		return id, uuid
	}
	question, questionUUID := group(human, agent, 0)
	_, answerUUID := group(agent, 0, question)
	group(toolkit, 0, 0)

	type expected struct {
		kind, cause string
		addressedTo []int
	}
	want := map[string]expected{
		questionUUID: {"human", "human_message", []int{agent}},
		answerUUID:   {"agent", "human_message", []int{}},
	}

	list, err := repo.ListMessages(ctx, "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(list.Items))
	}
	for _, message := range list.Items {
		if message.Visibility != "all" || message.AddressedTo == nil || message.AuthorKind == nil {
			t.Fatalf("message %s: %+v", message.UUID, message)
		}
		expectation, ok := want[message.UUID]
		if !ok {
			// The toolkit-authored message speaks as an agent; nothing
			// started it that the trigger can name.
			if *message.AuthorKind != "agent" || message.Cause != nil {
				t.Fatalf("attachment-authored message: kind %v cause %v", *message.AuthorKind, message.Cause)
			}
			continue
		}
		if *message.AuthorKind != expectation.kind || message.Cause == nil || *message.Cause != expectation.cause ||
			!slices.Equal(message.AddressedTo, expectation.addressedTo) {
			t.Fatalf("message %s: kind %v cause %v addressed %v, want %+v",
				message.UUID, *message.AuthorKind, message.Cause, message.AddressedTo, expectation)
		}
	}

	single, err := repo.GetMessageByUUID(ctx, "1", questionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if single["author_kind"] != "human" || single["cause"] != "human_message" || single["visibility"] != "all" ||
		!slices.Equal(single["addressed_to"].([]int), []int{agent}) {
		t.Fatalf("GetMessageByUUID = %v", single)
	}
	groups, err := repo.ListMessageGroups(ctx, "1", conversationUUID, 10, "asc")
	if err != nil || len(groups) != 3 {
		t.Fatalf("ListMessageGroups = %v, %v", groups, err)
	}
	if groups[1]["author_kind"] != "agent" || groups[1]["visibility"] != "all" {
		t.Fatalf("ListMessageGroups answer = %v", groups[1])
	}

	participants, err := repo.ListParticipants(ctx, "1", conversationUUID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[int]string{}
	for _, p := range participants {
		if p.Kind != nil {
			kinds[p.ID] = *p.Kind
		}
		if p.ID == agent && (p.Persona["display_name"] != "Mira" || p.Persona["role"] != "CI lead") {
			t.Fatalf("agent persona = %v", p.Persona)
		}
		if p.ID != agent && p.Persona != nil {
			t.Fatalf("participant %d has a persona: %v", p.ID, p.Persona)
		}
	}
	if kinds[human] != "human" || kinds[agent] != "agent" {
		t.Fatalf("kinds = %v", kinds)
	}
	if _, ok := kinds[toolkit]; ok {
		t.Fatalf("an attachment has a kind: %v", kinds)
	}

	// A writer that names one addressee gets the sent_to_id of this release.
	var sentTo *int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, addressed_to, visibility, created_at)
VALUES (gen_random_uuid(), $1, $2, ARRAY[$3::integer], 'side', now() AT TIME ZONE 'UTC')
RETURNING sent_to_id`, agent, conversationID, human).Scan(&sentTo); err != nil {
		t.Fatal(err)
	}
	if sentTo == nil || *sentTo != human {
		t.Fatalf("sent_to_id = %v, want %d", sentTo, human)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, visibility, created_at)
VALUES (gen_random_uuid(), $1, $2, 'loud', now() AT TIME ZONE 'UTC')`, agent, conversationID); err == nil {
		t.Fatal("an unknown visibility was stored")
	}
}
