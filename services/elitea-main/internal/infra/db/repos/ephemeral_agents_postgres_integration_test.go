package repos

// The sub-agent record (shared/0232, ADR-0050 decisions 4 and 9d, #1614)
// against a REAL migrated database. It SKIPS without
// ELITEA_TEST_DATABASE_URL; each case asserts values a skip could not have
// produced.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ephemeralRow(agentID, status string) EphemeralAgentRow {
	return EphemeralAgentRow{
		AgentID: agentID, ParentCallID: "call-" + agentID, Ordinal: 1, Label: "Owl", DisplayName: "Owl",
		Avatar: "default:owl", ProfileDigest: strings.Repeat("ab", 32), BriefDigest: strings.Repeat("cd", 32),
		BriefRefs: []byte(`{"message_ids":["m-1"],"attachments":[],"memory_entries":[],"facts":0}`),
		Status:    status, Steps: 1, InputTokens: 10, OutputTokens: 2,
		StartedAt: time.Unix(1_800_000_000, 0).UTC(),
	}
}

// One row per spawn, the project and parent agent from the execution
// kernel, and a retried or later report updates the same row.
func TestEphemeralAgentsOneRowPerSpawn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const execution = "exec-ephemeral-1614"
	seedEgressExecution(t, pool, execution, 7, 31)
	store, err := NewEphemeralAgents(pool)
	if err != nil {
		t.Fatal(err)
	}
	running := ephemeralRow("a_0011aabb", "running")
	other := ephemeralRow("a_2233ccdd", "running")
	other.Ordinal, other.BriefDigest, other.BriefRefs, other.Label = 2, "", nil, ""
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{running, other}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The same snapshot again, then the settled one: still one row each.
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{running, other}); err != nil {
		t.Fatalf("record again: %v", err)
	}
	done := running
	done.Status, done.Steps, done.InputTokens, done.FinishedAt = "completed", 4, 300, time.Unix(1_800_000_010, 0).UTC()
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{done}); err != nil {
		t.Fatalf("record settled: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.ephemeral_agents WHERE execution_id = $1`, execution).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rows=%d, want one per spawn", count)
	}
	var (
		project, parent  int64
		status, refs     string
		steps            int
		label, digest    *string
		finished         *time.Time
		retention, cmdID string
	)
	if err := pool.QueryRow(ctx, `
SELECT project_id, parent_application_id, status, steps, brief_refs::text, finished_at, retention_class, command_id
FROM elitea_runtime.ephemeral_agents WHERE execution_id = $1 AND agent_id = 'a_0011aabb'`, execution).Scan(
		&project, &parent, &status, &steps, &refs, &finished, &retention, &cmdID); err != nil {
		t.Fatal(err)
	}
	if project != 7 || parent != 31 || status != "completed" || steps != 4 || finished == nil ||
		!strings.Contains(refs, "m-1") || retention != "step_record" || cmdID != "cmd-"+execution {
		t.Fatalf("project=%d parent=%d status=%s steps=%d refs=%s finished=%v retention=%s command=%s",
			project, parent, status, steps, refs, finished, retention, cmdID)
	}
	if err := pool.QueryRow(ctx, `
SELECT label, brief_digest FROM elitea_runtime.ephemeral_agents WHERE execution_id = $1 AND agent_id = 'a_2233ccdd'`, execution).Scan(&label, &digest); err != nil {
		t.Fatal(err)
	}
	if label != nil || digest != nil {
		t.Fatalf("empty label and brief must be NULL: label=%v digest=%v", label, digest)
	}

	// A report that changes a spawn's profile does not rewrite its row.
	forged := done
	forged.ProfileDigest, forged.Status = strings.Repeat("ef", 32), "failed"
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{forged}); err != nil {
		t.Fatalf("record forged: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT status FROM elitea_runtime.ephemeral_agents WHERE execution_id = $1 AND agent_id = 'a_0011aabb'`, execution).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("a changed profile rewrote the row: status=%s", status)
	}

	// No job for the generation: refused, nothing stored.
	if err := store.Record(ctx, execution, "cmd-x", 9, []EphemeralAgentRow{ephemeralRow("a_44556677", "running")}); !errors.Is(err, ErrEphemeralAgentsRefused) {
		t.Fatalf("unknown generation: %v", err)
	}
}

// A depth-2 run records two levels (#1872): the children at depth 1 with no
// parent, the grandchild at depth 2 under the child that started it. A
// later report cannot move a spawn to another parent, and the table refuses
// a grandchild without one.
func TestEphemeralAgentsRecordDepthAndParent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const execution = "exec-ephemeral-1872"
	seedEgressExecution(t, pool, execution, 7, 31)
	store, err := NewEphemeralAgents(pool)
	if err != nil {
		t.Fatal(err)
	}
	child := ephemeralRow("a_00000001", "running")
	child.Depth = 1
	legacy := ephemeralRow("a_00000002", "running")
	legacy.Ordinal = 2 // Depth 0: an earlier worker, stored as 1.
	grandchild := ephemeralRow("a_00000003", "running")
	grandchild.Ordinal, grandchild.Depth, grandchild.ParentAgentID = 3, 2, "a_00000001"
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{child, legacy, grandchild}); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := pool.Query(ctx, `
SELECT agent_id, depth, COALESCE(parent_agent_id, '') FROM elitea_runtime.ephemeral_agents
WHERE execution_id = $1 ORDER BY ordinal`, execution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var (
			agentID, parent string
			depth           int
		)
		if err := rows.Scan(&agentID, &depth, &parent); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d:%s", agentID, depth, parent))
	}
	rows.Close()
	want := []string{"a_00000001:1:", "a_00000002:1:", "a_00000003:2:a_00000001"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows=%v, want %v", got, want)
	}

	// A report that names another parent does not rewrite the row.
	moved := grandchild
	moved.ParentAgentID, moved.Status = "a_00000002", "completed"
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{moved}); err != nil {
		t.Fatalf("record moved: %v", err)
	}
	var status, parent string
	if err := pool.QueryRow(ctx, `
SELECT status, parent_agent_id FROM elitea_runtime.ephemeral_agents WHERE execution_id = $1 AND agent_id = 'a_00000003'`,
		execution).Scan(&status, &parent); err != nil {
		t.Fatal(err)
	}
	if status != "running" || parent != "a_00000001" {
		t.Fatalf("a changed parent rewrote the row: status=%s parent=%s", status, parent)
	}

	// The table refuses a grandchild without a parent.
	orphan := ephemeralRow("a_00000004", "running")
	orphan.Ordinal, orphan.Depth = 4, 2
	if err := store.Record(ctx, execution, "cmd-"+execution, 1, []EphemeralAgentRow{orphan}); err == nil {
		t.Fatal("a depth-2 spawn without a parent was stored")
	}
}
