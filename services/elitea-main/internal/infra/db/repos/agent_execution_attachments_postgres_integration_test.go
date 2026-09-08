package repos

// #606 part 2: an admitted turn's uploaded files become `attachment_message`
// items on its QUESTION group, in the same transaction that creates the group.
//
// These exercise insertCurrentApplicationTurn / insertCurrentAdhocTurn — the
// repository functions the admission transaction calls — against real
// Postgres, because the properties under test are all properties of what
// landed in the two tables: the discriminator, the order_index sequence
// relative to the question's text item, and the 1:1 join
// ConversationsRepo.ListMessageGroups reads back.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresAttachmentItem struct {
	OrderIndex     int32
	ItemType       string
	Name           string
	Bucket         string
	AttachmentType string
	Content        []byte
}

func TestPostgresCurrentApplicationTurnWritesAttachmentItemsOnTheQuestionGroup(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000031",
		QuestionItemID:    "30000000-0000-4000-8000-000000000031",
		ResponseMessageID: "40000000-0000-4000-8000-000000000031",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "look at these",
		Attachments: currentAgentAttachmentFixtures(t, "20000000-0000-4000-8000-000000000031"),
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-attach-1", turn, 1); err != nil {
		t.Fatal(err)
	}

	items := readCurrentAgentQuestionItems(t, tx, turn.QuestionID)
	// order_index 0 is the question's own text item. Pylon enumerates the
	// attachments from 1 for exactly that reason (rpc/chat_all.py:303); a
	// second item at 0 would make the transcript's ordering ambiguous.
	if len(items) != 4 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].OrderIndex != 0 || items[0].ItemType != "text_message" {
		t.Fatalf("question text item moved: %+v", items[0])
	}
	want := []postgresAttachmentItem{
		{
			OrderIndex: 1, ItemType: "attachment_message",
			Name:   "10000000-0000-4000-8000-000000000031/report.pdf",
			Bucket: "chat-attachments", AttachmentType: "document",
		},
		{
			OrderIndex: 2, ItemType: "attachment_message",
			Name:   "10000000-0000-4000-8000-000000000031/shot.png",
			Bucket: "chat-attachments", AttachmentType: "image",
		},
		{
			OrderIndex: 3, ItemType: "attachment_message",
			Name:   "10000000-0000-4000-8000-000000000031/diagram.svg",
			Bucket: "chat-attachments", AttachmentType: "document",
		},
	}
	for index, expected := range want {
		got := items[index+1]
		if got.OrderIndex != expected.OrderIndex || got.ItemType != expected.ItemType ||
			got.Name != expected.Name || got.Bucket != expected.Bucket ||
			got.AttachmentType != expected.AttachmentType {
			t.Fatalf("item %d = %+v want %+v", index+1, got, expected)
		}
		// The payload row exists and carries the creation-time scaffold, not
		// an empty array: an empty array would claim content was computed.
		var chunks []map[string]any
		if err := json.Unmarshal(got.Content, &chunks); err != nil || len(chunks) != 1 {
			t.Fatalf("item %d content=%s err=%v", index+1, got.Content, err)
		}
		text, _ := chunks[0]["text"].(string)
		if !strings.Contains(text, "filepath: /"+expected.Bucket+"/"+expected.Name) {
			t.Fatalf("item %d scaffold does not name its filepath: %q", index+1, text)
		}
	}
}

func TestPostgresCurrentApplicationTurnWithoutAttachmentsWritesOnlyItsTextItem(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000031",
		QuestionItemID:    "30000000-0000-4000-8000-000000000031",
		ResponseMessageID: "40000000-0000-4000-8000-000000000031",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "no files here",
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-attach-2", turn, 1); err != nil {
		t.Fatal(err)
	}

	items := readCurrentAgentQuestionItems(t, tx, turn.QuestionID)
	if len(items) != 1 || items[0].ItemType != "text_message" {
		t.Fatalf("items=%+v", items)
	}
	var count int
	if err := tx.QueryRow(
		t.Context(),
		`SELECT count(*) FROM p_1.chat_messages_attachment`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("attachment payload rows=%d", count)
	}
}

func TestPostgresCurrentAdhocTurnWritesAttachmentItemsOnTheQuestionGroup(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentAdhocTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 23,
		ConversationUUID:  "10000000-0000-4000-8000-000000000032",
		QuestionID:        "20000000-0000-4000-8000-000000000032",
		QuestionItemID:    "30000000-0000-4000-8000-000000000032",
		ResponseMessageID: "40000000-0000-4000-8000-000000000032",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "look at these",
		Attachments: currentAgentAttachmentFixtures(t, "20000000-0000-4000-8000-000000000032"),
	}
	if err := insertCurrentAdhocTurn(t.Context(), queries, "execution-attach-3", turn); err != nil {
		t.Fatal(err)
	}

	items := readCurrentAgentQuestionItems(t, tx, turn.QuestionID)
	if len(items) != 4 || items[0].OrderIndex != 0 ||
		items[1].OrderIndex != 1 || items[1].ItemType != "attachment_message" ||
		items[1].AttachmentType != "document" ||
		items[2].OrderIndex != 2 || items[2].AttachmentType != "image" ||
		items[3].OrderIndex != 3 || items[3].AttachmentType != "document" {
		t.Fatalf("items=%+v", items)
	}
}

// currentAgentAttachmentFixtures builds what the application layer would hand
// the repository for three uploads: a document, an image and the .svg that is
// an image by MIME but a document by pylon's rule.
func currentAgentAttachmentFixtures(
	t *testing.T,
	questionID string,
) []agentexecutionapp.CurrentTurnAttachment {
	t.Helper()
	refs := []agentexecutionapp.CurrentTurnAttachmentRef{}
	for _, filepath := range []string{
		"/chat-attachments/10000000-0000-4000-8000-000000000031/report.pdf",
		"/chat-attachments/10000000-0000-4000-8000-000000000031/shot.png",
		"/chat-attachments/10000000-0000-4000-8000-000000000031/diagram.svg",
	} {
		ref, ok := agentexecutionapp.ParseAttachmentFilepath(filepath)
		if !ok {
			t.Fatalf("fixture filepath %q did not parse", filepath)
		}
		refs = append(refs, ref)
	}
	attachments := make([]agentexecutionapp.CurrentTurnAttachment, 0, len(refs))
	for index, ref := range refs {
		attachments = append(attachments, agentexecutionapp.CurrentTurnAttachment{
			ItemID:         currentAgentAttachmentItemIDs[index] + questionID[len(questionID)-2:],
			Name:           ref.Name,
			Bucket:         ref.Bucket,
			AttachmentType: agentexecutionapp.AttachmentKind(ref.Name),
			Content: json.RawMessage(
				`[{"type":"text","text":"Bucket: ` + ref.Bucket +
					`\nFilename: ` + ref.Name +
					`\nfilepath: /` + ref.Bucket + `/` + ref.Name + `"}]`,
			),
		})
	}
	return attachments
}

// Item ids only have to be valid, distinct uuids that differ per turn; the
// production derivation lives in the application package and is pinned by its
// own test.
var currentAgentAttachmentItemIDs = []string{
	"50000000-0000-4000-8000-0000000000",
	"60000000-0000-4000-8000-0000000000",
	"70000000-0000-4000-8000-0000000000",
}

func beginCurrentAgentAttachmentTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	return tx
}

// readCurrentAgentQuestionItems reads the question group's items through the
// SAME left join ConversationsRepo.ListMessageGroups uses, so a missing
// payload row shows up as empty attachment columns rather than as no row.
func readCurrentAgentQuestionItems(
	t *testing.T,
	tx pgx.Tx,
	questionID string,
) []postgresAttachmentItem {
	t.Helper()
	rows, err := tx.Query(t.Context(), `
		SELECT mi.order_index, mi.item_type,
		       COALESCE(ma.name, ''), COALESCE(ma.bucket, ''),
		       COALESCE(ma.attachment_type, ''), COALESCE(ma.content::text, '')
		FROM p_1.chat_message_items mi
		JOIN p_1.chat_message_group mg ON mg.id = mi.message_group_id
		LEFT JOIN p_1.chat_messages_attachment ma ON ma.id = mi.id
		WHERE mg.uuid = $1::uuid
		ORDER BY mi.order_index`, questionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var items []postgresAttachmentItem
	for rows.Next() {
		var item postgresAttachmentItem
		var content string
		if err := rows.Scan(
			&item.OrderIndex, &item.ItemType, &item.Name,
			&item.Bucket, &item.AttachmentType, &content,
		); err != nil {
			t.Fatal(err)
		}
		item.Content = []byte(content)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return items
}

// A conversation KEEPS WORKING after a turn that carried a file.
//
// This is the regression #606 part 2 would otherwise have introduced. Four
// gates — both resolvers and both inserters, agent_chat.sql — refused any
// conversation whose history contained an `attachment_message` item, because
// until now nothing in this service could produce one and the type meant "a
// pylon-era conversation outside the parity slice". The moment admission began
// writing those items the gate read its own output: turn 1 carrying the file
// was admitted, turn 2 was refused as unsupported, and attaching a file ended
// the conversation.
//
// Before #606 an attachment simply did nothing — no item was written and the
// conversation carried on — so refusing turn 2 would have been strictly WORSE
// than the behaviour being replaced. The gates now allow `attachment_message`
// and still refuse `canvas_message` and `context_message`; see the comment on
// ResolveCurrentApplicationTurn's gate for why those two are different.
//
// Restore `attachment_message` to any of the four gates and this fails with
// pgx.ErrNoRows. Note the first turn's streaming response must be completed
// first, or the pending-response gate refuses turn 2 for an unrelated reason
// and this test passes whether or not the item type is gated.
func TestPostgresATurnAfterAnAttachmentIsStillAdmitted(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000031",
		QuestionItemID:    "30000000-0000-4000-8000-000000000031",
		ResponseMessageID: "40000000-0000-4000-8000-000000000031",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "look at this",
		Attachments: currentAgentAttachmentFixtures(t, "20000000-0000-4000-8000-000000000031"),
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-attach-4", first, 1); err != nil {
		t.Fatal(err)
	}
	completePostgresCurrentApplicationTurn(
		t, tx, mustCurrentPGUUID(t, first.ResponseMessageID), "done",
	)

	if _, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000041"),
			ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
			ProjectID:        1,
		},
	); err != nil {
		t.Fatalf("a conversation that carried an attachment refuses its next turn: %v", err)
	}
}

// Regeneration and reset must survive a turn that carried a file.
//
// This is the SECOND regression #606 introduced by writing attachment items,
// and it is the same shape as the admission-gate one: `ResolveCurrentRegeneration`
// and `ResetCurrentAgentResponse` both refuse a question group holding any item
// whose type is outside an allow-list. `attachment_message` was outside it only
// because nothing could produce one, so once admission started writing them
// every turn that carried a file became unregeneratable — the gate refusing
// rows the admission it follows had just created.
//
// The RESPONSE-side gate in both queries stays strict on purpose: nothing here
// writes attachment items onto a response group. Pylon does, for agent-produced
// files (events/message_stream.py:107-120), and that is genuinely unported.
//
// Remove `attachment_message` from either question allow-list and this fails
// with pgx.ErrNoRows.
func TestPostgresRegenerationSurvivesAnAttachmentTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000031",
		QuestionItemID:    "30000000-0000-4000-8000-000000000031",
		ResponseMessageID: "40000000-0000-4000-8000-000000000031",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "regenerate this",
		Attachments: currentAgentAttachmentFixtures(t, "20000000-0000-4000-8000-000000000031"),
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-attach-5", turn, 1); err != nil {
		t.Fatal(err)
	}
	responseID := mustCurrentPGUUID(t, turn.ResponseMessageID)
	completePostgresCurrentApplicationTurn(t, tx, responseID, "an answer about the file")

	binding, err := queries.ResolveCurrentRegeneration(
		t.Context(),
		sqlcgen.ResolveCurrentRegenerationParams{
			ActorUserID: 11, ProjectID: 1, ResponseMessageID: responseID,
		},
	)
	if err != nil {
		t.Fatalf("a turn that carried a file cannot be regenerated: %v", err)
	}
	if binding.UserInput != "regenerate this" {
		t.Fatalf("binding=%+v", binding)
	}

	// The reset query needs the whole turn identity, not just the response —
	// passing a partial params struct makes it answer ErrNoRows for reasons
	// that have nothing to do with attachments, which would make this test
	// pass against a broken gate for the wrong reason.
	if _, err := queries.ResetCurrentAgentResponse(
		t.Context(),
		sqlcgen.ResetCurrentAgentResponseParams{
			TargetParticipantID:  21,
			ApplicationVersionID: 41,
			ApplicationID:        31,
			ConversationUuid:     mustCurrentPGUUID(t, turn.ConversationUUID),
			ResponseMessageID:    responseID,
			QuestionID:           mustCurrentPGUUID(t, turn.QuestionID),
			ActorUserID:          11,
			RegenerationKind:     "application",
			ProjectID:            1,
			ExecutionGeneration:  turn.QuestionID,
			ExecutionID:          "execution-attach-5-regen",
		},
	); err != nil {
		t.Fatalf("a turn that carried a file cannot be reset: %v", err)
	}
}
