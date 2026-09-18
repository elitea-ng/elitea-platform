package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresAgentResultChunksResumeAndFinalizeExactlyOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)
	const conversation = "10000000-0000-4000-8000-000000000071"
	const response = "20000000-0000-4000-8000-000000000071"
	const generation = "30000000-0000-4000-8000-000000000071"
	admitted := admitPostgresAgentExecution(t, pool, conversation, response, generation)
	seedCurrentAgentResponseGroup(t, pool, conversation, response, generation, admitted.ExecutionID)
	expected := outputapp.ExpectedAgentExecution{
		ProjectionProjectID: "1", ExecutionID: admitted.ExecutionID, Generation: 1,
		ClientStreamID: conversation, ClientMessageID: response, ClientExecutionGeneration: generation,
	}
	answer := strings.Repeat("Final answer with quotes: \"done\".\n", 25000)
	sum := sha256.Sum256([]byte(answer))
	digest := hex.EncodeToString(sum[:])
	metadata, _ := json.Marshal(map[string]any{"result_ref_v1": map[string]any{"total_bytes": len(answer), "sha256": digest}})
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	project := func(frame outputapp.NodeEventFrame) error {
		return store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
			return (postgresCurrentAgentTextProjector{}).projectAgentTextDelta(t.Context(), tx, 1, frame)
		})
	}
	if err := project(currentAgentTextFrame(admitted.ExecutionID, conversation, response, generation, "intermediate text must be replaced")); err != nil {
		t.Fatal(err)
	}
	resolve := func(binding outputapp.ExpectedAgentExecution) (string, error) {
		var content string
		err := store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
			var err error
			content, err = resolveCurrentAgentResultContent(t.Context(), tx, binding, metadata)
			return err
		})
		return content, err
	}
	var replay outputapp.NodeEventFrame
	for offset := 0; offset < len(answer); offset += 8192 {
		end := min(offset+8192, len(answer))
		frame := currentAgentTextFrame(admitted.ExecutionID, conversation, response, generation, answer[offset:end])
		var event map[string]any
		if err := json.Unmarshal(frame.BrowserData, &event); err != nil {
			t.Fatal(err)
		}
		event["type"] = "agent_result_chunk"
		event["response_metadata"] = map[string]any{"result_chunk_v1": agentToolOutputChunk{Offset: offset, Total: len(answer), Digest: digest, Final: end == len(answer)}}
		frame.BrowserData, _ = json.Marshal(event)
		if err := project(frame); err != nil {
			t.Fatal(err)
		}
		if offset == 0 {
			replay = frame
			if _, err := resolve(expected); err == nil {
				t.Fatal("incomplete result was accepted")
			}
			// A fresh pool and repository instance have no in-memory assembly.
			replacement, err := pgxpool.NewWithConfig(t.Context(), pool.Config().Copy())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(replacement.Close)
			store, err = newPostgresSharedStore(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := project(replay); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := project(replay); err != nil {
		t.Fatal(err)
	}
	actual, err := resolve(expected)
	if err != nil || actual != answer {
		t.Fatalf("assembled bytes=%d error=%v", len(actual), err)
	}
	wrong := expected
	wrong.Generation++
	if _, err := resolve(wrong); err == nil {
		t.Fatal("another generation read the result")
	}
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	err = projects.WithinProjectTx(t.Context(), 1, pgx.TxOptions{}, func(tx sqlExecutor) error {
		return persistCurrentAgentTerminal(t.Context(), tx, expected, currentAgentTerminal{FullMessage: &currentAgentFullMessage{
			ResultReference: true, ResponseMetadata: metadata, ThreadID: "worker-owned-thread",
			References: json.RawMessage(`[]`), InvokedSkills: json.RawMessage(`[]`),
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var content string
	var streaming bool
	err = pool.QueryRow(t.Context(), `SELECT count(*) OVER (), text_item.content, g.is_streaming
FROM p_1.chat_message_group g JOIN p_1.chat_message_items item ON item.message_group_id=g.id
JOIN p_1.chat_messages_text text_item ON text_item.id=item.id WHERE g.uuid::text=$1`, response).Scan(&count, &content, &streaming)
	if err != nil || count != 1 || content != answer || streaming {
		t.Fatalf("rows=%d bytes=%d streaming=%t error=%v", count, len(content), streaming, err)
	}
}
