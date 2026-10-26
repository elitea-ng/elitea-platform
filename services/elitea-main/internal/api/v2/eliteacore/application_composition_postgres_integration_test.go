package eliteacore_test

// Real-PostgreSQL coverage for FU-244: an attach (relation PATCH) or a
// pipeline save that would break a composition bound the Worker enforces is
// refused with APPLICATION_COMPOSITION_INVALID naming the path, and nothing is
// written. The schema comes from the real migration corpus (newPublishCopyPool).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type savedVersion struct{ appID, versionID int }

func seedComposed(t *testing.T, pool *pgxpool.Pool, name, agentType, instructions string) savedVersion {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var saved savedVersion
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.applications (name, description, owner_id) VALUES ($1, 'seeded', 1) RETURNING id`, name).
		Scan(&saved.appID); err != nil {
		t.Fatalf("seed application %s: %v", name, err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, $2, $3) RETURNING id`, saved.appID, instructions, agentType).
		Scan(&saved.versionID); err != nil {
		t.Fatalf("seed version %s: %v", name, err)
	}
	return saved
}

// attach PATCHes child onto parent as a tool and answers the response.
func attach(t *testing.T, pool *pgxpool.Pool, parent, child savedVersion) (int, map[string]any) {
	t.Helper()
	recorder := relationDo(t, relationRouter(eliteacore.NewHandler(pool)), relationFixture{
		parentAppID: parent.appID, parentVersionID: parent.versionID,
		childAppID: child.appID, childVersionID: child.versionID,
	}, map[string]any{"application_id": parent.appID, "version_id": parent.versionID, "has_relation": true})
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

func mustAttach(t *testing.T, pool *pgxpool.Pool, parent, child savedVersion) {
	t.Helper()
	if code, body := attach(t, pool, parent, child); code != http.StatusCreated {
		t.Fatalf("attach status = %d, body = %v", code, body)
	}
}

func expectRefused(t *testing.T, pool *pgxpool.Pool, parent, child savedVersion, wants ...string) {
	t.Helper()
	before, _ := countRelationRows(t, pool, parent.versionID)
	code, body := attach(t, pool, parent, child)
	if code != http.StatusBadRequest || body["code"] != "APPLICATION_COMPOSITION_INVALID" {
		t.Fatalf("attach status = %d, body = %v", code, body)
	}
	message, _ := body["error"].(string)
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q does not name %q", message, want)
		}
	}
	if after, _ := countRelationRows(t, pool, parent.versionID); after != before {
		t.Fatalf("a refused attach wrote a mapping: %d -> %d", before, after)
	}
}

func agentNodePipeline(alias string, pauses bool) string {
	text := fmt.Sprintf("state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: %q\n    input_mapping:\n      task: {type: fixed, value: 'Summarize'}\n    output: [answer, messages]\n    transition: END\n", alias)
	if pauses {
		text += "interrupt_before: [delegate]\n"
	}
	return text
}

func TestAnAttachThatClosesACycleIsRefusedAndWritesNothing(t *testing.T) {
	pool := newPublishCopyPool(t)
	planner := seedComposed(t, pool, "Planner", "agent", "plan")
	flow := seedComposed(t, pool, "Flow", "pipeline", agentNodePipeline("Planner", false))
	mustAttach(t, pool, planner, flow)
	expectRefused(t, pool, flow, planner, "circular", `"Flow" → "Planner" → "Flow"`)
}

func TestAgentTiersAreRefusedOnlyPastTheLimit(t *testing.T) {
	pool := newPublishCopyPool(t)
	// The limit: three agent tiers, the root included.
	top := seedComposed(t, pool, "Top", "agent", "top")
	middle := seedComposed(t, pool, "Middle", "agent", "middle")
	leaf := seedComposed(t, pool, "Leaf", "agent", "leaf")
	mustAttach(t, pool, middle, leaf)
	mustAttach(t, pool, top, middle)
	// One more: a chain of three under a new root is four tiers.
	root := seedComposed(t, pool, "Root", "agent", "root")
	expectRefused(t, pool, root, top, `"Root" → "Top" → "Middle" → "Leaf"`)
}

func TestAPausingPipelineUnderAPipelineAgentNodeIsRefusedAtAttachAndAtSave(t *testing.T) {
	pool := newPublishCopyPool(t)
	approval := seedComposed(t, pool, "Approval", "pipeline", "state:\n  messages: list\nentry_point: a\ninterrupt_before: [a]\nnodes:\n  - id: a\n    type: llm\n    transition: END\n")
	reviewer := seedComposed(t, pool, "Reviewer", "agent", "review")
	root := seedComposed(t, pool, "Release flow", "pipeline", agentNodePipeline("Reviewer", false))
	// An agent may carry a pausing pipeline: its own chat continues the pause.
	mustAttach(t, pool, reviewer, approval)
	expectRefused(t, pool, root, reviewer, `Pipeline "Approval"`, `agent "Reviewer"`)

	// The save path: the pipeline attaches the agent first, without an Agent
	// node, and the save that adds the node is refused.
	other := seedComposed(t, pool, "Other flow", "pipeline", "state:\n  messages: list\nentry_point: a\nnodes:\n  - id: a\n    type: llm\n    transition: END\n")
	mustAttach(t, pool, other, reviewer)
	repo := repos.NewApplicationsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := repo.UpdateVersion(ctx, "1", fmt.Sprint(other.appID), fmt.Sprint(other.versionID),
		applications.Version{AgentType: "pipeline", Instructions: agentNodePipeline("Reviewer", false)})
	apiErr, ok := err.(*apierr.APIError)
	if !ok || apiErr.Status != http.StatusBadRequest || apiErr.Code != "APPLICATION_COMPOSITION_INVALID" || !strings.Contains(apiErr.Message, `Pipeline "Approval"`) {
		t.Fatalf("save err = %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT instructions FROM p_1.application_versions WHERE id = $1`, other.versionID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(stored, "type: agent") {
		t.Fatal("a refused save stored the new definition")
	}
	// The same save without the pause below is stored.
	if _, err := repo.UpdateVersion(ctx, "1", fmt.Sprint(other.appID), fmt.Sprint(other.versionID),
		applications.Version{AgentType: "pipeline", Instructions: agentNodePipeline("Missing alias", false)}); err != nil {
		t.Fatalf("an Agent node on no attachment is the Worker's to refuse, not the composition check's: %v", err)
	}
}

// The save check at its hop budget on real PostgreSQL: 25 attachments under
// one root, walked through the same source the routes use. The bound is
// generous (the measurement is recorded in the source mapping); the shape
// under test is that the walk is bounded, not the machine's speed.
func TestTheCompositionCheckAtItsHopBudgetIsFast(t *testing.T) {
	pool := newPublishCopyPool(t)
	root := seedComposed(t, pool, "Wide root", "agent", "root")
	for n := 0; n < applicationcomposition.MaxApplicationHops; n++ {
		mustAttach(t, pool, root, seedComposed(t, pool, fmt.Sprintf("Leaf %02d", n), "agent", "leaf"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id := applicationcomposition.Identity{ApplicationID: int64(root.appID), VersionID: int64(root.versionID)}
	var slowest time.Duration
	for range 20 {
		started := time.Now()
		if err := applicationcomposition.Check(ctx, repos.NewCompositionSource(pool, `"p_1"`), id, nil); err != nil {
			t.Fatalf("the hop budget itself must pass: %v", err)
		}
		slowest = max(slowest, time.Since(started))
	}
	t.Logf("composition check, %d attachments, slowest of 20: %s", applicationcomposition.MaxApplicationHops, slowest)
	if slowest > 2*time.Second {
		t.Fatalf("a bounded walk took %s", slowest)
	}
	expectRefused(t, pool, root, seedComposed(t, pool, "One too many", "agent", "leaf"), "more than 25")
}

// Review item 7: turning an agent into a pipeline walks its stored
// instructions as the pipeline it becomes, even when the write sends none.
func TestTurningAnAgentIntoAPipelineWalksItsStoredInstructions(t *testing.T) {
	pool := newPublishCopyPool(t)
	approval := seedComposed(t, pool, "Approval", "pipeline", "state:\n  messages: list\nentry_point: a\ninterrupt_before: [a]\nnodes:\n  - id: a\n    type: llm\n    transition: END\n")
	reviewer := seedComposed(t, pool, "Reviewer", "agent", "review")
	mustAttach(t, pool, reviewer, approval)
	// As an agent its instructions are a prompt; as a pipeline they run Reviewer
	// from an Agent node, whose pausing pipeline tool a pipeline root cannot continue.
	switcher := seedComposed(t, pool, "Switcher", "agent", agentNodePipeline("Reviewer", false))
	mustAttach(t, pool, switcher, reviewer)
	repo := repos.NewApplicationsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := repo.UpdateVersion(ctx, "1", fmt.Sprint(switcher.appID), fmt.Sprint(switcher.versionID),
		applications.Version{AgentType: "pipeline"})
	apiErr, ok := err.(*apierr.APIError)
	if !ok || apiErr.Status != http.StatusBadRequest || apiErr.Code != applicationcomposition.Code || !strings.Contains(apiErr.Message, `Pipeline "Approval"`) {
		t.Fatalf("switch err = %v", err)
	}
	var agentType string
	if err := pool.QueryRow(ctx, `SELECT agent_type FROM p_1.application_versions WHERE id = $1`, switcher.versionID).Scan(&agentType); err != nil {
		t.Fatal(err)
	}
	if agentType != "agent" {
		t.Fatalf("a refused switch stored %q", agentType)
	}
}

// countingReader counts the statements a composition read sends.
type countingReader struct {
	pool       *pgxpool.Pool
	statements int
}

func (c *countingReader) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.statements++
	return c.pool.Query(ctx, sql, args...)
}

func (c *countingReader) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.statements++
	return c.pool.QueryRow(ctx, sql, args...)
}

// Review item 9: a participant read judges every participant through one
// source that reads a level of the composition per batch, so its statement
// count does not grow with the number of participants.
func TestJudgingParticipantsCostsAFixedNumberOfStatements(t *testing.T) {
	pool := newPublishCopyPool(t)
	var participants []applicationcomposition.Identity
	for n := range 8 {
		orchestrator := seedComposed(t, pool, fmt.Sprintf("Orchestrator %d", n), "agent", "o")
		mustAttach(t, pool, orchestrator, seedComposed(t, pool, fmt.Sprintf("Worker %d", n), "agent", "w"))
		participants = append(participants, applicationcomposition.Identity{ApplicationID: int64(orchestrator.appID), VersionID: int64(orchestrator.versionID)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cost := func(ids []applicationcomposition.Identity) int {
		reader := &countingReader{pool: pool}
		fits, err := applicationcomposition.ChatToolFits(ctx, repos.NewCompositionSource(reader, `"p_1"`), ids)
		if err != nil {
			t.Fatal(err)
		}
		for _, fit := range fits {
			if !fit.Fits || fit.Tiers != 2 {
				t.Fatalf("fits = %+v", fits)
			}
		}
		return reader.statements
	}
	n, twice := cost(participants[:4]), cost(participants)
	// One legacy-table probe, and per level of the composition one batched read
	// of the versions and one of their attachments (two levels here).
	if n != twice || twice > 5 {
		t.Fatalf("statements: %d for 4 participants, %d for 8; want the same, at most 5", n, twice)
	}
	t.Logf("statements for 4 and 8 participants: %d, %d", n, twice)
}

// Review round 2 (C, PLAN §8.8): a chain linked top-down is refused at the link
// that would make a fourth agent level, from the top of the chain, although the
// parent being linked holds only two levels itself.
func TestAnAttachIsRefusedWhenAnAncestorWouldPassTheLimit(t *testing.T) {
	pool := newPublishCopyPool(t)
	chain := make([]savedVersion, 5)
	for n := range chain {
		chain[n] = seedComposed(t, pool, fmt.Sprintf("Tier %d", n), "agent", "work")
	}
	mustAttach(t, pool, chain[0], chain[1])
	mustAttach(t, pool, chain[1], chain[2])
	expectRefused(t, pool, chain[2], chain[3], "Agents are nested more than 3 levels deep",
		`"Tier 0" → "Tier 1" → "Tier 2" → "Tier 3"`)
}

// The upward walk costs a few batched statements per level, not per ancestor:
// ten agents attaching one version and ten more above them.
func TestTheAncestorWalkCostsAFixedNumberOfStatementsPerLevel(t *testing.T) {
	pool := newPublishCopyPool(t)
	leaf := seedComposed(t, pool, "Shared leaf", "agent", "work")
	for n := range 10 {
		middle := seedComposed(t, pool, fmt.Sprintf("Middle %d", n), "agent", "work")
		mustAttach(t, pool, middle, leaf)
		mustAttach(t, pool, seedComposed(t, pool, fmt.Sprintf("Top %d", n), "agent", "work"), middle)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reader := &countingReader{pool: pool}
	id := applicationcomposition.Identity{ApplicationID: int64(leaf.appID), VersionID: int64(leaf.versionID)}
	if err := applicationcomposition.CheckWithAncestors(ctx, repos.NewCompositionSource(reader, `"p_1"`), id, nil); err != nil {
		t.Fatal(err)
	}
	// The leaf and its attachments (2), the legacy probe (1), three parent
	// levels (3), and the twenty ancestors read in one batch (2); their walks
	// then read from the cache.
	t.Logf("statements for 20 ancestors: %d", reader.statements)
	if reader.statements > 8 {
		t.Fatalf("the ancestor walk sent %d statements for 20 ancestors", reader.statements)
	}
}
