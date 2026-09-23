package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5"
)

// Result assembly uses the existing fenced provisional item. The first chunk
// replaces streamed intermediate turns. Success still requires a terminal ref.
func appendCurrentAgentResultChunk(ctx context.Context, tx sqlExecutor, schema string, groupID int64, executionID string, generation uint64, fragment string, metadata json.RawMessage) error {
	var itemID int64
	var content string
	var priorMetadata []byte
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT item.id, text_item.content, item.meta -> 'runtime_result_chunk_v1'
FROM %s item JOIN %s text_item ON text_item.id = item.id
WHERE item.message_group_id = $1
  AND item.meta ->> 'runtime_stream_execution_id' = $2
  AND item.meta ->> 'runtime_stream_generation' = $3
ORDER BY item.id LIMIT 1 FOR UPDATE OF item, text_item`, schema+".chat_message_items", schema+".chat_messages_text"), groupID, executionID, fmt.Sprint(generation)).Scan(&itemID, &content, &priorMetadata)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load result assembly: %w", err)
	}
	previous := map[string]any{}
	if len(priorMetadata) != 0 && string(priorMetadata) != "null" {
		previous["content"] = content
		previous["result_chunk_v1"] = json.RawMessage(priorMetadata)
	}
	merged, mergeErr := mergeAgentTextChunk(previous, map[string]any{"content": fragment, "result_chunk_v1": metadata}, "content", "result_chunk_v1", maxAgentModelOutputBytes, false)
	if mergeErr != nil {
		return mergeErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := appendCurrentAgentProvisionalText(ctx, tx, schema, groupID, executionID, generation, ""); err != nil {
			return err
		}
	}
	chunk, err := json.Marshal(merged["result_chunk_v1"])
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, fmt.Sprintf(`
WITH updated AS (
    UPDATE %s SET meta = meta || jsonb_build_object('runtime_result_chunk_v1', $4::jsonb)
    WHERE message_group_id = $1
      AND meta ->> 'runtime_stream_execution_id' = $2
      AND meta ->> 'runtime_stream_generation' = $3
    RETURNING id
)
UPDATE %s SET content = $5 WHERE id IN (SELECT id FROM updated)`, schema+".chat_message_items", schema+".chat_messages_text"), groupID, executionID, fmt.Sprint(generation), string(chunk), merged["content"])
	if err != nil {
		return fmt.Errorf("persist result assembly: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("result assembly item is unavailable")
	}
	return nil
}

func resolveCurrentAgentResultContent(ctx context.Context, tx sqlExecutor, expected outputapp.ExpectedAgentExecution, metadata json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(metadata, &fields) != nil {
		return "", outputapp.ErrAgentExecutionResultMismatch
	}
	raw := fields["result_ref_v1"]
	var ref struct {
		Total  int    `json:"total_bytes"`
		Digest string `json:"sha256"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ref) != nil || ref.Total <= 0 || ref.Total > maxAgentModelOutputBytes {
		return "", outputapp.ErrAgentExecutionResultMismatch
	}
	projectID, err := parseProjectID(expected.ProjectionProjectID)
	if err != nil {
		return "", err
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return "", err
	}
	var text string
	var chunkJSON []byte
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT text_item.content, item.meta -> 'runtime_result_chunk_v1'
FROM %s item JOIN %s text_item ON text_item.id = item.id
JOIN %s message_group ON message_group.id = item.message_group_id
WHERE message_group.uuid::text = $1
  AND item.meta ->> 'runtime_stream_execution_id' = $2
  AND item.meta ->> 'runtime_stream_generation' = $3
ORDER BY item.id LIMIT 1`, schema+".chat_message_items", schema+".chat_messages_text", schema+".chat_message_group"), expected.ClientMessageID, expected.ExecutionID, fmt.Sprint(expected.Generation)).Scan(&text, &chunkJSON)
	if err != nil {
		return "", fmt.Errorf("load completed result assembly: %w", err)
	}
	var chunk agentToolOutputChunk
	sum := sha256.Sum256([]byte(text))
	if json.Unmarshal(chunkJSON, &chunk) != nil || !chunk.Final || chunk.Total != ref.Total || chunk.Digest != ref.Digest || len(text) != ref.Total || hex.EncodeToString(sum[:]) != ref.Digest {
		return "", outputapp.ErrAgentExecutionResultMismatch
	}
	return text, nil
}
