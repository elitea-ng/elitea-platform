package mcp

// FU-244, review item 2: the internal MCP writers of a version (the instruction
// patch and an agent_type update) carry the same composition check as the REST
// writes: a refused write changes and backs up nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// seedComposedApplication seeds one application version and answers its ids.
func seedComposedApplication(t *testing.T, pool *pgxpool.Pool, name, agentType, instructions string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var applicationID, versionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.applications (name, description, owner_id, meta, created_at, updated_at)
		VALUES ($1, 'fixture', 1, '{}'::jsonb, now(), now()) RETURNING id`, name).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type, instructions, created_at)
		VALUES ($1, 'base', 'draft', 7, $2, $3, now()) RETURNING id`, applicationID, agentType, instructions).Scan(&versionID); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	return applicationID, versionID
}

// attachComposed writes the reference pair Main's relation route writes.
func attachComposed(t *testing.T, pool *pgxpool.Pool, parentApp, parentVersion, childApp, childVersion int64, alias string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
WITH tool AS (
  INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
  VALUES ($1, 'application', '', jsonb_build_object('application_id', $2::bigint, 'application_version_id', $3::bigint), '{}'::jsonb, 1, 1, now())
  RETURNING id)
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id, created_at)
SELECT $4, 'agent', tool.id, $5, now() FROM tool`, alias, childApp, childVersion, parentVersion, parentApp); err != nil {
		t.Fatalf("attach %s: %v", alias, err)
	}
}

const reviewerNode = "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: Reviewer\n    input_mapping:\n      task: {type: fixed, value: 'Summarize'}\n    output: [answer, messages]\n    transition: END\n"

func TestInternalWritesRefuseABrokenComposition(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := context.Background()
	approvalApp, approvalVersion := seedComposedApplication(t, pool, "Approval", "pipeline",
		"state:\n  messages: list\nentry_point: a\ninterrupt_before: [a]\nnodes:\n  - id: a\n    type: llm\n    transition: END\n")
	reviewerApp, reviewerVersion := seedComposedApplication(t, pool, "Reviewer", "agent", "review")
	attachComposed(t, pool, reviewerApp, reviewerVersion, approvalApp, approvalVersion, "Approval")
	executor := newPostgresInternalApplicationExecutor(pool)
	refused := func(name string, status int, body string) {
		t.Helper()
		if status != 400 || !strings.Contains(body, applicationcomposition.Code) || !strings.Contains(body, `Pipeline \"Approval\"`) {
			t.Fatalf("%s: status=%d body=%.400s", name, status, body)
		}
	}
	stored := func(versionID int64) (string, string) {
		var agentType, instructions string
		if err := pool.QueryRow(ctx, `SELECT agent_type, instructions FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&agentType, &instructions); err != nil {
			t.Fatal(err)
		}
		return agentType, instructions
	}

	// The instruction patch adds an Agent node running Reviewer, whose pausing
	// pipeline tool a pipeline root cannot continue.
	flowApp, flowVersion := seedComposedApplication(t, pool, "Flow", "pipeline", "First rule.")
	attachComposed(t, pool, flowApp, flowVersion, reviewerApp, reviewerVersion, "Reviewer")
	result, err := executor.Execute(ctx, 1, 73, internalPatchInstructions, map[string]any{
		"application_id": flowApp, "version_id": flowVersion,
		"expected_instructions_sha256": instructionsSHA256("First rule."),
		"replace_all":                  true, "replacement": reviewerNode,
	})
	if err != nil {
		t.Fatal(err)
	}
	refused("patch", result.status, string(result.body))
	if _, instructions := stored(flowVersion); instructions != "First rule." {
		t.Fatal("a refused patch changed the instructions")
	}

	// An agent whose stored instructions run Reviewer becomes a pipeline.
	switchApp, switchVersion := seedComposedApplication(t, pool, "Switcher", "agent", reviewerNode)
	attachComposed(t, pool, switchApp, switchVersion, reviewerApp, reviewerVersion, "Reviewer")
	result, err = executor.Execute(ctx, 1, 73, internalUpdateVersion, map[string]any{
		"application_id": switchApp, "version_id": switchVersion, "agent_type": "pipeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	refused("update", result.status, string(result.body))
	if agentType, _ := stored(switchVersion); agentType != "agent" {
		t.Fatalf("a refused update stored %q", agentType)
	}
	var backups int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.application_versions WHERE name LIKE 'mcp-backup-%'`).Scan(&backups); err != nil || backups != 0 {
		t.Fatalf("a refused write created %d backups (%v)", backups, err)
	}
}

// Review round 2 (D): an agent switched to a pipeline by the MCP update has its
// stored text bounded first, as the REST write does; an over-bound text is
// refused, not walked as a pipeline without nodes.
func TestInternalAgentTypeSwitchBoundsTheStoredText(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, versionID := seedComposedApplication(t, pool, "Big switcher", "agent", limitYAML(pipelinelimits.MaxNodes+1, 0))
	executor := newPostgresInternalApplicationExecutor(pool)
	result, err := executor.Execute(context.Background(), 1, 73, internalUpdateVersion, map[string]any{
		"application_id": applicationID, "version_id": versionID, "agent_type": "pipeline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.status != 400 || !strings.Contains(string(result.body), "128 nodes") {
		t.Fatalf("status=%d body=%.300s", result.status, result.body)
	}
	var agentType string
	if err := pool.QueryRow(context.Background(), `SELECT agent_type FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&agentType); err != nil {
		t.Fatal(err)
	}
	if agentType != "agent" {
		t.Fatalf("a refused switch stored %q", agentType)
	}
}
