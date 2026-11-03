package repos

// The run brief and the goal check against a real PostgreSQL (ADR-0045
// decision 11, #1564). Requires ELITEA_TEST_DATABASE_URL; every test skips
// without it.

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

const briefGrantID = "4c1d2e3f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"

// startBriefChain starts a desktop chain whose grant asks for a goal check,
// and answers its root.
func startBriefChain(t *testing.T, pool *pgxpool.Pool, service *localturn.Service, conversation string,
	user int64, question, root, kind string,
) localturn.StartOutcome {
	t.Helper()
	outcome, err := service.Start(t.Context(), localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "91", ConversationUUID: conversation,
		QuestionID: question, UserInput: "Make the login test pass.",
		Unattended: &unattended.Request{
			GrantID: briefGrantID, ChainRootExecutionID: root, ContinueStepLimit: true, GoalCheck: true, Kind: kind,
			Chain: unattended.ChainRequest{MaxContinuations: int64Pointer(5)},
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return outcome
}

func TestPostgresRunBriefFrozenAndChecksOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4701
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "b1")
	repo := NewRunChainsRepository(pool)

	first := startBriefChain(t, pool, service, conversation, user, "31111111-2222-4333-8444-555555555551", "", "")
	root := first.ExecutionID
	chain, found, err := repo.ChainBrief(ctx, root)
	if err != nil || !found || chain.State != unattended.BriefPending || chain.Brief != nil ||
		chain.OwnerUserID != user || chain.Origin != unattended.OriginDesktop {
		t.Fatalf("chain at start = %+v, %v, %v", chain, found, err)
	}

	brief := runbrief.Brief{Goal: "Make the login test pass.", DoneWhen: []runbrief.Condition{
		{ID: "c1", Text: "The login test passes."}, {ID: "c2", Text: "The change is committed."},
	}, Constraints: []string{}, Assumptions: []string{}, Ambiguity: runbrief.AmbiguityLow}
	saved, err := repo.SaveBrief(ctx, root, unattended.BriefReady, &brief)
	if err != nil || saved.State != unattended.BriefReady || saved.Brief == nil || saved.Brief.Goal != brief.Goal {
		t.Fatalf("save = %+v, %v", saved, err)
	}
	// Frozen: a second save changes nothing.
	again, err := repo.SaveBrief(ctx, root, unattended.BriefFailed, nil)
	if err != nil || again.State != unattended.BriefReady || again.Brief == nil {
		t.Fatalf("second save = %+v, %v", again, err)
	}
	var digest string
	if err := pool.QueryRow(ctx, `SELECT brief_sha256 FROM elitea_runtime.run_chains WHERE root_execution_id = $1`,
		root).Scan(&digest); err != nil || len(digest) != 64 {
		t.Fatalf("digest = %q, %v", digest, err)
	}

	// A check is written once, with the counts.
	if stored, isLink, err := repo.LinkCheck(ctx, root, root); err != nil || !isLink || stored != nil {
		t.Fatalf("link before the check = %+v, %v, %v", stored, isLink, err)
	}
	notMet := runbrief.Result{Outcome: runbrief.OutcomeNotMet, Met: 1, Total: 2, Reason: "still fails",
		ContinueText: "Continue from where you stopped.", Conditions: []runbrief.ConditionResult{
			{ID: "c1", Text: "The login test passes.", Verdict: runbrief.VerdictNotMet, Reason: "still fails"},
			{ID: "c2", Text: "The change is committed.", Verdict: runbrief.VerdictMet, Reason: "committed"},
		}}
	recorded, err := repo.RecordCheck(ctx, root, root, notMet, map[string]int{"c1": 1})
	if err != nil || recorded.Outcome != runbrief.OutcomeNotMet {
		t.Fatalf("record = %+v, %v", recorded, err)
	}
	replay, err := repo.RecordCheck(ctx, root, root, runbrief.Result{Outcome: runbrief.OutcomeMet}, map[string]int{})
	if err != nil || replay.Outcome != runbrief.OutcomeNotMet || replay.ContinueText != notMet.ContinueText {
		t.Fatalf("replayed record = %+v, %v", replay, err)
	}
	chain, _, err = repo.ChainBrief(ctx, root)
	if err != nil || chain.Failures["c1"] != 1 || chain.LastCheck == nil || chain.LastCheck.Reason != "still fails" {
		t.Fatalf("chain after the check = %+v, %v", chain, err)
	}
	if _, isLink, err := repo.LinkCheck(ctx, root, "ffffffffffffffffffffffffffffffff"); err != nil || isLink {
		t.Fatalf("a foreign execution is a link: %v, %v", isLink, err)
	}
	if _, err := repo.RecordCheck(ctx, root, "ffffffffffffffffffffffffffffffff", notMet, nil); err == nil {
		t.Fatal("a check of a foreign execution was recorded")
	}

	// The goal_not_met continuation is a link of its kind.
	next := startBriefChain(t, pool, service, conversation, user, "31111111-2222-4333-8444-555555555552", root,
		unattended.LinkGoalNotMet)
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM elitea_runtime.run_chain_links WHERE execution_id = $1`,
		next.ExecutionID).Scan(&kind); err != nil || kind != unattended.LinkGoalNotMet {
		t.Fatalf("kind = %q, %v", kind, err)
	}
}

// TestPostgresRunBriefCallsCountAgainstTheChainSpend: the request log rows
// of the brief and check calls, signed run_brief:<root> and
// goal_check:<link>, count against the chain's spend.
func TestPostgresRunBriefCallsCountAgainstTheChainSpend(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4702
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "b2")
	root := startBriefChain(t, pool, service, conversation, user, "41111111-2222-4333-8444-555555555551", "", "").
		ExecutionID
	for _, row := range []struct {
		id  string
		usd float64
	}{
		{runbrief.AttributionBriefPrefix + root, 0.25},
		{runbrief.AttributionCheckPrefix + root, 0.5},
		{"run_brief:other", 9},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms, provider, model, execution_id, operation, cost_usd)
VALUES (now(), 1, $3, '/tools', 'POST', 200, 10, 'firecrawl', 'scrape', $1, 'tool', $2)`, row.id, row.usd, user); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	startBriefChain(t, pool, service, conversation, user, "41111111-2222-4333-8444-555555555552", root,
		unattended.LinkStepLimit)
	var spend float64
	if err := pool.QueryRow(ctx, `SELECT spend_usd::float8 FROM elitea_runtime.run_chains WHERE root_execution_id = $1`,
		root).Scan(&spend); err != nil || spend != 0.75 {
		t.Fatalf("spend = %v, %v", spend, err)
	}
}

func TestPostgresRunBriefStateFollowsTheGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	const user int64 = 4703
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "b3")
	outcome, err := service.Start(t.Context(), localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "91", ConversationUUID: conversation,
		QuestionID: "51111111-2222-4333-8444-555555555551", UserInput: "hi",
		Unattended: &unattended.Request{GrantID: "5c1d2e3f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", ContinueStepLimit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	chain, _, err := NewRunChainsRepository(pool).ChainBrief(t.Context(), outcome.ExecutionID)
	if err != nil || chain.State != unattended.BriefNone {
		t.Fatalf("goal_check false = %+v, %v", chain, err)
	}
}

// TestPostgresRunBriefReplayedAdmissionKeepsTheSettledMeta: a replayed
// first-run admission of a cloud chain whose brief is ready does not write
// meta.run_brief back to pending.
func TestPostgresRunBriefReplayedAdmissionKeepsTheSettledMeta(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4704
	conversation := seedLocalTurnConversation(t, pool, user)
	var response string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, created_at)
SELECT gen_random_uuid(), m.participant_id, c.id, '{}'::jsonb, now() AT TIME ZONE 'UTC'
FROM p_1.chat_conversations AS c
JOIN p_1.chat_participant_mapping AS m ON m.conversation_id = c.id
WHERE c.uuid = $1::uuid LIMIT 1
RETURNING uuid::text`, conversation).Scan(&response); err != nil {
		t.Fatal(err)
	}
	root := strings.Repeat("e", 32)
	admission := agentexecutionapp.Admission{
		Record: executiondomain.Admission{Job: executiondomain.Job{ID: root}},
		Unattended: &agentexecutionapp.UnattendedAdmission{Start: &unattended.ChainStart{
			ProjectID: 1, OwnerUserID: user, ConversationUUID: conversation, Origin: unattended.OriginCloud,
			Grant:             unattended.Grant{GrantID: "7c1d2e3f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", GoalCheck: true},
			Limits:            unattended.Limits{MaxContinuations: 3, Deadline: time.Hour},
			BriefState:        unattended.BriefPending,
			ResponseMessageID: response,
		}},
	}
	admit := func() {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return admitAgentChain(ctx, tx, admission, 1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, `
SELECT COALESCE(meta->'run_brief'->>'state', '') FROM p_1.chat_message_group WHERE uuid = $1::uuid`,
			response).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	admit()
	if got := state(); got != unattended.BriefPending {
		t.Fatalf("meta at start = %q", got)
	}
	brief := runbrief.Brief{Goal: "Say hello.", DoneWhen: []runbrief.Condition{}, Constraints: []string{},
		Assumptions: []string{}, Ambiguity: runbrief.AmbiguityLow}
	if _, err := NewRunChainsRepository(pool).SaveBrief(ctx, root, unattended.BriefReady, &brief); err != nil {
		t.Fatal(err)
	}
	admit()
	if got := state(); got != unattended.BriefReady {
		t.Fatalf("meta after a replay = %q, want ready", got)
	}
}
