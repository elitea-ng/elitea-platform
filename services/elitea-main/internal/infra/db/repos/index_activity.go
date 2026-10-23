package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5"
)

const (
	currentActivityMaxTextBytes = 48 * 1024
	currentActivityMaxAttrBytes = 8 * 1024
	currentActivityMaxNameBytes = 2 * 1024
)

type currentIndexActivityProjector interface {
	projectNodeEvent(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error
	projectTerminal(context.Context, sqlExecutor, int64, currentIndexActivityTerminal) error
}

type currentIndexActivityTerminal struct {
	ExecutionID string
	Generation  uint64
	OccurredAt  time.Time
	Message     string
	IsError     bool
}

func currentIndexActivityCancellation(record outputRecord) currentIndexActivityTerminal {
	return currentIndexActivityTerminal{
		ExecutionID: record.ExecutionID,
		Generation:  record.Generation,
		OccurredAt:  record.OccurredAt,
		Message:     "Execution was cancelled.",
		IsError:     true,
	}
}

type noopCurrentIndexActivityProjector struct{}

func (noopCurrentIndexActivityProjector) projectNodeEvent(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error {
	return nil
}

func (noopCurrentIndexActivityProjector) projectTerminal(context.Context, sqlExecutor, int64, currentIndexActivityTerminal) error {
	return nil
}

type postgresCurrentIndexActivityProjector struct {
	// A positive schema capability is immutable for the lifetime of this
	// projector. Negative results are deliberately not cached so an in-progress
	// tenant migration can become eligible without a process restart.
	readySchemas sync.Map
}

type currentIndexActivityNode struct {
	kind         string
	runID        string
	toolName     string
	text         string
	finishReason string
	stepType     string
	modelName    string
	startedAt    time.Time
	finishedAt   *time.Time
	isError      bool
	attrs        []byte
}

func (p *postgresCurrentIndexActivityProjector) projectNodeEvent(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	frame outputapp.NodeEventFrame,
) error {
	node, ok, err := currentIndexActivityNodeFromFrame(frame)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	schema, ready, err := p.currentSchemaReady(ctx, tx, projectID)
	if err != nil || !ready {
		return err
	}
	return projectCurrentIndexActivityNode(
		ctx, tx, schema, projectID, frame.Fence.ExecutionID,
		frame.Fence.Generation, frame.OccurredAt, node,
	)
}

func (p *postgresCurrentIndexActivityProjector) projectTerminal(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	terminal currentIndexActivityTerminal,
) error {
	if terminal.ExecutionID == "" || terminal.Generation == 0 ||
		terminal.OccurredAt.IsZero() || !validCurrentActivityText(terminal.Message, currentActivityMaxTextBytes) {
		return errors.New("invalid current index Activity terminal")
	}
	schema, ready, err := p.currentSchemaReady(ctx, tx, projectID)
	if err != nil || !ready {
		return err
	}
	meta, err := json.Marshal(map[string]any{
		"activity_kind": "indexing",
		"context":       map[string]any{"included": true, "priority": 1.0, "weight": 1.0},
		"is_error":      terminal.IsError,
	})
	if err != nil {
		return fmt.Errorf("encode current index Activity terminal metadata: %w", err)
	}
	var textID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
WITH admitted AS MATERIALIZED (
    SELECT j.actor_id,
           j.execution_id,
           i.toolkit_id,
           i.client_stream_id,
           i.client_message_id
    FROM elitea_runtime.execution_jobs AS j
    JOIN elitea_runtime.index_ingest_jobs AS i
      ON i.execution_id = j.execution_id
     AND i.generation = j.generation
     AND i.capability_id = j.capability_id
    WHERE j.execution_id = $1
      AND j.generation = $2
      AND j.capability_id = 'index.ingest.v1'
      AND j.resource_project_id = $3
      AND j.projection_project_id = $3
      AND i.client_stream_id IS NOT NULL
      AND i.client_message_id IS NOT NULL
),
target AS MATERIALIZED (
    SELECT conversation.id AS conversation_id,
           participant.id AS participant_id,
           admitted.client_message_id,
           admitted.execution_id
    FROM admitted
    JOIN %s AS conversation
      ON conversation.uuid::text = admitted.client_stream_id
     AND conversation.source = 'toolkit'
     AND conversation.author_id::text = admitted.actor_id
    JOIN %s AS mapping
      ON mapping.conversation_id = conversation.id
    JOIN %s AS participant
      ON participant.id = mapping.participant_id
     AND participant.entity_name = 'toolkit'
     AND participant.entity_meta->>'id' = admitted.toolkit_id::text
    WHERE admitted.client_message_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
),
inserted_group AS (
    INSERT INTO %s (
        uuid, author_participant_id, conversation_id, meta,
        is_streaming, created_at, updated_at, task_id
    )
    SELECT client_message_id::uuid,
           participant_id,
           conversation_id,
           '{"activity_kind":"indexing","context":{"included":true,"priority":1.0,"weight":1.0}}'::jsonb,
           TRUE,
           $4,
           $4,
           execution_id
    FROM target
    ON CONFLICT (uuid) DO NOTHING
    RETURNING id
),
selected_group AS MATERIALIZED (
    SELECT id
    FROM inserted_group
    UNION ALL
    SELECT message_group.id
    FROM %s AS message_group
    JOIN target
      ON message_group.uuid = target.client_message_id::uuid
     AND message_group.conversation_id = target.conversation_id
     AND message_group.author_participant_id = target.participant_id
     AND message_group.task_id = target.execution_id
     AND message_group.meta->>'activity_kind' = 'indexing'
    WHERE NOT EXISTS (SELECT 1 FROM inserted_group)
    LIMIT 1
),
target_group AS MATERIALIZED (
    UPDATE %s AS message_group
    SET is_streaming = FALSE,
        meta = $5::jsonb,
        updated_at = $4
    FROM selected_group
    WHERE message_group.id = selected_group.id
    RETURNING message_group.id, message_group.uuid
),
inserted_item AS (
    INSERT INTO %s (
        uuid, item_type, order_index, meta, created_at, updated_at, message_group_id
    )
    SELECT uuid, 'text_message', 0, '{}'::jsonb, $4, $4, id
    FROM target_group
    ON CONFLICT (uuid) DO NOTHING
    RETURNING id
),
selected_item AS MATERIALIZED (
    SELECT id
    FROM inserted_item
    UNION ALL
    SELECT item.id
    FROM %s AS item
    JOIN target_group AS target
      ON item.uuid = target.uuid
     AND item.message_group_id = target.id
     AND item.item_type = 'text_message'
     AND item.order_index = 0
    WHERE NOT EXISTS (SELECT 1 FROM inserted_item)
),
upserted_text AS (
    INSERT INTO %s (id, content)
    SELECT id, $6
    FROM selected_item
    ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content
    RETURNING id
)
SELECT id FROM upserted_text
LIMIT 1`,
		schema+".chat_conversations",
		schema+".chat_participant_mapping",
		schema+".chat_participants",
		schema+".chat_message_group",
		schema+".chat_message_group",
		schema+".chat_message_group",
		schema+".chat_message_items",
		schema+".chat_message_items",
		schema+".chat_messages_text",
	),
		terminal.ExecutionID,
		int64(terminal.Generation),
		projectID,
		terminal.OccurredAt.UTC(),
		meta,
		terminal.Message,
	).Scan(&textID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("project current index Activity terminal: %w", err)
	}
	if textID <= 0 {
		return errors.New("current index Activity terminal was not projected")
	}
	return nil
}

func (p *postgresCurrentIndexActivityProjector) currentSchemaReady(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
) (string, bool, error) {
	if tx == nil || projectID <= 0 {
		return "", false, errors.New("invalid current index Activity schema binding")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return "", false, err
	}
	if _, ok := p.readySchemas.Load(projectID); ok {
		return schema, true, nil
	}
	conversationTable := schema + ".chat_conversations"
	participantTable := schema + ".chat_participants"
	mappingTable := schema + ".chat_participant_mapping"
	groupTable := schema + ".chat_message_group"
	var currentTables bool
	err = tx.QueryRow(ctx, `
SELECT to_regclass($1) IS NOT NULL
   AND to_regclass($2) IS NOT NULL
   AND to_regclass($3) IS NOT NULL
   AND to_regclass($4) IS NOT NULL
   AND to_regclass($5) IS NOT NULL
   AND to_regclass($6) IS NOT NULL
   AND to_regclass($7) IS NOT NULL`,
		conversationTable,
		participantTable,
		mappingTable,
		groupTable,
		schema+".chat_message_trace_step",
		schema+".chat_message_items",
		schema+".chat_messages_text",
	).Scan(&currentTables)
	if err != nil {
		return "", false, fmt.Errorf("inspect current index Activity schema: %w", err)
	}
	if !currentTables {
		return schema, false, nil
	}
	p.readySchemas.Store(projectID, struct{}{})
	return schema, true, nil
}

func projectCurrentIndexActivityNode(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	projectID int64,
	executionID string,
	generation uint64,
	occurredAt time.Time,
	node currentIndexActivityNode,
) error {
	if executionID == "" || generation == 0 || occurredAt.IsZero() {
		return errors.New("invalid current index Activity node binding")
	}
	var traceID int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
WITH admitted AS MATERIALIZED (
    SELECT j.actor_id,
           j.execution_id,
           i.toolkit_id,
           i.client_stream_id,
           i.client_message_id
    FROM elitea_runtime.execution_jobs AS j
    JOIN elitea_runtime.index_ingest_jobs AS i
      ON i.execution_id = j.execution_id
     AND i.generation = j.generation
     AND i.capability_id = j.capability_id
    WHERE j.execution_id = $1
      AND j.generation = $2
      AND j.capability_id = 'index.ingest.v1'
      AND j.resource_project_id = $3
      AND j.projection_project_id = $3
      AND i.client_stream_id IS NOT NULL
      AND i.client_message_id IS NOT NULL
),
target AS MATERIALIZED (
    SELECT conversation.id AS conversation_id,
           participant.id AS participant_id,
           admitted.client_message_id,
           admitted.execution_id
    FROM admitted
    JOIN %s AS conversation
      ON conversation.uuid::text = admitted.client_stream_id
     AND conversation.source = 'toolkit'
     AND conversation.author_id::text = admitted.actor_id
    JOIN %s AS mapping
      ON mapping.conversation_id = conversation.id
    JOIN %s AS participant
      ON participant.id = mapping.participant_id
     AND participant.entity_name = 'toolkit'
     AND participant.entity_meta->>'id' = admitted.toolkit_id::text
    WHERE admitted.client_message_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
),
inserted AS (
    INSERT INTO %s (
        uuid, author_participant_id, conversation_id, meta,
        is_streaming, created_at, updated_at, task_id
    )
    SELECT client_message_id::uuid,
           participant_id,
           conversation_id,
           '{"activity_kind":"indexing","context":{"included":true,"priority":1.0,"weight":1.0}}'::jsonb,
           TRUE,
           $4,
           $4,
           execution_id
    FROM target
    ON CONFLICT (uuid) DO NOTHING
    RETURNING id
),
selected_group AS MATERIALIZED (
    SELECT id
    FROM inserted
    UNION ALL
    SELECT message_group.id
    FROM %s AS message_group
    JOIN target
      ON message_group.uuid = target.client_message_id::uuid
     AND message_group.conversation_id = target.conversation_id
     AND message_group.author_participant_id = target.participant_id
     AND message_group.task_id = target.execution_id
     AND message_group.meta->>'activity_kind' = 'indexing'
    WHERE NOT EXISTS (SELECT 1 FROM inserted)
    LIMIT 1
),
updated_trace AS (
    UPDATE %s AS trace
    SET tool_name = NULLIF($5, ''),
        model_name = NULLIF($16, ''),
        started_at = $6,
        finished_at = $7,
        is_error = $8,
        has_visible_content = $9,
        finish_reason = NULLIF($10, ''),
        step_type = NULLIF($11, ''),
        text = NULLIF($12, ''),
        attrs = CASE
            WHEN $13 = 'tool_call'
                THEN COALESCE(trace.attrs, '{}'::jsonb) || $14::jsonb
            ELSE $14::jsonb
        END
    WHERE trace.id = (
        SELECT candidate.id
        FROM %s AS candidate
        JOIN selected_group
          ON candidate.message_group_id = selected_group.id
        WHERE candidate.kind = $13
          AND candidate.run_id = $15
        ORDER BY candidate.id
        LIMIT 1
        FOR UPDATE
    )
    RETURNING trace.id
),
inserted_trace AS (
    INSERT INTO %s (
        message_group_id, kind, run_id, started_at, finished_at,
        is_error, has_visible_content, tool_name, finish_reason,
        step_type, text, model_name, attrs
    )
    SELECT selected_group.id, $13, $15, $6, $7, $8, $9, NULLIF($5, ''),
           NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''),
           NULLIF($16, ''), $14::jsonb
    FROM selected_group
    WHERE NOT EXISTS (SELECT 1 FROM updated_trace)
    RETURNING id
)
SELECT id FROM updated_trace
UNION ALL
SELECT id FROM inserted_trace
LIMIT 1`,
		schema+".chat_conversations",
		schema+".chat_participant_mapping",
		schema+".chat_participants",
		schema+".chat_message_group",
		schema+".chat_message_group",
		schema+".chat_message_trace_step",
		schema+".chat_message_trace_step",
		schema+".chat_message_trace_step",
	),
		executionID,
		int64(generation),
		projectID,
		occurredAt.UTC(),
		node.toolName,
		node.startedAt.UTC(),
		node.finishedAt,
		node.isError,
		node.kind == "tool_call" || node.text != "",
		node.finishReason,
		node.stepType,
		node.text,
		node.kind,
		node.attrs,
		node.runID,
		node.modelName,
	).Scan(&traceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("project current index Activity node: %w", err)
	}
	if traceID <= 0 {
		return errors.New("current index Activity trace was not projected")
	}
	return nil
}

func currentIndexActivityNodeFromFrame(frame outputapp.NodeEventFrame) (currentIndexActivityNode, bool, error) {
	var event struct {
		Type             string          `json:"type"`
		ResponseMetadata json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return currentIndexActivityNode{}, false, errors.New("decode current index Activity node event")
	}
	recognized := event.Type == "agent_tool_start" ||
		event.Type == "agent_tool_end" ||
		event.Type == "agent_tool_error" ||
		event.Type == "agent_thinking_step" ||
		event.Type == "agent_thinking_step_update"
	if !recognized {
		return currentIndexActivityNode{}, false, nil
	}
	if len(event.ResponseMetadata) == 0 {
		return currentIndexActivityNode{}, false, errors.New("current index Activity node metadata is required")
	}
	var metadata struct {
		RunID           string            `json:"run_id"`
		ToolRunID       string            `json:"tool_run_id"`
		ToolName        string            `json:"tool_name"`
		Message         string            `json:"message"`
		FinishReason    string            `json:"finish_reason"`
		TimestampStart  string            `json:"timestamp_start"`
		TimestampFinish string            `json:"timestamp_finish"`
		Datetime        string            `json:"datetime"`
		StepType        string            `json:"type"`
		ModelName       string            `json:"model_name"`
		Metadata        map[string]string `json:"metadata"`
	}
	if json.Unmarshal(event.ResponseMetadata, &metadata) != nil {
		return currentIndexActivityNode{}, false, errors.New("decode current index Activity node metadata")
	}
	runID := metadata.ToolRunID
	if runID == "" {
		runID = metadata.RunID
	}
	if !validCurrentActivityText(runID, currentActivityMaxNameBytes) ||
		!validOptionalCurrentActivityText(metadata.ToolName, currentActivityMaxNameBytes) ||
		!validOptionalCurrentActivityText(metadata.Message, currentActivityMaxTextBytes) ||
		!validOptionalCurrentActivityText(metadata.FinishReason, currentActivityMaxNameBytes) ||
		!validOptionalCurrentActivityText(metadata.StepType, currentActivityMaxNameBytes) ||
		!validOptionalCurrentActivityText(metadata.ModelName, currentActivityMaxNameBytes) {
		return currentIndexActivityNode{}, false, errors.New("current index Activity node metadata is invalid")
	}
	node := currentIndexActivityNode{
		runID:        runID,
		toolName:     metadata.ToolName,
		startedAt:    frame.OccurredAt.UTC(),
		finishReason: metadata.FinishReason,
	}
	switch event.Type {
	case "agent_tool_start":
		node.kind = "tool_call"
		node.startedAt = currentActivityTime(metadata.TimestampStart, node.startedAt)
	case "agent_tool_end", "agent_tool_error":
		node.kind = "tool_call"
		node.isError = event.Type == "agent_tool_error" || metadata.FinishReason == "error"
		node.startedAt = currentActivityTime(metadata.TimestampStart, node.startedAt)
		finished := currentActivityTime(metadata.TimestampFinish, frame.OccurredAt.UTC())
		node.finishedAt = &finished
	case "agent_thinking_step", "agent_thinking_step_update":
		node.kind = "thinking_step"
		node.stepType = metadata.StepType
		node.modelName = metadata.ModelName
		node.text = metadata.Message
		node.startedAt = currentActivityTime(
			metadata.TimestampStart,
			currentActivityTime(metadata.Datetime, node.startedAt),
		)
		finished := currentActivityTime(metadata.TimestampFinish, node.startedAt)
		node.finishedAt = &finished
	}
	attrs := map[string]any{}
	if node.kind == "thinking_step" {
		attrs["response_metadata"] = map[string]string{"tool_name": metadata.ToolName}
	} else if safe := currentActivityMetadata(metadata.Metadata); len(safe) != 0 {
		attrs["metadata"] = safe
	}
	node.attrs, _ = json.Marshal(attrs)
	if len(node.attrs) == 0 || len(node.attrs) > currentActivityMaxAttrBytes {
		return currentIndexActivityNode{}, false, errors.New("current index Activity node attrs are invalid")
	}
	return node, true, nil
}

func currentActivityMetadata(value map[string]string) map[string]string {
	safe := make(map[string]string, 3)
	for _, key := range []string{"display_name", "initiator", "tool_name"} {
		if item := value[key]; validOptionalCurrentActivityText(item, currentActivityMaxNameBytes) && item != "" {
			safe[key] = item
		}
	}
	return safe
}

func currentActivityTime(value string, fallback time.Time) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return fallback.UTC()
	}
	return parsed.UTC()
}

func currentProjectSchema(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", errors.New("invalid current project schema")
	}
	return pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10)}.Sanitize(), nil
}

func validCurrentActivityText(value string, maxBytes int) bool {
	return value != "" && validOptionalCurrentActivityText(value, maxBytes)
}

func validOptionalCurrentActivityText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
