package repos

import (
	"encoding/json"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"testing"
)

func TestMixedPauseSQLPreservesBothGuardSets(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	ctx := t.Context()
	// This isolated relation contains the existing projection columns used by the query.
	_, err := pool.Exec(ctx, `CREATE TABLE chat_message_group (id bigint PRIMARY KEY, is_streaming boolean, meta jsonb, updated_at timestamptz); INSERT INTO chat_message_group VALUES (1, true, '{"preserved":true}', now())`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := sqlcgen.New(pool).FinalizeCurrentAgentMixedPause(ctx, sqlcgen.FinalizeCurrentAgentMixedPauseParams{
		MessageGroupID: 1, ThreadID: "thread", HitlInterrupt: []byte(`{"interrupt_id":"sensitive"}`), HitlInterrupts: []byte(`[{"interrupt_id":"sensitive"}]`), AuthorizationRequests: []byte(`[{"interrupt_id":"auth"}]`), InvokedSkills: []byte(`[]`),
	})
	if err != nil || rows != 1 {
		t.Fatalf("write rows=%d error=%v", rows, err)
	}
	var streaming bool
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT is_streaming,meta FROM chat_message_group WHERE id=1`).Scan(&streaming, &raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if streaming || meta["preserved"] != true || meta["is_error"] != false {
		t.Fatal("incorrect settled projection")
	}
	for _, key := range []string{"hitl_interrupts", "authorization_requests"} {
		if len(meta[key].([]any)) != 1 {
			t.Fatalf("missing %s", key)
		}
	}
}
