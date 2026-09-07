package repos

import (
	"encoding/json"
	"fmt"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresCurrentAgentTextSurvivesAWorkerFailure(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	const (
		conversationID   = "10000000-0000-4000-8000-000000000021"
		responseID       = "20000000-0000-4000-8000-000000000021"
		clientGeneration = "30000000-0000-4000-8000-000000000021"
	)
	admitted := admitPostgresAgentExecution(
		t,
		pool,
		conversationID,
		responseID,
		clientGeneration,
	)
	seedCurrentAgentResponseGroup(
		t,
		pool,
		conversationID,
		responseID,
		clientGeneration,
		admitted.ExecutionID,
	)

	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	projector := postgresCurrentAgentTextProjector{}
	for _, content := range []string{"durable ", "child result", "partial answer"} {
		frame := currentAgentTextFrame(
			admitted.ExecutionID,
			conversationID,
			responseID,
			clientGeneration,
			content,
		)
		if content == "child result" {
			var event map[string]any
			if err := json.Unmarshal(frame.BrowserData, &event); err != nil {
				t.Fatal(err)
			}
			event["response_metadata"] = map[string]any{
				"parent_agent_name": "Name Resolver",
				"parent_agent_path": []any{map[string]any{"name": "Name Resolver", "call_id": "name-call"}},
			}
			frame.BrowserData, err = json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := store.WithinTx(
			t.Context(),
			pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
			func(tx sqlExecutor) error {
				return projector.projectAgentTextDelta(t.Context(), tx, 1, frame)
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	err = store.WithinTx(
		t.Context(),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			return persistCurrentAgentRuntimeTerminal(
				t.Context(),
				tx,
				1,
				executiondomain.AgentApplicationCapability,
				outputRecord{ExecutionID: admitted.ExecutionID, Generation: 1},
				"INTERNAL",
				"The runtime operation failed.",
			)
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var content, metadata string
	var isStreaming bool
	if err := pool.QueryRow(t.Context(), `
SELECT text_item.content, message_group.meta::text, message_group.is_streaming
FROM p_1.chat_message_group AS message_group
JOIN p_1.chat_message_items AS item
  ON item.message_group_id = message_group.id
JOIN p_1.chat_messages_text AS text_item ON text_item.id = item.id
WHERE message_group.uuid::text = $1
  AND item.meta ->> 'runtime_stream_execution_id' = $2`,
		responseID,
		admitted.ExecutionID,
	).Scan(&content, &metadata, &isStreaming); err != nil {
		t.Fatal(err)
	}
	if content != "durable partial answer" || isStreaming ||
		!containsAll(metadata, `"is_error": true`, "The runtime operation failed.") {
		t.Fatalf("content=%q streaming=%t metadata=%s", content, isStreaming, metadata)
	}

	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	responseGroupID := currentResponseGroupID(t, pool, responseID)
	err = projects.WithinProjectTx(
		t.Context(),
		1,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			writer := tx.(currentAgentTerminalWriter)
			return writer.DeleteCurrentAgentProvisionalText(
				t.Context(),
				sqlcgen.DeleteCurrentAgentProvisionalTextParams{
					MessageGroupID: responseGroupID,
					ExecutionID:    admitted.ExecutionID,
					Generation:     1,
				},
			)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
FROM p_1.chat_message_items AS item
JOIN p_1.chat_message_group AS message_group
  ON message_group.id = item.message_group_id
WHERE message_group.uuid::text = $1
  AND item.meta ->> 'runtime_stream_execution_id' = $2`,
		responseID,
		admitted.ExecutionID,
	).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("provisional text rows = %d", remaining)
	}
}

func currentAgentTextFrame(
	executionID,
	conversationID,
	responseID,
	clientGeneration,
	content string,
) outputapp.NodeEventFrame {
	return outputapp.NodeEventFrame{
		Fence: runtimedomain.Fence{ExecutionID: executionID, Generation: 1},
		BrowserData: []byte(fmt.Sprintf(`{
  "type":"agent_llm_chunk",
  "stream_id":%q,
  "message_id":%q,
  "execution_generation":%q,
  "sio_event":"chat_predict",
  "content":%q
}`, conversationID, responseID, clientGeneration, content)),
	}
}

func currentResponseGroupID(t *testing.T, pool *pgxpool.Pool, responseID string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
SELECT id FROM p_1.chat_message_group WHERE uuid::text = $1`, responseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
