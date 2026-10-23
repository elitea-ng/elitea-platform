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
