package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresCurrentApplicationTurnAllowsASecondMessageOnTheSameConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID, _ := currentPGUUID("10000000-0000-4000-8000-000000000031")
	resolve := sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		QuestionID: mustCurrentPGUUID(
			t,
			"20000000-0000-4000-8000-000000000031",
		),
		ConversationUuid: conversationID, ProjectID: 1,
	}
	firstTarget, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if firstTarget.ApplicationID != 31 || firstTarget.ApplicationVersionID != 41 {
		t.Fatalf("first target=%+v", firstTarget)
	}
	var versionDetails struct {
		Skills []struct {
			SkillID        int32          `json:"skill_id"`
			SkillVersionID int32          `json:"skill_version_id"`
			Name           string         `json:"name"`
			VersionName    string         `json:"version_name"`
			Description    string         `json:"description"`
			IconMeta       map[string]any `json:"icon_meta"`
			Instructions   string         `json:"instructions"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(
		[]byte(firstTarget.ApplicationVersionDetailsJson),
		&versionDetails,
	); err != nil {
		t.Fatalf("decode application version details: %v", err)
	}
	if len(versionDetails.Skills) != 1 ||
		versionDetails.Skills[0].SkillID != 61 ||
		versionDetails.Skills[0].SkillVersionID != 71 ||
		versionDetails.Skills[0].Name != "release-proof" ||
		versionDetails.Skills[0].VersionName != "base" ||
		versionDetails.Skills[0].Description != "Release proof skill" ||
		versionDetails.Skills[0].Instructions != "Return RELEASE-PROOF" ||
		versionDetails.Skills[0].IconMeta["icon"] != "proof" {
		t.Fatalf("application skills=%s", firstTarget.ApplicationVersionDetailsJson)
	}
	if firstTarget.ChatHistoryJson != "[]" {
		t.Fatalf("first chat history=%s", firstTarget.ChatHistoryJson)
	}

	firstResponse := insertPostgresCurrentApplicationTurn(
		t,
		queries,
		conversationID,
		"20000000-0000-4000-8000-000000000031",
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"first turn",
		"execution-agent-turn-1",
	)
	completePostgresCurrentApplicationTurn(
		t,
		tx,
		firstResponse,
		"first response",
	)
	resolve.QuestionID = mustCurrentPGUUID(
		t,
		"50000000-0000-4000-8000-000000000031",
	)
	secondTarget, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve)
	if err != nil {
		t.Fatalf("resolve after first persisted turn: %v", err)
	}
	if secondTarget.ApplicationID != firstTarget.ApplicationID ||
		secondTarget.ApplicationVersionID != firstTarget.ApplicationVersionID {
		t.Fatalf("target changed across turns: first=%+v second=%+v", firstTarget, secondTarget)
	}
	var history []struct {
		Role             string              `json:"role"`
		Content          []map[string]string `json:"content"`
		AdditionalKwargs map[string]any      `json:"additional_kwargs"`
	}
	if err := json.Unmarshal([]byte(secondTarget.ChatHistoryJson), &history); err != nil {
		t.Fatalf("decode second chat history: %v", err)
	}
	if len(history) != 2 || history[0].Role != "user" ||
		len(history[0].Content) != 1 || history[0].Content[0]["text"] != "first turn" ||
		history[1].Role != "assistant" || len(history[1].Content) != 1 ||
		history[1].Content[0]["text"] != "first response" {
		t.Fatalf("second chat history=%s", secondTarget.ChatHistoryJson)
	}
	secondResponse := insertPostgresCurrentApplicationTurn(
		t,
		queries,
		conversationID,
		"50000000-0000-4000-8000-000000000031",
		"60000000-0000-4000-8000-000000000031",
		"70000000-0000-4000-8000-000000000031",
		"second turn",
		"execution-agent-turn-2",
	)
	if firstResponse == secondResponse {
		t.Fatal("distinct turns reused one response message identity")
	}
	thirdQuestionID := mustCurrentPGUUID(
		t,
		"80000000-0000-4000-8000-000000000031",
	)
	thirdQuestionItemID := mustCurrentPGUUID(
		t,
		"90000000-0000-4000-8000-000000000031",
	)
	thirdResponseID := mustCurrentPGUUID(
		t,
		"a0000000-0000-4000-8000-000000000031",
	)
	resolve.QuestionID = thirdQuestionID
	if _, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping turn resolution error=%v", err)
	}
	_, err = queries.InsertCurrentApplicationTurn(
		t.Context(),
		sqlcgen.InsertCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			ApplicationVersionID: 41, ApplicationID: 31,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: thirdQuestionID, QuestionMeta: []byte(`{}`),
			QuestionItemID: thirdQuestionItemID, UserInput: "overlap",
			ResponseMessageID:   thirdResponseID,
			ExecutionGeneration: "80000000-0000-4000-8000-000000000031",
			ExecutionID:         "execution-agent-overlap",
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping turn error=%v", err)
	}

	var groups, questions, responses int
	var contents []string
	if err := tx.QueryRow(t.Context(), `
SELECT count(*),
       count(*) FILTER (WHERE sent_to_id = 21),
       count(*) FILTER (WHERE reply_to_id IS NOT NULL),
       array_agg(text.content ORDER BY message_group.id)
           FILTER (WHERE text.content IS NOT NULL)
FROM chat_message_group AS message_group
LEFT JOIN chat_message_items AS item
  ON item.message_group_id = message_group.id
LEFT JOIN chat_messages_text AS text
  ON text.id = item.id`).Scan(&groups, &questions, &responses, &contents); err != nil {
		t.Fatal(err)
	}
	if groups != 4 || questions != 2 || responses != 2 ||
		len(contents) != 3 || contents[0] != "first turn" ||
		contents[1] != "first response" || contents[2] != "second turn" {
		t.Fatalf(
			"groups=%d questions=%d responses=%d contents=%v",
			groups,
			questions,
			responses,
			contents,
		)
	}

	completePostgresCurrentApplicationTurn(
		t,
		tx,
		secondResponse,
		"second response",
	)
	markPostgresResponseAsSupersededOrphan(t, tx, firstResponse)
	if _, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve); err != nil {
		t.Fatalf("resolve with superseded orphan response: %v", err)
	}
	if _, err := queries.InsertCurrentApplicationTurn(
		t.Context(),
		sqlcgen.InsertCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			ApplicationVersionID: 41, ApplicationID: 31,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: thirdQuestionID, QuestionMeta: []byte(`{}`),
			QuestionItemID: thirdQuestionItemID, UserInput: "after orphan",
			ResponseMessageID:   thirdResponseID,
			ExecutionGeneration: "80000000-0000-4000-8000-000000000031",
			ExecutionID:         "execution-agent-after-orphan",
		},
	); err != nil {
		t.Fatalf("insert with superseded orphan response: %v", err)
	}
}

func TestPostgresCurrentApplicationTurnAdmitsDefaultInternalMCPAndRejectsUnsupportedFeatures(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	resolve := sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000039"),
		ConversationUuid: conversationID, ProjectID: 1,
	}
	insert := sqlcgen.InsertCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		ApplicationVersionID: 41, ApplicationID: 31,
		ConversationUuid: conversationID, ProjectID: 1,
		QuestionID: resolve.QuestionID, QuestionMeta: []byte(`{}`),
		QuestionItemID:      mustCurrentPGUUID(t, "30000000-0000-4000-8000-000000000039"),
		UserInput:           "must fall back",
		ResponseMessageID:   mustCurrentPGUUID(t, "40000000-0000-4000-8000-000000000039"),
		ExecutionGeneration: "50000000-0000-4000-8000-000000000039",
		ExecutionID:         "execution-agent-unsupported",
	}
	if _, err := tx.Exec(t.Context(), "SAVEPOINT default_internal_mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE chat_conversations
SET meta = jsonb_set(meta, '{internal_tools}', '["internal_mcp"]'::jsonb)
WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve); err != nil {
		t.Fatalf("resolve selected application with default internal_mcp: %v", err)
	}
	if _, err := queries.InsertCurrentApplicationTurn(t.Context(), insert); err != nil {
		t.Fatalf("insert selected application turn with default internal_mcp: %v", err)
	}
	if _, err := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT default_internal_mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "RELEASE SAVEPOINT default_internal_mcp"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		apply   string
		restore string
	}{
		{
			name: "application internal tools",
			// The catalogue names (attachments, planner, ...) are ADMITTED
			// since the gates widened to the authorable set; what the version
			// gate still refuses is a name outside the catalogue. The same
			// synthetic name is the Rust corpus's refusal fixture.
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', '["not_a_platform_tool"]'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
		{
			// The non-array shape, held against the SELECTED application's own
			// gate rather than the adhoc negative branch. It must classify
			// (pgx.ErrNoRows -> 422), never raise 22023 -> 500. The clause it
			// exercises already wraps `jsonb_array_elements` in the CASE; this
			// pins that it keeps doing so.
			name: "application internal tools scalar",
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', '"planner"'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
		{
			name: "project context",
			apply: `INSERT INTO configuration (
    id, uuid, project_id, elitea_title, type, section, data, meta,
    shared, status_ok, source, author_id
) VALUES (
    2, '60000000-0000-4000-8000-000000000039', 1, 'project_context_gate',
    'project_context', 'project', '{"content":"Project instructions"}'::jsonb,
    '{}'::jsonb, false, true, 'user', 11
)`,
			restore: `DELETE FROM configuration WHERE elitea_title = 'project_context_gate'`,
		},
		{
			name: "conversation toolkit",
			apply: `WITH toolkit AS (
    INSERT INTO chat_participants (id, uuid, entity_name, entity_meta)
    VALUES (
        25, '70000000-0000-4000-8000-000000000039', 'toolkit',
        '{"id":51,"project_id":1}'::jsonb
    )
    RETURNING id
)
INSERT INTO chat_participant_mapping (conversation_id, participant_id, entity_settings)
SELECT 1, toolkit.id, '{}'::jsonb FROM toolkit`,
			restore: `WITH removed AS (
    DELETE FROM chat_participant_mapping WHERE conversation_id = 1 AND participant_id = 25
)
DELETE FROM chat_participants WHERE id = 25`,
		},
		{
			name: "summarized history",
			apply: `UPDATE chat_conversations
SET meta = jsonb_set(
    meta,
    '{context_analytics}',
    '{"last_summarization":{"summary_content":"Earlier context"}}'::jsonb
)
WHERE id = 1`,
			restore: `UPDATE chat_conversations SET meta = meta - 'context_analytics' WHERE id = 1`,
		},
		{
			name: "context message history",
			apply: `WITH message_group AS (
    INSERT INTO chat_message_group (
        uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming
    ) VALUES (
        '80000000-0000-4000-8000-000000000039', 20, 1, 21, '{}'::jsonb, false
    )
    RETURNING id
), message_item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT '90000000-0000-4000-8000-000000000039',
           'context_message', 0, '{}'::jsonb, message_group.id
    FROM message_group
    RETURNING id
)
INSERT INTO chat_messages_context (id, context_data, context_type)
SELECT message_item.id, '{"user_id":"11","project_id":"1"}'::jsonb,
       'support_assistant_context'
FROM message_item`,
			restore: `WITH removed_context AS (
    DELETE FROM chat_messages_context
    WHERE id = (
        SELECT id FROM chat_message_items
        WHERE uuid = '90000000-0000-4000-8000-000000000039'
    )
), removed_item AS (
    DELETE FROM chat_message_items
    WHERE uuid = '90000000-0000-4000-8000-000000000039'
)
DELETE FROM chat_message_group
WHERE uuid = '80000000-0000-4000-8000-000000000039'`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := tx.Exec(t.Context(), test.apply); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("resolve error=%v", err)
			}
			if _, err := queries.InsertCurrentApplicationTurn(t.Context(), insert); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("insert error=%v", err)
			}
			if _, err := tx.Exec(t.Context(), test.restore); err != nil {
				t.Fatal(err)
			}
		})
	}

	if _, err := queries.ResolveCurrentApplicationTurn(t.Context(), resolve); err != nil {
		t.Fatalf("bounded application turn remained rejected after restoring fixtures: %v", err)
	}
}

func TestPostgresCurrentPipelineTurnAdmitsDirectAndAdhocEntryModes(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE application_versions
SET agent_type = 'pipeline',
    instructions = 'nodes:\n  - id: draft\n    type: llm\n  - id: approve\n    type: hitl\nedges:\n  - from: draft\n    to: approve'
WHERE id = 41;
UPDATE chat_participants
SET meta = jsonb_set(meta::jsonb, '{agent_type}', '"pipeline"'::jsonb)::json
WHERE id = 30`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	directConversation := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	directQuestion := mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000037")
	direct, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21, QuestionID: directQuestion,
			ConversationUuid: directConversation, ProjectID: 1,
		},
	)
	if err != nil {
		t.Fatalf("resolve direct pipeline: %v", err)
	}
	var versionDetails map[string]any
	if err := json.Unmarshal([]byte(direct.ApplicationVersionDetailsJson), &versionDetails); err != nil ||
		versionDetails["agent_type"] != "pipeline" ||
		versionDetails["instructions"] == "" {
		t.Fatalf("direct pipeline version=%s error=%v", direct.ApplicationVersionDetailsJson, err)
	}
	if _, err := queries.InsertCurrentApplicationTurn(
		t.Context(),
		sqlcgen.InsertCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			ApplicationVersionID: 41, ApplicationID: 31,
			ConversationUuid: directConversation, ProjectID: 1,
			QuestionID: directQuestion, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "30000000-0000-4000-8000-000000000037"),
			UserInput:           "run direct pipeline",
			ResponseMessageID:   mustCurrentPGUUID(t, "40000000-0000-4000-8000-000000000037"),
			ExecutionGeneration: "20000000-0000-4000-8000-000000000037",
			ExecutionID:         "execution-pipeline-direct",
		},
	); err != nil {
		t.Fatalf("insert direct pipeline turn: %v", err)
	}

	adhocConversation := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	adhocQuestion := mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000037")
	adhoc, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID: adhocQuestion, ConversationUuid: adhocConversation,
		},
	)
	if err != nil {
		t.Fatalf("resolve ad-hoc pipeline: %v", err)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(adhoc.ToolsJson), &tools); err != nil || len(tools) != 2 ||
		tools[1]["type"] != "application" || tools[1]["agent_type"] != "pipeline" {
		t.Fatalf("ad-hoc pipeline tools=%s error=%v", adhoc.ToolsJson, err)
	}
	if _, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: adhocConversation, ProjectID: 1,
			QuestionID: adhocQuestion, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "60000000-0000-4000-8000-000000000037"),
			UserInput:           "run attached pipeline",
			ResponseMessageID:   mustCurrentPGUUID(t, "70000000-0000-4000-8000-000000000037"),
			ExecutionGeneration: "50000000-0000-4000-8000-000000000037",
			ExecutionID:         "execution-pipeline-adhoc",
		},
	); err != nil {
		t.Fatalf("insert ad-hoc pipeline turn: %v", err)
	}
}

func TestPostgresCurrentNestedApplicationTurnPreservesThreeTierContract(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES
    (42, 32, 'nested-orchestrator', 'draft', 11, '80000000-0000-4000-8000-000000000032',
     '{"model_name":"test"}'::jsonb, 'Delegate once', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb),
    (43, 33, 'nested-leaf', 'draft', 11, '80000000-0000-4000-8000-000000000033',
     '{"model_name":"test"}'::jsonb, 'Return the result', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb),
    (44, 34, 'attached-orchestrator', 'draft', 11, '80000000-0000-4000-8000-000000000034',
     '{"model_name":"test"}'::jsonb, 'Delegate once', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb),
    (45, 35, 'attached-leaf', 'draft', 11, '80000000-0000-4000-8000-000000000035',
     '{"model_name":"test"}'::jsonb, 'Return the result', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb);
INSERT INTO elitea_tools (id, type, name, description, settings, author_id, meta) VALUES
    (52, 'application', 'nested-orchestrator', NULL,
     '{"application_id":32,"application_version_id":42}'::jsonb, 11, '{}'::jsonb),
    (53, 'application', 'nested-leaf', NULL,
     '{"application_id":33,"application_version_id":43}'::jsonb, 11, '{}'::jsonb),
    (54, 'application', 'attached-leaf', NULL,
     '{"application_id":35,"application_version_id":45}'::jsonb, 11, '{}'::jsonb);
INSERT INTO entity_tool_mapping (
    tool_id, entity_id, entity_version_id, entity_type, selected_tools
) VALUES
    (52, 31, 41, 'agent', NULL),
    (53, 32, 42, 'agent', NULL),
    (54, 34, 44, 'agent', NULL);
UPDATE chat_participants
SET entity_meta = '{"id":34,"project_id":1}'::jsonb,
    meta = '{"name":"attached-orchestrator","description":"Nested child","agent_type":"agent"}'::json
WHERE id = 30;
UPDATE chat_participant_mapping
SET entity_settings = '{"version_id":44,"variables":[],"agent_type":"agent"}'::jsonb
WHERE conversation_id = 2 AND participant_id = 30;`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	if err := validateCurrentApplicationNesting(t.Context(), queries, 41, 1); err != nil {
		t.Fatalf("validate selected three-tier agent: %v", err)
	}
	direct, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000038"),
			ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
			ProjectID:        1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var versionDetails struct {
		Tools []struct {
			Type      string         `json:"type"`
			AgentType string         `json:"agent_type"`
			Settings  map[string]any `json:"settings"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(direct.ApplicationVersionDetailsJson), &versionDetails); err != nil ||
		len(versionDetails.Tools) != 1 || versionDetails.Tools[0].Type != "application" ||
		versionDetails.Tools[0].AgentType != "agent" ||
		versionDetails.Tools[0].Settings["application_version_id"] != float64(42) {
		t.Fatalf("direct nested version=%s error=%v", direct.ApplicationVersionDetailsJson, err)
	}

	adhocConversation := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	adhocQuestion := mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000038")
	adhoc, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID: adhocQuestion, ConversationUuid: adhocConversation,
		},
	)
	if err != nil {
		t.Fatalf("resolve attached nested agent: %v", err)
	}
	filtered, err := filterCurrentAdhocApplicationNesting(
		t.Context(),
		queries,
		json.RawMessage(adhoc.ToolsJson),
	)
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(filtered, &tools); err != nil || len(tools) != 2 ||
		tools[1]["type"] != "application" || tools[1]["agent_type"] != "agent" {
		t.Fatalf("ad-hoc nested tools=%s error=%v", filtered, err)
	}
	if _, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: adhocConversation, ProjectID: 1,
			QuestionID: adhocQuestion, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "60000000-0000-4000-8000-000000000038"),
			UserInput:           "run attached nested agent",
			ResponseMessageID:   mustCurrentPGUUID(t, "70000000-0000-4000-8000-000000000038"),
			ExecutionGeneration: "50000000-0000-4000-8000-000000000038",
			ExecutionID:         "execution-nested-adhoc",
		},
	); err != nil {
		t.Fatalf("insert attached nested turn: %v", err)
	}
}

func TestPostgresCurrentAdhocTurnPreservesToolsHistoryAndOverlapGate(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	resolve := sqlcgen.ResolveCurrentAdhocTurnParams{
		ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
		QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000032"),
		ConversationUuid: conversationID,
	}
	first, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if first.TargetParticipantID != 23 || first.Instructions != "Conversation instructions\n\nUser defaults" ||
		first.LlmSettingsJson != `{"max_tokens": 1024, "model_name": "saved", "model_project_id": 1}` ||
		first.ChatHistoryJson != "[]" {
		t.Fatalf("first target=%+v", first)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(first.ToolsJson), &tools); err != nil || len(tools) != 2 ||
		tools[0]["id"] != float64(51) || tools[0]["project_id"] != float64(1) ||
		tools[0]["type"] != "aha" || tools[1]["type"] != "application" ||
		tools[1]["id"] != nil || tools[1]["name"] != "leaf-agent" ||
		tools[1]["toolkit_name"] != "leaf-agent" || tools[1]["participant_id"] != float64(30) ||
		tools[1]["project_id"] != float64(1) ||
		tools[1]["settings"].(map[string]any)["application_id"] != float64(31) ||
		tools[1]["settings"].(map[string]any)["application_version_id"] != float64(41) {
		t.Fatalf("tools=%s error=%v", first.ToolsJson, err)
	}

	firstResponse := insertPostgresCurrentAdhocTurn(
		t, queries, conversationID,
		"20000000-0000-4000-8000-000000000032",
		"30000000-0000-4000-8000-000000000032",
		"40000000-0000-4000-8000-000000000032",
		"first ad-hoc turn", "execution-adhoc-turn-1",
	)
	completePostgresCurrentApplicationTurn(t, tx, firstResponse, "first ad-hoc response")
	insertPostgresSupportContext(
		t,
		tx,
		"20000000-0000-4000-8000-000000000032",
		11,
		1,
	)
	resolve.QuestionID = mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000032")
	second, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	var history []map[string]any
	if err := json.Unmarshal([]byte(second.ChatHistoryJson), &history); err != nil || len(history) != 2 {
		t.Fatalf("history=%s error=%v", second.ChatHistoryJson, err)
	}
	secondResponse := insertPostgresCurrentAdhocTurn(
		t, queries, conversationID,
		"50000000-0000-4000-8000-000000000032",
		"60000000-0000-4000-8000-000000000032",
		"70000000-0000-4000-8000-000000000032",
		"second ad-hoc turn", "execution-adhoc-turn-2",
	)
	if firstResponse == secondResponse {
		t.Fatal("distinct ad-hoc turns reused one response message identity")
	}
	resolve.QuestionID = mustCurrentPGUUID(t, "80000000-0000-4000-8000-000000000032")
	if _, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping ad-hoc resolution error=%v", err)
	}
	_, err = queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID:          mustCurrentPGUUID(t, "80000000-0000-4000-8000-000000000032"),
			QuestionMeta:        []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "90000000-0000-4000-8000-000000000032"),
			UserInput:           "overlap",
			ResponseMessageID:   mustCurrentPGUUID(t, "a0000000-0000-4000-8000-000000000032"),
			ExecutionGeneration: "80000000-0000-4000-8000-000000000032",
			ExecutionID:         "execution-adhoc-overlap",
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping ad-hoc turn error=%v", err)
	}

	completePostgresCurrentApplicationTurn(
		t,
		tx,
		secondResponse,
		"second ad-hoc response",
	)
	markPostgresResponseAsSupersededOrphan(t, tx, firstResponse)
	if _, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve); err != nil {
		t.Fatalf("resolve ad-hoc turn with superseded orphan response: %v", err)
	}
	if _, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: resolve.QuestionID, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "90000000-0000-4000-8000-000000000032"),
			UserInput:           "after orphan",
			ResponseMessageID:   mustCurrentPGUUID(t, "a0000000-0000-4000-8000-000000000032"),
			ExecutionGeneration: "80000000-0000-4000-8000-000000000032",
			ExecutionID:         "execution-adhoc-after-orphan",
		},
	); err != nil {
		t.Fatalf("insert ad-hoc turn with superseded orphan response: %v", err)
	}
}

func TestPostgresCurrentAdhocTurnAdmitsSameProjectMCPToolkit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_tools (id, type, name, description, settings, author_id, meta)
VALUES (
    52, 'mcp', 'documentation-mcp', 'Saved external MCP server',
    '{"url":"https://mcp.example.invalid/events","selected_tools":["search_docs"]}'::jsonb,
    11, '{"mcp":true}'::jsonb
);
INSERT INTO chat_participants (id, uuid, entity_name, entity_meta, meta)
VALUES (
    25, 'd0000000-0000-4000-8000-000000000032', 'toolkit',
    '{"id":52,"project_id":1}'::jsonb, '{"name":"documentation-mcp","mcp":true}'::json
);
INSERT INTO chat_participant_mapping (conversation_id, participant_id, entity_settings)
VALUES (2, 25, '{}'::jsonb);`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	questionID := mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000039")
	resolved, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID: questionID, ConversationUuid: conversationID,
		},
	)
	if err != nil {
		t.Fatalf("resolve direct MCP toolkit: %v", err)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(resolved.ToolsJson), &tools); err != nil {
		t.Fatalf("decode direct MCP toolkit: %v", err)
	}
	var mcpTool map[string]any
	for _, tool := range tools {
		if tool["id"] == float64(52) {
			mcpTool = tool
			break
		}
	}
	meta, validMeta := mcpTool["meta"].(map[string]any)
	if len(tools) != 3 || mcpTool["type"] != "mcp" ||
		mcpTool["toolkit_name"] != "documentation-mcp" || mcpTool["project_id"] != float64(1) ||
		!validMeta || meta["mcp"] != true {
		t.Fatalf("direct MCP tools=%s", resolved.ToolsJson)
	}
	settings, ok := mcpTool["settings"].(map[string]any)
	if !ok || settings["url"] != "https://mcp.example.invalid/events" ||
		!reflect.DeepEqual(settings["selected_tools"], []any{"search_docs"}) {
		t.Fatalf("direct MCP settings=%#v", mcpTool["settings"])
	}

	if _, err := tx.Exec(t.Context(), `
UPDATE chat_participants
SET entity_meta = jsonb_set(entity_meta, '{project_id}', '2'::jsonb)
WHERE id = 25`); err != nil {
		t.Fatal(err)
	}
	_, err = queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000039"),
			ConversationUuid: conversationID,
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project MCP resolve error=%v", err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_participants
SET entity_meta = jsonb_set(entity_meta, '{project_id}', '1'::jsonb)
WHERE id = 25`); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: questionID, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "30000000-0000-4000-8000-000000000039"),
			UserInput:           "use the saved MCP toolkit",
			ResponseMessageID:   mustCurrentPGUUID(t, "40000000-0000-4000-8000-000000000039"),
			ExecutionGeneration: "20000000-0000-4000-8000-000000000039",
			ExecutionID:         "execution-adhoc-mcp",
		},
	); err != nil {
		t.Fatalf("insert direct MCP turn: %v", err)
	}
}

func TestPostgresCurrentAdhocTurnAdmitsApplicationWithMCPChild(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_tools (id, type, name, description, settings, author_id, meta)
VALUES (
    52, 'mcp', 'child-mcp', 'MCP child owned by the attached application',
    '{"url":"https://mcp.example.invalid/events","selected_tools":["search_docs"]}'::jsonb,
    11, '{"mcp":true}'::jsonb
);
INSERT INTO entity_tool_mapping (
    tool_id, entity_id, entity_version_id, entity_type, selected_tools
) VALUES (52, 31, 41, 'agent', '["search_docs"]'::jsonb);`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	questionID := mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000040")
	resolved, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID: questionID, ConversationUuid: conversationID,
		},
	)
	if err != nil {
		t.Fatalf("resolve application with MCP child: %v", err)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(resolved.ToolsJson), &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	var application map[string]any
	for _, tool := range tools {
		if tool["type"] == "application" {
			application = tool
			break
		}
	}
	settings, validSettings := application["settings"].(map[string]any)
	if len(tools) != 2 || application["toolkit_name"] != "leaf-agent" ||
		!validSettings || settings["application_version_id"] != float64(41) {
		t.Fatalf("application with MCP child tools=%s", resolved.ToolsJson)
	}

	if _, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: questionID, QuestionMeta: []byte(`{}`),
			QuestionItemID:      mustCurrentPGUUID(t, "30000000-0000-4000-8000-000000000040"),
			UserInput:           "run the attached application without invoking its MCP child",
			ResponseMessageID:   mustCurrentPGUUID(t, "40000000-0000-4000-8000-000000000040"),
			ExecutionGeneration: "20000000-0000-4000-8000-000000000040",
			ExecutionID:         "execution-adhoc-application-mcp-child",
		},
	); err != nil {
		t.Fatalf("insert application with MCP child turn: %v", err)
	}
}

func TestPostgresCurrentAdhocTurnRejectsUnsupportedApplicationParticipantBoundaries(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	resolve := sqlcgen.ResolveCurrentAdhocTurnParams{
		ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
		QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000038"),
		ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032"),
	}
	tests := []struct {
		name    string
		apply   string
		restore string
	}{
		{
			name: "cross project child",
			apply: `UPDATE chat_participants
SET entity_meta = jsonb_set(entity_meta, '{project_id}', '2'::jsonb)
WHERE id = 30`,
			restore: `UPDATE chat_participants
SET entity_meta = jsonb_set(entity_meta, '{project_id}', '1'::jsonb)
WHERE id = 30`,
		},
		{
			name: "child internal tools",
			// See the application gate above: catalogue names are admitted
			// now, so the refusal fixture must sit outside the catalogue.
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', '["not_a_platform_tool"]'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
		// The next three fixtures are not more names to refuse: they are the
		// three NON-ARRAY shapes `meta.internal_tools` can hold. Each one has
		// to come back as pgx.ErrNoRows — a classification the caller answers
		// 422 with — and NOT as SQLSTATE 22023 "cannot extract elements from a
		// scalar", which the caller can only answer 500. The negative
		// participant branches of ResolveCurrentAdhocTurn and
		// InsertCurrentAdhocTurn feed the same value to
		// `jsonb_array_elements`, and a bare `OR jsonb_typeof(...) <> 'array'`
		// beside that call is not a guard: PostgreSQL does not promise the
		// arms of an OR are evaluated in written order, so the CASE the
		// positive clauses use is required there too.
		{
			name: "child internal tools scalar string",
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', '"planner"'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
		{
			name: "child internal tools json null",
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', 'null'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
		{
			name: "child internal tools object",
			apply: `UPDATE application_versions
SET meta = jsonb_set(meta, '{internal_tools}', '{"planner": true}'::jsonb)
WHERE id = 41`,
			restore: `UPDATE application_versions SET meta = meta - 'internal_tools' WHERE id = 41`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := tx.Exec(t.Context(), test.apply); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("resolve error=%v", err)
			}
			_, err := queries.InsertCurrentAdhocTurn(
				t.Context(),
				sqlcgen.InsertCurrentAdhocTurnParams{
					ActorUserID: 11, TargetParticipantID: 23,
					ConversationUuid: resolve.ConversationUuid, ProjectID: 1,
					QuestionID: resolve.QuestionID, QuestionMeta: []byte(`{}`),
					QuestionItemID: mustCurrentPGUUID(
						t,
						"30000000-0000-4000-8000-000000000038",
					),
					UserInput: "must fall back",
					ResponseMessageID: mustCurrentPGUUID(
						t,
						"40000000-0000-4000-8000-000000000038",
					),
					ExecutionGeneration: "20000000-0000-4000-8000-000000000038",
					ExecutionID:         "execution-adhoc-unsupported",
				},
			)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("insert error=%v", err)
			}
			if _, err := tx.Exec(t.Context(), test.restore); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := queries.ResolveCurrentAdhocTurn(t.Context(), resolve); err != nil {
		t.Fatalf("bounded leaf application remained rejected after restoring fixtures: %v", err)
	}
}

func TestPostgresCurrentRegenerationResolvesOwnershipAndAtomicallyReusesResponse(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000031"
	responseID := "40000000-0000-4000-8000-000000000031"
	response := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000031", responseID,
		"regenerate this", "execution-original",
	)
	// Main-chat user turns include a persisted context envelope alongside the
	// visible text. The current WebSocket regeneration path ignores that item;
	// typed regeneration must accept the same stored shape while attachments and
	// canvases remain outside this focused contract.
	insertPostgresSupportContext(t, tx, questionID, 11, 1)
	pending, err := queries.ResolveCurrentRegeneration(
		t.Context(),
		sqlcgen.ResolveCurrentRegenerationParams{
			ActorUserID: 11, ProjectID: 1, ResponseMessageID: response,
		},
	)
	if err != nil || !pending.ResponseIsStreaming {
		t.Fatalf("resolve streaming response: binding=%+v error=%v", pending, err)
	}
	completePostgresCurrentApplicationTurn(t, tx, response, "discarded answer")
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET meta = jsonb_set(
    COALESCE(meta, '{}'::jsonb),
    '{invoked_skills}',
    '[{"skill_id": 91, "name": "Discarded skill", "icon_meta": null}]'::jsonb,
    TRUE
)
WHERE uuid = $1`, response); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO chat_message_trace_step (
    id, message_group_id, kind, run_id, is_error, has_visible_content
)
SELECT 1, id, 'tool_call', 'discarded-run', FALSE, TRUE
FROM chat_message_group
WHERE uuid = $1`, response); err != nil {
		t.Fatal(err)
	}

	binding, err := queries.ResolveCurrentRegeneration(
		t.Context(),
		sqlcgen.ResolveCurrentRegenerationParams{
			ActorUserID: 11, ProjectID: 1, ResponseMessageID: response,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ResponseIsStreaming || binding.RegenerationKind != "application" || binding.TargetParticipantID != 21 ||
		binding.QuestionID != mustCurrentPGUUID(t, questionID) ||
		binding.ConversationUuid != conversationID || binding.UserInput != "regenerate this" {
		t.Fatalf("binding=%+v", binding)
	}
	if _, err := queries.ResolveCurrentRegeneration(
		t.Context(),
		sqlcgen.ResolveCurrentRegenerationParams{
			ActorUserID: 12, ProjectID: 1, ResponseMessageID: response,
		},
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-user resolution error=%v", err)
	}

	generation := "50000000-0000-4000-8000-000000000031"
	reset, err := queries.ResetCurrentAgentResponse(
		t.Context(),
		sqlcgen.ResetCurrentAgentResponseParams{
			ActorUserID: 11, TargetParticipantID: 21,
			ConversationUuid: conversationID, QuestionID: mustCurrentPGUUID(t, questionID),
			ResponseMessageID: response, RegenerationKind: "application",
			ApplicationID: 31, ApplicationVersionID: 41,
			ExecutionGeneration: generation, ExecutionID: "execution-regenerated",
			ProjectID: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reset.ResponseMessageID != response {
		t.Fatalf("reset=%+v", reset)
	}
	var isStreaming bool
	var taskID, storedGeneration, invokedSkills string
	var items, traces int
	if err := tx.QueryRow(t.Context(), `
SELECT response.is_streaming,
       response.task_id,
       response.meta ->> 'execution_generation',
       COALESCE(response.meta -> 'invoked_skills', '[]'::jsonb)::text,
       (SELECT count(*) FROM chat_message_items WHERE message_group_id = response.id),
       (SELECT count(*) FROM chat_message_trace_step WHERE message_group_id = response.id)
FROM chat_message_group AS response
WHERE response.uuid = $1`, response).Scan(
		&isStreaming, &taskID, &storedGeneration, &invokedSkills, &items, &traces,
	); err != nil {
		t.Fatal(err)
	}
	if !isStreaming || taskID != "execution-regenerated" || storedGeneration != generation ||
		invokedSkills != "[]" || items != 0 || traces != 0 {
		t.Fatalf("streaming=%v task=%q generation=%q skills=%s items=%d traces=%d",
			isStreaming, taskID, storedGeneration, invokedSkills, items, traces)
	}
}

func completePostgresCurrentApplicationTurn(
	t *testing.T,
	tx pgx.Tx,
	responseID pgtype.UUID,
	content string,
) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
WITH response_group AS (
    UPDATE chat_message_group
    SET is_streaming = FALSE,
        created_at = statement_timestamp()
    WHERE uuid = $1
    RETURNING id
), response_item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, response_group.id
    FROM response_group
    RETURNING id
)
INSERT INTO chat_messages_text (id, content)
SELECT response_item.id, $2
FROM response_item`, responseID, content); err != nil {
		t.Fatal(err)
	}
}

func markPostgresResponseAsSupersededOrphan(
	t *testing.T,
	tx pgx.Tx,
	responseID pgtype.UUID,
) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = TRUE,
    created_at = created_at - INTERVAL '1 hour'
WHERE uuid = $1`, responseID); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCurrentOutputContinuationResolvesVisibleTextAndConsumesOneSequence(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000061"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000061",
		"40000000-0000-4000-8000-000000000061",
		"Explain durable workers", "execution-output-limited",
	)
	completePostgresCurrentApplicationTurn(t, tx, responseID, "A durable worker stores ")
	if _, err := tx.Exec(t.Context(), `
WITH response AS (
    UPDATE chat_message_group
    SET meta = meta || jsonb_build_object(
        'thread_id', 'thread-output-1',
        'execution_generation', $2::text,
        'output_limit_reached', TRUE,
        'output_limit_sequence', 1
    )
    WHERE uuid = $1
    RETURNING id
), item AS (
    INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 1, '{}'::jsonb, response.id
    FROM response
    RETURNING id
)
INSERT INTO chat_messages_text (id, content)
SELECT item.id, 'progress before acknowledgement.' FROM item`, responseID, questionID); err != nil {
		t.Fatal(err)
	}

	resolve := sqlcgen.ResolveCurrentOutputLimitContinuationParams{
		ActorUserID: 12, ProjectID: 1, ConversationUuid: conversationID,
		ResponseMessageID: responseID,
	}
	if _, err := queries.ResolveCurrentOutputLimitContinuation(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-user resolution error=%v", err)
	}
	resolve.ActorUserID = 11
	resolved, err := queries.ResolveCurrentOutputLimitContinuation(t.Context(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContinuationKind != "application" || resolved.UserInput != "Explain durable workers" ||
		resolved.TruncatedContent != "A durable worker stores progress before acknowledgement." ||
		resolved.ThreadID != "thread-output-1" || resolved.ExecutionGeneration != questionID ||
		resolved.OutputLimitSequence != 1 {
		t.Fatalf("resolved=%+v", resolved)
	}

	resume := sqlcgen.ResumeCurrentAgentOutputLimitParams{
		ActorUserID: 11, ProjectID: 1, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41, ContinuationKind: "application",
		ConversationUuid: conversationID, QuestionID: mustCurrentPGUUID(t, questionID),
		ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-output-1", OutputLimitSequence: 2,
		ExecutionID: "execution-output-continued",
	}
	if _, err := queries.ResumeCurrentAgentOutputLimit(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong sequence resume error=%v", err)
	}
	resume.OutputLimitSequence = 1
	row, err := queries.ResumeCurrentAgentOutputLimit(t.Context(), resume)
	if err != nil {
		t.Fatal(err)
	}
	if row.ResponseMessageID != responseID || row.ResponseMessageGroupID <= 0 {
		t.Fatalf("resume row=%+v", row)
	}
	if _, err := queries.ResumeCurrentAgentOutputLimit(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed resume error=%v", err)
	}
	if _, err := queries.ResolveCurrentOutputLimitContinuation(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consumed continuation remained visible: %v", err)
	}

	var isStreaming, hasOutputLimit bool
	var taskID string
	if err := tx.QueryRow(t.Context(), `
SELECT is_streaming, task_id, meta ? 'output_limit_reached'
FROM chat_message_group
WHERE uuid = $1`, responseID).Scan(&isStreaming, &taskID, &hasOutputLimit); err != nil {
		t.Fatal(err)
	}
	if !isStreaming || taskID != "execution-output-continued" || hasOutputLimit {
		t.Fatalf("streaming=%v task=%q output_limit=%v", isStreaming, taskID, hasOutputLimit)
	}
}

func TestPostgresCurrentSequentialNestedHITLContinuationConsumesExistingResponseAtomically(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000051"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000051",
		"40000000-0000-4000-8000-000000000051",
		"delete the stale branch", "execution-paused",
	)
	if _, err := tx.Exec(t.Context(), `
WITH paused AS (
    UPDATE chat_message_group
    SET is_streaming = FALSE,
        meta = meta || jsonb_build_object(
            'thread_id', 'thread-hitl-1',
            'execution_generation', $2::text,
            'hitl_interrupt', jsonb_build_object(
                'interrupt_id', 'interrupt-root-1',
                'available_actions', jsonb_build_array(
                    'approve', 'reject', 'edit', 'block_with_comment'
                ),
                'tool_name', 'configurations:delete_branch'
            ) || jsonb_build_object(
                'parent_agent_call_id', 'call-pipeline-1',
                'parent_agent_path', jsonb_build_array(
                    jsonb_build_object(
                        'name', 'release-pipeline',
                        'call_id', 'call-pipeline-1'
                    )
                )
            )
        )
    WHERE uuid = $1
    RETURNING id
)
INSERT INTO chat_message_trace_step (
    id, message_group_id, kind, run_id, tool_name, step_type, text, attrs
)
SELECT 51, paused.id, 'hitl', 'run-hitl-1', 'configurations:delete_branch',
       'agent_hitl_interrupt', 'Approval required',
       '{"interrupt_id":"interrupt-root-1"}'::jsonb
FROM paused`, responseID, questionID); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveCurrentContinuation(
		t.Context(),
		sqlcgen.ResolveCurrentContinuationParams{
			ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
			ResponseMessageID: responseID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContinuationKind != "application" ||
		resolved.ThreadID != "thread-hitl-1" || resolved.ExecutionGeneration != questionID ||
		resolved.UserInput != "delete the stale branch" {
		t.Fatalf("resolved=%+v", resolved)
	}

	resume := sqlcgen.ResumeCurrentAgentHITLParams{
		ActorUserID: 12, ProjectID: 1, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41, ContinuationKind: "application",
		ConversationUuid: conversationID, QuestionID: mustCurrentPGUUID(t, questionID),
		ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID:      "thread-hitl-1",
		HitlDecisions: []byte(`[{"interrupt_id":"interrupt-root-1","action":"approve"}]`),
		ExecutionID:   "execution-resumed",
	}
	if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong actor resume error=%v", err)
	}
	resume.ActorUserID = 11
	resume.HitlDecisions = []byte(`[{"interrupt_id":"interrupt-root-1","action":"answer"}]`)
	if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unavailable action resume error=%v", err)
	}
	resume.HitlDecisions = []byte(`[{"interrupt_id":"interrupt-root-1","action":"approve"}]`)
	row, err := queries.ResumeCurrentAgentHITL(t.Context(), resume)
	if err != nil {
		t.Fatal(err)
	}
	if row.ResponseMessageID != responseID || row.ResponseMessageGroupID <= 0 {
		t.Fatalf("resume row=%+v", row)
	}
	if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed resume error=%v", err)
	}
	if _, err := queries.ResolveCurrentContinuation(
		t.Context(),
		sqlcgen.ResolveCurrentContinuationParams{
			ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
			ResponseMessageID: responseID,
		},
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resolved continuation remained visible: %v", err)
	}

	var isStreaming, hasPending bool
	var taskID string
	var resolvedIDs []string
	var traceCount int
	if err := tx.QueryRow(t.Context(), `
SELECT response.is_streaming,
       response.task_id,
       response.meta ? 'hitl_interrupt',
       ARRAY(
           SELECT jsonb_array_elements_text(
               COALESCE(response.meta -> 'resolved_hitl_interrupt_ids', '[]'::jsonb)
           )
       ),
       (SELECT count(*) FROM chat_message_trace_step WHERE message_group_id = response.id)
FROM chat_message_group AS response
WHERE response.uuid = $1`, responseID).Scan(
		&isStreaming, &taskID, &hasPending, &resolvedIDs, &traceCount,
	); err != nil {
		t.Fatal(err)
	}
	if !isStreaming || taskID != "execution-resumed" || hasPending ||
		len(resolvedIDs) != 1 || resolvedIDs[0] != "interrupt-root-1" || traceCount != 1 {
		t.Fatalf(
			"streaming=%v task=%q pending=%v resolved=%v traces=%d",
			isStreaming, taskID, hasPending, resolvedIDs, traceCount,
		)
	}
}

func TestPostgresCurrentParallelHITLContinuationConsumesExactDecisionSetAtomically(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000071"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000071",
		"40000000-0000-4000-8000-000000000071",
		"approve one change and block the other", "execution-parallel-hitl-paused",
	)
	const pending = `[
		{
			"interrupt_id":"interrupt-parallel-1",
			"available_actions":["approve","reject","edit","block_with_comment"],
			"tool_name":"configurations:update_file"
		},
		{
			"interrupt_id":"interrupt-parallel-2",
			"available_actions":["approve","reject","edit","block_with_comment"],
			"tool_name":"configurations:delete_file"
		}
	]`
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-parallel-hitl-1',
        'execution_generation', $2::text,
        'hitl_interrupt', ($3::jsonb -> 0),
        'hitl_interrupts', $3::jsonb
    )
WHERE uuid = $1`, responseID, questionID, pending); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveCurrentContinuation(
		t.Context(),
		sqlcgen.ResolveCurrentContinuationParams{
			ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
			ResponseMessageID: responseID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var pendingInterrupts []map[string]any
	if err := json.Unmarshal([]byte(resolved.HitlInterruptsJson), &pendingInterrupts); err != nil {
		t.Fatal(err)
	}
	if len(pendingInterrupts) != 2 {
		t.Fatalf("pending interrupts=%v", pendingInterrupts)
	}

	resume := sqlcgen.ResumeCurrentAgentHITLParams{
		ActorUserID: 11, ProjectID: 1, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41, ContinuationKind: "application",
		ConversationUuid: conversationID, QuestionID: mustCurrentPGUUID(t, questionID),
		ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-parallel-hitl-1", ExecutionID: "execution-parallel-hitl-resumed",
	}
	invalidDecisionSets := [][]byte{
		[]byte(`[{"interrupt_id":"interrupt-parallel-1","action":"approve"}]`),
		[]byte(`[
			{"interrupt_id":"interrupt-parallel-1","action":"approve"},
			{"interrupt_id":"interrupt-parallel-1","action":"reject"}
		]`),
		[]byte(`[
			{"interrupt_id":"interrupt-parallel-1","action":"approve"},
			{"interrupt_id":"interrupt-unknown","action":"reject"}
		]`),
		[]byte(`[
			{"interrupt_id":"interrupt-parallel-1","action":"approve"},
			{"interrupt_id":"interrupt-parallel-2","action":"answer"}
		]`),
	}
	for _, decisions := range invalidDecisionSets {
		resume.HitlDecisions = decisions
		if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid decisions %s error=%v", decisions, err)
		}
	}

	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET meta = jsonb_set(meta, '{hitl_interrupts}', jsonb_build_array($2::jsonb, $2::jsonb))
WHERE uuid = $1`, responseID, `{
		"interrupt_id":"interrupt-parallel-1",
		"available_actions":["approve","reject","edit","block_with_comment"]
	}`); err != nil {
		t.Fatal(err)
	}
	resume.HitlDecisions = []byte(`[
		{"interrupt_id":"interrupt-parallel-1","action":"approve"},
		{"interrupt_id":"interrupt-parallel-2","action":"block_with_comment","value":"retain for audit"}
	]`)
	if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate pending interrupts error=%v", err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET meta = jsonb_set(meta, '{hitl_interrupts}', $2::jsonb)
WHERE uuid = $1`, responseID, pending); err != nil {
		t.Fatal(err)
	}

	row, err := queries.ResumeCurrentAgentHITL(t.Context(), resume)
	if err != nil {
		t.Fatal(err)
	}
	if row.ResponseMessageID != responseID || row.ResponseMessageGroupID <= 0 {
		t.Fatalf("resume row=%+v", row)
	}
	if _, err := queries.ResumeCurrentAgentHITL(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed resume error=%v", err)
	}

	var isStreaming, hasSingularPending, hasPluralPending bool
	var taskID string
	var resolvedIDs []string
	if err := tx.QueryRow(t.Context(), `
SELECT response.is_streaming,
       response.task_id,
       response.meta ? 'hitl_interrupt',
       response.meta ? 'hitl_interrupts',
       ARRAY(
           SELECT jsonb_array_elements_text(
               COALESCE(response.meta -> 'resolved_hitl_interrupt_ids', '[]'::jsonb)
           )
       )
FROM chat_message_group AS response
WHERE response.uuid = $1`, responseID).Scan(
		&isStreaming, &taskID, &hasSingularPending, &hasPluralPending, &resolvedIDs,
	); err != nil {
		t.Fatal(err)
	}
	if !isStreaming || taskID != "execution-parallel-hitl-resumed" ||
		hasSingularPending || hasPluralPending ||
		!slices.Equal(resolvedIDs, []string{"interrupt-parallel-1", "interrupt-parallel-2"}) {
		t.Fatalf(
			"streaming=%v task=%q singular_pending=%v plural_pending=%v resolved=%v",
			isStreaming, taskID, hasSingularPending, hasPluralPending, resolvedIDs,
		)
	}
}

func TestPostgresCurrentAuthorizationContinuationConsumesExactDecisionSetAtomically(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000061"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000061",
		"40000000-0000-4000-8000-000000000061",
		"list SharePoint sites", "execution-authorization-paused",
	)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-authorization-1',
        'execution_generation', $2::text,
	        'authorization_requests', jsonb_build_array(
	            jsonb_build_object(
	                'interrupt_id', 'mcp_auth_sharepoint-1',
	                'tool_run_id', 'legacy-tool-run-sharepoint-1',
	                'tool_call_id', 'call-sharepoint-search-1',
	                'server_url', 'https://sharepoint.example.test',
                'toolkit_name', 'SharePoint'
            ),
            jsonb_build_object(
                'tool_run_id', 'tool-run-openapi-2',
                'server_url', 'https://openapi.example.test',
                'toolkit_name', 'OpenAPI'
            )
        )
    )
WHERE uuid = $1`, responseID, questionID); err != nil {
		t.Fatal(err)
	}

	resolve := sqlcgen.ResolveCurrentAuthorizationContinuationParams{
		ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
		ResponseMessageID: responseID, AuthorizationRequestID: "mcp_auth_sharepoint-1",
	}
	if _, err := queries.ResolveCurrentAuthorizationContinuation(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ambiguous sibling authorization resolve error=%v", err)
	}
	resolve.AuthorizationRequestID = ""
	resolved, err := queries.ResolveCurrentAuthorizationContinuation(t.Context(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContinuationKind != "application" ||
		resolved.ThreadID != "thread-authorization-1" ||
		resolved.ExecutionGeneration != questionID ||
		resolved.UserInput != "list SharePoint sites" ||
		!json.Valid([]byte(resolved.AuthorizationRequestsJson)) {
		t.Fatalf("resolved=%+v", resolved)
	}
	resolve.ActorUserID = 12
	if _, err := queries.ResolveCurrentAuthorizationContinuation(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong actor resolve error=%v", err)
	}
	resolve.ActorUserID = 11
	resolve.AuthorizationRequestID = "tool-run-missing"
	if _, err := queries.ResolveCurrentAuthorizationContinuation(t.Context(), resolve); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong request resolve error=%v", err)
	}

	resume := sqlcgen.ResumeCurrentAgentAuthorizationParams{
		ActorUserID: 12, ProjectID: 1, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41, ContinuationKind: "application",
		ConversationUuid: conversationID, QuestionID: mustCurrentPGUUID(t, questionID),
		ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-authorization-1",
		HitlDecisions: []byte(`[
  {"interrupt_id":"mcp_auth_sharepoint-1","tool_call_id":"call-sharepoint-search-1","guardrail_type":"mcp_auth","action":"authorize"},
  {"interrupt_id":"tool-run-openapi-2","guardrail_type":"mcp_auth","action":"skip"}
]`),
		ExecutionID: "execution-authorization-resumed",
	}
	if _, err := queries.ResumeCurrentAgentAuthorization(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong actor resume error=%v", err)
	}
	resume.ActorUserID = 11
	resume.HitlDecisions = bytes.Replace(resume.HitlDecisions, []byte(`"authorize"`), []byte(`"approve"`), 1)
	if _, err := queries.ResumeCurrentAgentAuthorization(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unsupported action resume error=%v", err)
	}
	resume.HitlDecisions = bytes.Replace(resume.HitlDecisions, []byte(`"approve"`), []byte(`"authorize"`), 1)
	row, err := queries.ResumeCurrentAgentAuthorization(t.Context(), resume)
	if err != nil {
		t.Fatal(err)
	}
	if row.ResponseMessageID != responseID || row.ResponseMessageGroupID <= 0 {
		t.Fatalf("resume row=%+v", row)
	}
	if _, err := queries.ResumeCurrentAgentAuthorization(t.Context(), resume); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed resume error=%v", err)
	}

	var isStreaming, hasPending bool
	var taskID string
	var resolvedIDs []string
	if err := tx.QueryRow(t.Context(), `
SELECT response.is_streaming,
       response.task_id,
       response.meta ? 'authorization_requests',
       ARRAY(
           SELECT jsonb_array_elements_text(
               COALESCE(response.meta -> 'resolved_authorization_request_ids', '[]'::jsonb)
           )
       )
FROM chat_message_group AS response
WHERE response.uuid = $1`, responseID).Scan(
		&isStreaming, &taskID, &hasPending, &resolvedIDs,
	); err != nil {
		t.Fatal(err)
	}
	if !isStreaming || taskID != "execution-authorization-resumed" || hasPending ||
		len(resolvedIDs) != 2 || resolvedIDs[0] != "mcp_auth_sharepoint-1" ||
		resolvedIDs[1] != "tool-run-openapi-2" {
		t.Fatalf("streaming=%v task=%q pending=%v resolved=%v",
			isStreaming, taskID, hasPending, resolvedIDs)
	}
}

// TestPostgresCurrentAuthorizationContinuationClassifiesNonArrayRequests holds
// the authorization pair to the same contract every other refusal here obeys:
// a response whose `meta.authorization_requests` is not an array must come back
// as pgx.ErrNoRows — a classification the caller answers 422 — and never as a
// database error, which it can only answer 500.
//
// The distinction is not theoretical for these two queries. Their type test
// (`AND jsonb_typeof(response.meta -> 'authorization_requests') = 'array'`) is
// a SIBLING qual, and PostgreSQL costs and reorders the quals of an AND
// (order_qual_clauses); on PostgreSQL 16 the length test was already being
// evaluated first, so each of the three shapes below raised SQLSTATE 22023
// ("cannot get array length of a scalar" / "of a non-array") straight through
// this path. Every array function in both queries now reads a CASE-materialized
// value instead, so the type test decides the answer rather than deciding
// whether the statement survives.
func TestPostgresCurrentAuthorizationContinuationClassifiesNonArrayRequests(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000062"
	responseID := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000062",
		"40000000-0000-4000-8000-000000000062",
		"list SharePoint sites", "execution-authorization-nonarray",
	)

	for _, shape := range []struct {
		name  string
		value string
	}{
		{name: "json null", value: `null`},
		{name: "scalar string", value: `"mcp_auth_sharepoint-1"`},
		{name: "object", value: `{"interrupt_id": "mcp_auth_sharepoint-1"}`},
		// An empty array is a valid array and still has to be refused, by the
		// BETWEEN rather than by the type test — the branch the CASE's ELSE arm
		// lands on, so this pins that the guard refuses rather than admits.
		{name: "empty array", value: `[]`},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-authorization-nonarray',
        'execution_generation', $2::text,
        'authorization_requests', $3::jsonb
    )
WHERE uuid = $1`, responseID, questionID, shape.value); err != nil {
				t.Fatal(err)
			}

			resolve := sqlcgen.ResolveCurrentAuthorizationContinuationParams{
				ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
				ResponseMessageID: responseID, AuthorizationRequestID: "",
			}
			if _, err := queries.ResolveCurrentAuthorizationContinuation(
				t.Context(), resolve,
			); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("resolve error=%v", err)
			}
			// The single-request selector reaches a second array_length and an
			// element scan, so it is exercised separately.
			resolve.AuthorizationRequestID = "mcp_auth_sharepoint-1"
			if _, err := queries.ResolveCurrentAuthorizationContinuation(
				t.Context(), resolve,
			); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("targeted resolve error=%v", err)
			}

			resume := sqlcgen.ResumeCurrentAgentAuthorizationParams{
				ActorUserID: 11, ProjectID: 1, TargetParticipantID: 21,
				ApplicationID: 31, ApplicationVersionID: 41,
				ContinuationKind:    "application",
				ConversationUuid:    conversationID,
				QuestionID:          mustCurrentPGUUID(t, questionID),
				ResponseMessageID:   responseID,
				ExecutionGeneration: questionID,
				ThreadID:            "thread-authorization-nonarray",
				HitlDecisions: []byte(`[
  {"interrupt_id":"mcp_auth_sharepoint-1","tool_call_id":"call-sharepoint-search-1","guardrail_type":"mcp_auth","action":"authorize"}
]`),
				ExecutionID: "execution-authorization-nonarray-resumed",
			}
			if _, err := queries.ResumeCurrentAgentAuthorization(
				t.Context(), resume,
			); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("resume error=%v", err)
			}
		})
	}

	// A real pending array still resolves: the guard refused the shapes above
	// on their shape, not by refusing everything.
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-authorization-nonarray',
        'execution_generation', $2::text,
        'authorization_requests', jsonb_build_array(
            jsonb_build_object(
                'interrupt_id', 'mcp_auth_sharepoint-1',
                'tool_call_id', 'call-sharepoint-search-1',
                'server_url', 'https://sharepoint.example.test',
                'toolkit_name', 'SharePoint'
            )
        )
    )
WHERE uuid = $1`, responseID, questionID); err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.ResolveCurrentAuthorizationContinuation(
		t.Context(),
		sqlcgen.ResolveCurrentAuthorizationContinuationParams{
			ActorUserID: 11, ProjectID: 1, ConversationUuid: conversationID,
			ResponseMessageID: responseID, AuthorizationRequestID: "",
		},
	)
	if err != nil {
		t.Fatalf("array authorization request remained refused: %v", err)
	}
	if !json.Valid([]byte(resolved.AuthorizationRequestsJson)) {
		t.Fatalf("resolved=%+v", resolved)
	}
}

func insertPostgresSupportContext(
	t *testing.T,
	tx pgx.Tx,
	questionID string,
	userID,
	projectID int64,
) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
WITH context_item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(), 'context_message', -1, '{}'::jsonb, message_group.id
    FROM chat_message_group AS message_group
    WHERE message_group.uuid = $1::uuid
    RETURNING id
)
INSERT INTO chat_messages_context (id, context_data, context_type)
SELECT context_item.id,
       jsonb_build_object('user_id', $2::bigint, 'project_id', $3::bigint),
       'support_assistant_context'
FROM context_item`, questionID, userID, projectID); err != nil {
		t.Fatal(err)
	}
}

func mustCurrentPGUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	result, err := currentPGUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func insertPostgresCurrentApplicationTurn(
	t *testing.T,
	queries *sqlcgen.Queries,
	conversationID pgtype.UUID,
	questionIDRaw,
	questionItemIDRaw,
	responseIDRaw,
	input,
	executionID string,
) pgtype.UUID {
	t.Helper()
	questionID, _ := currentPGUUID(questionIDRaw)
	questionItemID, _ := currentPGUUID(questionItemIDRaw)
	responseID, _ := currentPGUUID(responseIDRaw)
	row, err := queries.InsertCurrentApplicationTurn(
		t.Context(),
		sqlcgen.InsertCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			ApplicationVersionID: 41, ApplicationID: 31,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: questionID, QuestionMeta: []byte(`{}`),
			QuestionItemID: questionItemID, UserInput: input,
			ResponseMessageID: responseID, ExecutionGeneration: questionIDRaw,
			ExecutionID: executionID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return row.ResponseMessageID
}

func insertPostgresCurrentAdhocTurn(
	t *testing.T,
	queries *sqlcgen.Queries,
	conversationID pgtype.UUID,
	questionIDRaw,
	questionItemIDRaw,
	responseIDRaw,
	input,
	executionID string,
) pgtype.UUID {
	t.Helper()
	row, err := queries.InsertCurrentAdhocTurn(
		t.Context(),
		sqlcgen.InsertCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23,
			ConversationUuid: conversationID, ProjectID: 1,
			QuestionID: mustCurrentPGUUID(t, questionIDRaw), QuestionMeta: []byte(`{}`),
			QuestionItemID: mustCurrentPGUUID(t, questionItemIDRaw), UserInput: input,
			ResponseMessageID:   mustCurrentPGUUID(t, responseIDRaw),
			ExecutionGeneration: questionIDRaw, ExecutionID: executionID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return row.ResponseMessageID
}

func seedCurrentAgentContinuationSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
CREATE TABLE p_1.applications (
    id SERIAL PRIMARY KEY, name VARCHAR(128) NOT NULL,
    description VARCHAR(2304), owner_id INTEGER NOT NULL
);
ALTER TABLE p_1.application_versions
    ADD COLUMN application_id INTEGER NOT NULL,
    ADD COLUMN name VARCHAR(128) NOT NULL,
    ADD COLUMN status VARCHAR NOT NULL,
    ADD COLUMN author_id INTEGER NOT NULL,
    ADD COLUMN uuid UUID NOT NULL UNIQUE,
    ADD COLUMN created_at TIMESTAMP NOT NULL DEFAULT now(),
    ADD COLUMN instructions VARCHAR,
    ADD COLUMN conversation_starters JSON NOT NULL DEFAULT '[]'::json,
    ADD COLUMN welcome_message VARCHAR NOT NULL DEFAULT '',
    ADD COLUMN agent_type VARCHAR NOT NULL,
    ADD COLUMN meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN pipeline_settings JSONB NOT NULL DEFAULT '{}'::jsonb;
CREATE TABLE p_1.elitea_tools (
    id SERIAL PRIMARY KEY, created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP,
    type VARCHAR NOT NULL, name VARCHAR(128), description VARCHAR(1024),
    settings JSONB NOT NULL, author_id INTEGER NOT NULL, meta JSONB NOT NULL
);
CREATE TABLE p_1.entity_tool_mapping (
    id SERIAL PRIMARY KEY, tool_id INTEGER NOT NULL, entity_id INTEGER NOT NULL,
    entity_version_id INTEGER NOT NULL, entity_type VARCHAR NOT NULL,
    selected_tools JSONB
);
-- The version's AUTHORED variables. This is the store the shared
-- application_version_details_json projection reads its variables key from, so
-- a seed without it makes every query that carries that projection fail with
-- 42P01 rather than answer a wrong list. Same shape as
-- internal/infra/db/migrations/001_initial.sql, which is where the real tenant
-- schema gets it.
CREATE TABLE p_1.application_variables (
    id SERIAL PRIMARY KEY,
    application_version_id INTEGER NOT NULL REFERENCES p_1.application_versions(id) ON DELETE CASCADE,
    name VARCHAR NOT NULL,
    value VARCHAR,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    CONSTRAINT _application_version_variable_name_uc UNIQUE (application_version_id, name)
);
-- skills / skill_versions are created by newMigratedPostgresIntegrationPool,
-- which every caller of this seed runs first (#249 put them there so the
-- tenant migration history has something to alter). Re-creating them here
-- would be a duplicate relation.
CREATE TABLE p_1.entity_skill_mapping (
    id SERIAL PRIMARY KEY, entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL, skill_id INTEGER NOT NULL REFERENCES p_1.skills(id),
    skill_version_id INTEGER REFERENCES p_1.skill_versions(id)
);
-- The eight chat tables are created by newMigratedPostgresIntegrationPool, for
-- the same reason skills / skill_versions above are: tenant/0123 took ownership
-- of them (#287) so a pylon-free deployment has them at all, and this seed's
-- callers run that migration first. Re-creating them here would be a duplicate
-- relation.
INSERT INTO p_1.applications (id, name, description, owner_id) VALUES
    (31, 'Parent Agent', 'Parent agent fixture', 11);
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES (
    41, 31, 'base', 'draft', 11, '80000000-0000-4000-8000-000000000031',
    '{"model_name":"test"}'::jsonb, 'Be concise', '[]'::json, '', 'agent',
    '{}'::jsonb, '{}'::jsonb
);
INSERT INTO p_1.chat_conversations (
    id, uuid, name, author_id, source, meta
) VALUES (
    1, '10000000-0000-4000-8000-000000000031', 'continuation', 11, 'agent',
    '{}'::jsonb
);
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta) VALUES
    (20, '90000000-0000-4000-8000-000000000031', 'user', '{"id":11}'::jsonb),
    (21, 'a0000000-0000-4000-8000-000000000031', 'application', '{"id":31,"project_id":1}'::jsonb);
INSERT INTO p_1.chat_participant_mapping (
    conversation_id, participant_id, entity_settings
) VALUES
    (1, 20, '{}'::jsonb),
    (1, 21, '{"version_id":41,"variables":[]}'::jsonb);
INSERT INTO p_1.elitea_tools (
    id, type, name, description, settings, author_id, meta
) VALUES (
    51, 'aha', 'product', 'Aha product access',
    '{"selected_tools":["list_products"]}'::jsonb, 11, '{}'::jsonb
);
INSERT INTO p_1.skills (id, name, description) VALUES
    (61, 'release-proof', 'Release proof skill');
INSERT INTO p_1.skill_versions (id, skill_id, name, instructions, meta) VALUES
    (71, 61, 'base', 'Return RELEASE-PROOF', '{"icon_meta":{"icon":"proof"}}'::jsonb);
INSERT INTO p_1.entity_skill_mapping (
    id, entity_version_id, entity_type, skill_id, skill_version_id
) VALUES
    (81, 41, 'agent', 61, 71);
INSERT INTO p_1.chat_conversations (
    id, uuid, name, author_id, source, instructions, meta
) VALUES (
    2, '10000000-0000-4000-8000-000000000032', 'ad-hoc continuation', 11, 'elitea',
    'Conversation instructions',
    '{"default_instructions":"User defaults","persona":"qa","steps_limit":12,"internal_tools":["internal_mcp"]}'::jsonb
);
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta, meta) VALUES
    (23, 'a0000000-0000-4000-8000-000000000032', 'dummy', '{}'::jsonb, '{"name":"EliteA"}'::json),
    (24, 'b0000000-0000-4000-8000-000000000032', 'toolkit', '{"id":51,"project_id":1}'::jsonb, '{"name":"product"}'::json),
    (30, 'c0000000-0000-4000-8000-000000000032', 'application', '{"id":31,"project_id":1}'::jsonb,
     '{"name":"leaf-agent","description":"Read-only child agent","agent_type":"agent"}'::json);
INSERT INTO p_1.chat_participant_mapping (
    conversation_id, participant_id, entity_settings
) VALUES
    (2, 20, '{}'::jsonb),
    (2, 23, '{"llm_settings":{"model_name":"saved","model_project_id":1,"max_tokens":1024}}'::jsonb),
    (2, 24, '{}'::jsonb),
    (2, 30, '{"version_id":41,"variables":[],"agent_type":"agent"}'::jsonb);`); err != nil {
		t.Fatal(err)
	}
}
