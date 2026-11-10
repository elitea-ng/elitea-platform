package repos

// Agent proposals (shared/0234, ADR-0050 decision 5a, #1615) and Copy to
// project's memory copy against a REAL migrated database. It SKIPS without
// ELITEA_TEST_DATABASE_URL, and a skip prints the same `ok`, so each case
// asserts values a skip could not have produced.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	proposalProject     int64 = 1
	proposalProposer    int64 = 991615
	proposalNewAgent    int64 = 991616
	proposalPersonalSrc int64 = 991617
	proposalTeamCopy    int64 = 991618
	// proposalOldAgent existed before any proposal; proposalOthersAgent's
	// first version is another person's.
	proposalOldAgent    int64 = 991619
	proposalOthersAgent int64 = 991620
)

func proposalStore(t *testing.T, pool *pgxpool.Pool, now *time.Time) *AgentProposals {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewAgentProposals(pool, appender)
	if err != nil {
		t.Fatal(err)
	}
	return store.WithClock(func() time.Time { return *now })
}

// seedProposalApplications writes the agents and their first versions: the
// columns MarkProposalCreated and IsForkOfBy read (applications.created_at,
// application_versions author_id and meta) are the tenant schema's own
// (001_initial.sql), added here because the template's minimal tables lack
// them. Person 4 wrote every first version but proposalOthersAgent's;
// proposalTeamCopy is person 4's fork of proposalPersonalSrc in project 3.
func seedProposalApplications(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`ALTER TABLE p_1.applications ADD COLUMN IF NOT EXISTS created_at timestamp NOT NULL DEFAULT now()`,
		`ALTER TABLE p_1.application_versions ADD COLUMN IF NOT EXISTS application_id integer`,
		`ALTER TABLE p_1.application_versions ADD COLUMN IF NOT EXISTS author_id integer`,
		`ALTER TABLE p_1.application_versions ADD COLUMN IF NOT EXISTS meta jsonb DEFAULT '{}'::jsonb`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the application columns: %v", err)
		}
	}
	for _, agent := range []struct {
		id      int64
		created string
		author  int64
		meta    string
	}{
		{proposalProposer, "now()", 4, `{}`},
		{proposalNewAgent, "now()", 4, `{}`},
		{proposalPersonalSrc, "now()", 4, `{}`},
		{proposalTeamCopy, "now()", 4, fmt.Sprintf(`{"parent_entity_id":"%d","parent_project_id":"3"}`, proposalPersonalSrc)},
		{proposalOldAgent, "(now() - interval '1 day')", 4, `{}`},
		{proposalOthersAgent, "now()", 5, `{}`},
	} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO p_1.applications (id, name, owner_id, created_at)
VALUES ($1, $2, 4, %s AT TIME ZONE 'UTC') ON CONFLICT (id) DO NOTHING`, agent.created),
			agent.id, fmt.Sprintf("Agent %d", agent.id)); err != nil {
			t.Fatalf("seed application %d: %v", agent.id, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p_1.application_versions (application_id, author_id, meta)
SELECT $1, $2, $3::jsonb WHERE NOT EXISTS (SELECT 1 FROM p_1.application_versions WHERE application_id = $1)`,
			agent.id, agent.author, agent.meta); err != nil {
			t.Fatalf("seed the first version of %d: %v", agent.id, err)
		}
	}
}

// seedProposingRun writes a claimed run of the proposer whose frozen version
// grants (or not) can_propose_agents, served to person 4 in a conversation.
func seedProposingRun(t *testing.T, pool *pgxpool.Pool, executionID string, canPropose bool) string {
	t.Helper()
	seedEgressExecution(t, pool, executionID, proposalProject, proposalProposer)
	conversation := "6b8f2a1c-0d3e-4f5a-9b7c-1e2d3f4a5b6c"
	input := agentCapacityInput("gen-" + executionID)
	input.Application = fmt.Appendf(nil, `{"id":%d,"version_details":{"meta":{"capabilities":{"can_propose_agents":%t}}}}`, proposalProposer, canPropose)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	ctx := context.Background()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE elitea_runtime.input_bundle_entries
SET content_bytes = $2, content_digest = $3, content_size = $4, semantic_role = 'agent.execution_request'
WHERE input_bundle_id = $1`, []any{"bundle-" + executionID, encoded, digest[:], len(encoded)}},
		{`UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $2 WHERE execution_id = $1`, []any{executionID, conversation}},
		{`UPDATE elitea_runtime.execution_jobs SET actor_id = '4', principal_ref = '4' WHERE execution_id = $1`, []any{executionID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed the proposing run: %v", err)
		}
	}
	return conversation
}

func proposalWrite(agentID, soul string) storage.ProposalWrite {
	request, err := agentproposal.Check(agentproposal.Request{
		EphemeralAgentID: agentID, Rationale: "It found the bug fast.", Instructions: "Review pull requests.",
		Profile:     agentproposal.Profile{DisplayName: "Ada", Soul: soul, Tools: []agentproposal.Tool{{Key: "github/read_file"}}},
		MemorySeeds: []agentproposal.Seed{{Content: "The repo uses Go."}, {Kind: "preference", Content: "Short answers."}},
	})
	if err != nil {
		panic(err)
	}
	return storage.ProposalWrite{Request: request, SpecDigest: agentproposal.SpecDigest(request.Profile), Draft: agentproposal.Draft(request)}
}

func TestAgentProposalsReadTheRunFromTheExecutionKernel(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store := proposalStore(t, pool, &now)
	conversation := seedProposingRun(t, pool, "exec-propose-run-1615", true)
	run, err := store.ResolveProposeAgentRun(ctx, proposalProject, "exec-propose-run-1615", 1)
	if err != nil {
		t.Fatal(err)
	}
	if run.ApplicationID != proposalProposer || run.ActorID != 4 || run.ConversationUUID != conversation || !run.CanPropose {
		t.Fatalf("run %+v", run)
	}
	seedProposingRun(t, pool, "exec-propose-run-off-1615", false)
	run, err = store.ResolveProposeAgentRun(ctx, proposalProject, "exec-propose-run-off-1615", 1)
	if err != nil || run.CanPropose {
		t.Fatalf("a version without the capability: %+v %v", run, err)
	}
	if _, err := store.ResolveProposeAgentRun(ctx, 2, "exec-propose-run-1615", 1); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("another project's claim: %v", err)
	}
}

// Three open per agent; a retried call answers the same row; a dismissed
// spec is suppressed for 30 days and not after; Create links the agent.
func TestAgentProposalsLimitsSuppressionAndDecisions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedApplicationsTable(t, pool)
	seedProposalApplications(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	store := proposalStore(t, pool, &now)
	conversation := seedProposingRun(t, pool, "exec-propose-1615", true)
	run, err := store.ResolveProposeAgentRun(ctx, proposalProject, "exec-propose-1615", 1)
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]int64, 0, 3)
	for index := range 3 {
		outcome, err := store.Propose(ctx, run, proposalWrite(fmt.Sprintf("a_0000000%d", index), fmt.Sprintf("Soul %d.", index)))
		if err != nil || outcome.Status != agentproposal.OutcomeProposed || outcome.ProposalID <= 0 {
			t.Fatalf("proposal %d: %+v %v", index, outcome, err)
		}
		ids = append(ids, outcome.ProposalID)
	}
	again, err := store.Propose(ctx, run, proposalWrite("a_00000000", "Soul 0."))
	if err != nil || again.Status != agentproposal.OutcomeDuplicate || again.ProposalID != ids[0] {
		t.Fatalf("a retried call: %+v %v", again, err)
	}
	limited, err := store.Propose(ctx, run, proposalWrite("a_00000003", "Soul 3."))
	if err != nil || limited.Status != agentproposal.OutcomeLimit {
		t.Fatalf("the fourth open proposal: %+v %v", limited, err)
	}

	// The inbox: three open, the conversation's three, nothing created yet.
	rows, total, err := store.ListProposals(ctx, proposalProject, AgentProposalFilter{ConversationUUID: conversation})
	if err != nil || total != 3 || len(rows) != 3 {
		t.Fatalf("inbox: %d %d %v", total, len(rows), err)
	}
	if rows[0].ProposerName != fmt.Sprintf("Agent %d", proposalProposer) || rows[0].Draft["risk_tier"] != "low" ||
		len(rows[0].MemorySeeds) != 2 || len(rows[0].ToolCategories) != 1 || rows[0].Evidence != nil {
		t.Fatalf("an inbox row: %+v", rows[0])
	}
	var agents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.applications WHERE name = 'Ada'`).Scan(&agents); err != nil || agents != 0 {
		t.Fatalf("nothing is saved as an agent before Create: %d %v", agents, err)
	}

	// Dismiss the first: a slot frees, and its spec is suppressed.
	if err := store.DismissProposal(ctx, proposalProject, ids[0], 4); err != nil {
		t.Fatal(err)
	}
	if err := store.DismissProposal(ctx, proposalProject, ids[0], 4); !errors.Is(err, ErrProposalDecided) {
		t.Fatalf("dismissing twice: %v", err)
	}
	suppressed, err := store.Propose(ctx, run, proposalWrite("a_00000004", "Soul 0."))
	if err != nil || suppressed.Status != agentproposal.OutcomeSuppressed {
		t.Fatalf("the dismissed spec again: %+v %v", suppressed, err)
	}
	other, err := store.Propose(ctx, run, proposalWrite("a_00000005", "Soul 5."))
	if err != nil || other.Status != agentproposal.OutcomeProposed {
		t.Fatalf("another spec in the freed slot: %+v %v", other, err)
	}
	if err := store.DismissProposal(ctx, proposalProject, other.ProposalID, 4); err != nil {
		t.Fatal(err)
	}
	now = now.Add(agentproposal.SuppressFor + time.Hour)
	after, err := store.Propose(ctx, run, proposalWrite("a_00000006", "Soul 0."))
	if err != nil || after.Status != agentproposal.OutcomeProposed {
		t.Fatalf("the dismissed spec after 30 days: %+v %v", after, err)
	}

	// Create: the agent must exist; the proposal links it, once.
	if err := store.MarkProposalCreated(ctx, proposalProject, ids[1], 4, 999999999); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("an agent that does not exist: %v", err)
	}
	// Only an agent the caller just made: not one older than the proposal,
	// not one whose first version another person wrote.
	for _, agent := range []int64{proposalOldAgent, proposalOthersAgent} {
		if err := store.MarkProposalCreated(ctx, proposalProject, ids[1], 4, agent); !errors.Is(err, ErrNotYourNewAgent) {
			t.Fatalf("linking agent %d: %v", agent, err)
		}
	}
	if err := store.MarkProposalCreated(ctx, proposalProject, ids[1], 5, proposalNewAgent); !errors.Is(err, ErrNotYourNewAgent) {
		t.Fatalf("another person linking person 4's new agent: %v", err)
	}
	if err := store.MarkProposalCreated(ctx, proposalProject, ids[1], 4, proposalNewAgent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProposalCreated(ctx, proposalProject, ids[2], 4, proposalNewAgent); !errors.Is(err, ErrNotYourNewAgent) {
		t.Fatalf("one agent linked to a second proposal: %v", err)
	}
	created, err := store.GetProposal(ctx, proposalProject, ids[1])
	if err != nil || created.Status != "created" || created.CreatedApplicationID == nil || *created.CreatedApplicationID != proposalNewAgent {
		t.Fatalf("created: %+v %v", created, err)
	}
	if err := store.MarkProposalCreated(ctx, proposalProject, ids[1], 4, proposalNewAgent); !errors.Is(err, ErrProposalDecided) {
		t.Fatalf("creating twice: %v", err)
	}
	seeded, err := store.SeedAgentMemory(ctx, proposalProject, proposalNewAgent, 4,
		[]CopiedMemory{{Kind: "fact", Content: "The repo uses Go."}, {Kind: "cursor", Content: "skipped"}}, "agent.proposal.seeded", "x")
	if err != nil || seeded != 1 {
		t.Fatalf("seeding: %d %v", seeded, err)
	}
	// Linking never touches the registry: the new agent's tier is the
	// create route's (low), never the proposer's.
	var registry int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, proposalProject, proposalNewAgent).Scan(&registry); err != nil || registry != 0 {
		t.Fatalf("a registry row written by the proposal store: %d %v", registry, err)
	}

	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeAgentProposalChanged)).Scan(&events); err != nil || events < 8 {
		t.Fatalf("audit events: %d %v", events, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, proposalNewAgent)
	})
}

// Copy to project: the copy's entries are rows of their own; changing one
// never changes the other. An entry about a person and a cursor stay.
func TestCopyMemoryMakesIndependentRows(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedApplicationsTable(t, pool)
	seedProposalApplications(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()
	store := proposalStore(t, pool, &now)
	var ids []int64
	rows, err := pool.Query(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, subject_user_id, status, written_by_kind, pinned)
VALUES ($1, 'fact', 'Uses pnpm', NULL, 'active', 'person', true),
       ($1, 'preference', 'Dana likes tea', 7, 'active', 'agent', false),
       ($1, 'cursor', 'page 4', NULL, 'active', 'agent', false),
       ($1, 'fact', 'Waits for review', NULL, 'pending', 'agent', false)
RETURNING id`, proposalPersonalSrc)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = ANY($1)`,
			[]int64{proposalPersonalSrc, proposalTeamCopy})
	})

	entries, err := store.MemoryEntriesForCopy(ctx, proposalProject, proposalPersonalSrc, ids)
	if err != nil || len(entries) != 1 || entries[0].Content != "Uses pnpm" || !entries[0].Pinned {
		t.Fatalf("only the active entry about nobody: %+v %v", entries, err)
	}
	copied, err := store.SeedAgentMemory(ctx, proposalProject, proposalTeamCopy, 4, entries, "agent.copied_to_project", "1:x")
	if err != nil || copied != 1 {
		t.Fatalf("copied %d %v", copied, err)
	}
	var copyID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM p_1.agent_memory_entries WHERE application_id = $1`, proposalTeamCopy).Scan(&copyID); err != nil {
		t.Fatal(err)
	}
	if copyID == ids[0] {
		t.Fatal("the copy shares the source row")
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.agent_memory_entries SET content = 'Uses npm' WHERE id = $1`, copyID); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT content FROM p_1.agent_memory_entries WHERE id = $1`, ids[0]).Scan(&source); err != nil || source != "Uses pnpm" {
		t.Fatalf("editing the copy changed the source: %q %v", source, err)
	}
	var versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.agent_memory_entry_versions WHERE entry_id = $1`, copyID).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("the copy's first version: %d %v", versions, err)
	}
	if _, err := store.SeedAgentMemory(ctx, proposalProject, 999999999, 4, entries, "agent.copied_to_project", "x"); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("a copy into an agent that does not exist: %v", err)
	}

	// The copy's target must be the caller's fork of the source.
	for _, check := range []struct {
		application, user, sourceProject, sourceApplication int64
		want                                                bool
	}{
		{proposalTeamCopy, 4, 3, proposalPersonalSrc, true},
		{proposalTeamCopy, 5, 3, proposalPersonalSrc, false},
		{proposalTeamCopy, 4, 2, proposalPersonalSrc, false},
		{proposalNewAgent, 4, 3, proposalPersonalSrc, false},
		{999999999, 4, 3, proposalPersonalSrc, false},
	} {
		fork, err := store.IsForkOfBy(ctx, proposalProject, check.application, check.user, check.sourceProject, check.sourceApplication)
		if err != nil || fork != check.want {
			t.Fatalf("IsForkOfBy%+v = %t %v", check, fork, err)
		}
	}

	// Seeding keeps the active limit: the oldest unpinned episodes go.
	limited := proposalStore(t, pool, &now).WithActiveLimit(2)
	episodes := []CopiedMemory{{Kind: "episode", Content: "first"}, {Kind: "episode", Content: "second"}, {Kind: "episode", Content: "third"}}
	if _, err := limited.SeedAgentMemory(ctx, proposalProject, proposalTeamCopy, 4, episodes, "agent.copied_to_project", "x"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.agent_memory_entries WHERE application_id = $1 AND status = 'active'`,
		proposalTeamCopy).Scan(&active); err != nil || active != 2 {
		t.Fatalf("active entries past the limit: %d %v", active, err)
	}
}
