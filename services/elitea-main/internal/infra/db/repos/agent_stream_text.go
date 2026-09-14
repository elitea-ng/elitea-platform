package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5"
)

const currentAgentProvisionalTextMaxBytes = defaultMaxProgressBytes

type currentAgentTextProjector interface {
	projectAgentTextDelta(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error
}

type noopCurrentAgentTextProjector struct{}

func (noopCurrentAgentTextProjector) projectAgentTextDelta(
	context.Context,
	sqlExecutor,
	int64,
	outputapp.NodeEventFrame,
) error {
	return nil
}

// postgresCurrentAgentTextProjector stores visible model chunks in the current
// response group. A terminal success replaces this provisional text.
type postgresCurrentAgentTextProjector struct{}

type currentAgentTextDelta struct {
	streamID            string
	messageID           string
	executionGeneration string
	sioEvent            string
	content             string
}

type currentAgentTextOwner struct {
	ParentAgentName string            `json:"parent_agent_name"`
	ParentAgentPath []json.RawMessage `json:"parent_agent_path"`
}

func (owner currentAgentTextOwner) isChild() bool {
	return strings.TrimSpace(owner.ParentAgentName) != "" || len(owner.ParentAgentPath) > 0
}

func (postgresCurrentAgentTextProjector) projectAgentTextDelta(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	frame outputapp.NodeEventFrame,
) error {
	delta, recognized, err := decodeCurrentAgentTextDelta(frame.BrowserData)
	if err != nil || !recognized || delta.content == "" {
		return err
	}
	projectDatabaseID, ok := currentAgentDatabaseID(projectID)
	if !ok {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(
		ctx,
		tx,
		frame.Fence.ExecutionID,
		frame.Fence.Generation,
		projectDatabaseID,
	)
	if err != nil {
		return err
	}
	if !bound {
		return errors.New("current agent text binding is unavailable")
	}
	if delta.streamID != binding.streamID || delta.messageID != binding.messageID ||
		delta.executionGeneration != binding.executionGeneration ||
		delta.sioEvent != binding.sioEvent {
		return errors.New("current agent text delta conflicts with immutable admission")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	messageGroupID, err := lockCurrentAgentMessageGroup(
		ctx,
		tx,
		schema,
		frame.Fence.ExecutionID,
		binding,
	)
	if err != nil {
		return err
	}
	return appendCurrentAgentProvisionalText(
		ctx,
		tx,
		schema,
		messageGroupID,
		frame.Fence.ExecutionID,
		frame.Fence.Generation,
		delta.content,
	)
}

func decodeCurrentAgentTextDelta(raw json.RawMessage) (currentAgentTextDelta, bool, error) {
	var event struct {
		Type                string          `json:"type"`
		StreamID            string          `json:"stream_id"`
		MessageID           string          `json:"message_id"`
		ExecutionGeneration string          `json:"execution_generation"`
		SIOEvent            string          `json:"sio_event"`
		Content             json.RawMessage `json:"content"`
		ResponseMetadata    struct {
			currentAgentTextOwner
			Metadata currentAgentTextOwner `json:"metadata"`
			ToolMeta struct {
				Metadata currentAgentTextOwner `json:"metadata"`
			} `json:"tool_meta"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return currentAgentTextDelta{}, false, errors.New("decode current agent text event")
	}
	if event.Type != "agent_llm_chunk" {
		return currentAgentTextDelta{}, false, nil
	}
	owner := event.ResponseMetadata
	if owner.isChild() || owner.Metadata.isChild() || owner.ToolMeta.Metadata.isChild() {
		// The trace projector stores child output. Do not copy it into the parent answer.
		return currentAgentTextDelta{}, false, nil
	}
	if !validCurrentAgentCorrelation(event.StreamID) ||
		!validCurrentAgentCorrelation(event.MessageID) ||
		!validCurrentAgentCorrelation(event.ExecutionGeneration) ||
		(event.SIOEvent != "chat_predict" && event.SIOEvent != "chat_continue_predict") {
		return currentAgentTextDelta{}, false, errors.New("current agent text correlation is invalid")
	}
	if string(event.Content) == "null" {
		return currentAgentTextDelta{
			streamID:            event.StreamID,
			messageID:           event.MessageID,
			executionGeneration: event.ExecutionGeneration,
			sioEvent:            event.SIOEvent,
		}, true, nil
	}
	var content string
	if err := json.Unmarshal(event.Content, &content); err != nil ||
		!utf8.ValidString(content) || strings.ContainsRune(content, '\x00') ||
		len(content) > outputapp.MaxNodeEventOutputBytes {
		return currentAgentTextDelta{}, false, errors.New("current agent text content is invalid")
	}
	return currentAgentTextDelta{
		streamID:            event.StreamID,
		messageID:           event.MessageID,
		executionGeneration: event.ExecutionGeneration,
		sioEvent:            event.SIOEvent,
		content:             content,
	}, true, nil
}

func appendCurrentAgentProvisionalText(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	messageGroupID int64,
	executionID string,
	generation uint64,
	content string,
) error {
	var itemID, currentBytes int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT item.id, octet_length(text_item.content)
FROM %s AS item
JOIN %s AS text_item ON text_item.id = item.id
WHERE item.message_group_id = $1
  AND item.item_type = 'text_message'
  AND item.meta ->> 'runtime_stream_execution_id' = $2
  AND item.meta ->> 'runtime_stream_generation' = $3
ORDER BY item.id
LIMIT 1
FOR UPDATE OF item, text_item`,
		schema+".chat_message_items",
		schema+".chat_messages_text",
	), messageGroupID, executionID, fmt.Sprint(generation)).Scan(&itemID, &currentBytes)
	if err == nil {
		if currentBytes+int64(len(content)) > currentAgentProvisionalTextMaxBytes {
			return outputapp.ErrInvalidNodeEventOutput
		}
		result, updateErr := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s
SET content = content || $2::text
WHERE id = $1`, schema+".chat_messages_text"), itemID, content)
		if updateErr != nil {
			return fmt.Errorf("append current agent provisional text: %w", updateErr)
		}
		if result.RowsAffected() != 1 {
			return errors.New("current agent provisional text is unavailable")
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load current agent provisional text: %w", err)
	}
	result, err := tx.Exec(ctx, fmt.Sprintf(`
WITH next_order AS MATERIALIZED (
    SELECT COALESCE(max(order_index), -1) + 1 AS order_index
    FROM %s
    WHERE message_group_id = $1
), inserted_item AS (
    INSERT INTO %s (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(),
           'text_message',
           next_order.order_index,
           jsonb_build_object(
               'runtime_stream_execution_id', $2::text,
               'runtime_stream_generation', $3::text,
               'runtime_stream_provisional', TRUE
           ),
           $1
    FROM next_order
    RETURNING id
)
INSERT INTO %s (id, content)
SELECT inserted_item.id, $4::text
FROM inserted_item`,
		schema+".chat_message_items",
		schema+".chat_message_items",
		schema+".chat_messages_text",
	), messageGroupID, executionID, fmt.Sprint(generation), content)
	if err != nil {
		return fmt.Errorf("insert current agent provisional text: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("current agent provisional text was not inserted")
	}
	return nil
}
