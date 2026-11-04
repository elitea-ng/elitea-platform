package repos

// Schedules and webhook triggers that target a handed-off workspace thread
// (ADR-0045 decision 9) against a REAL database (ELITEA_TEST_DATABASE_URL):
// the thread read, the overlap read, the CLOUD_IDLE fence in the admission
// transaction, one turn for two racing questions, and the run_finished of an
// occurrence's chain.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	agentapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

// question is a new question of the thread, as the start use case admits a
// posted message or an occurrence on a CLOUD_IDLE thread: no completion,
// and the CLOUD_IDLE fence.
func (f handoffContinueFixture) question(owner int64, chain *agentapp.UnattendedAdmission) agentapp.SubmitRequest {
	request := f.submit(owner, f.snapshotSHA, chain)
	question, response := uuid.NewString(), uuid.NewString()
	input := agentCapacityInput(question)
	input.UserInput = []byte(`"Run the nightly checks."`)
	input.ExecutionGeneration = proto.String(question)
	turn := *request.CurrentAdhocTurn
	turn.QuestionID, turn.ResponseMessageID, turn.QuestionItemID = question, response, uuid.NewString()
	turn.UserInput = "Run the nightly checks."
	request.CurrentAdhocTurn = &turn
	request.IdempotencyKey, request.ClientMessageID, request.Input = question, response, input
	request.HandoffCompletion = nil
	request.CloudThread = true
	return request
}

func TestPostgresThreadTargetReadsTheHandedOffThread(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5501
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	f := newHandoffContinueFixture(t, pool, owner)
	conversation := f.thread.thread.ConversationUUID

	// HANDING_OFF: a handed-off thread, not idle.
	target, err := repo.ThreadTarget(ctx, 1, conversation)
	if err != nil || target.Idle || target.Agent || target.ParticipantID != f.model || target.ConversationUUID != conversation {
		t.Fatalf("handing off = %+v, %v", target, err)
	}
	f.completeStopped(t, owner, "")
	if target, err = repo.ThreadTarget(ctx, 1, conversation); err != nil || !target.Idle {
		t.Fatalf("cloud idle = %+v, %v", target, err)
	}
	// An answer of the thread that still streams: not idle.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, conversation_id, author_participant_id, is_streaming, meta, created_at)
VALUES (gen_random_uuid(), $1, $2, TRUE, '{}'::jsonb, now() AT TIME ZONE 'UTC')`,
		f.thread.conversationID, f.model); err != nil {
		t.Fatal(err)
	}
	if target, err = repo.ThreadTarget(ctx, 1, conversation); err != nil || target.Idle {
		t.Fatalf("streaming = %+v, %v", target, err)
	}
	// Another project, or a conversation never handed off: not found.
	if _, err := repo.ThreadTarget(ctx, 2, conversation); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("another project = %v", err)
	}
	plain := seedLocalWorkThread(t, pool, owner, "local_work")
	if _, err := repo.ThreadTarget(ctx, 1, plain.thread.ConversationUUID); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("never handed off = %v", err)
	}
	if _, err := repo.ThreadTarget(ctx, 1, uuid.NewString()); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("no conversation = %v", err)
	}
}

// The fence in the admission transaction, and one turn for two racing
// questions (an occurrence and a posted message are the same admission).
func TestPostgresCloudThreadAdmissionIsFencedAndSingle(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5502
	admissions := handoffContinueAdmissions(t, pool)
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	f := newHandoffContinueFixture(t, pool, owner)
	conversation := f.thread.thread.ConversationUUID

	// HANDING_OFF: the fence refuses, and nothing is written.
	if _, err := admissions.Submit(ctx, f.question(owner, nil)); !errors.Is(err, agentapp.ErrLocalWorkThread) {
		t.Fatalf("handing off = %v, want ErrLocalWorkThread", err)
	}
	if groups, jobs := countTurnRows(t, pool, f.thread.conversationID); groups != 0 || jobs != 0 {
		t.Fatalf("a fenced question wrote %d messages and %d executions", groups, jobs)
	}

	f.completeStopped(t, owner, "")
	var wait sync.WaitGroup
	results := make([]error, 2)
	outcomes := make([]string, 2)
	for i := range results {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			outcome, err := admissions.Submit(ctx, f.question(owner, nil))
			results[i], outcomes[i] = err, outcome.ExecutionID
		}(i)
	}
	wait.Wait()
	admitted := ""
	for i, err := range results {
		if err == nil {
			if admitted != "" {
				t.Fatal("two racing questions both started a turn")
			}
			admitted = outcomes[i]
		}
	}
	if admitted == "" {
		t.Fatalf("no question was admitted: %v", results)
	}
	if _, jobs := countTurnRows(t, pool, f.thread.conversationID); jobs != 1 {
		t.Fatalf("executions = %d, want 1", jobs)
	}

	// The overlap read: the run goes until its execution settles.
	if active, err := repo.RunActive(ctx, 1, admitted); err != nil || !active {
		t.Fatalf("running = %v, %v", active, err)
	}
	if active, err := repo.RunActive(ctx, 2, admitted); err != nil || active {
		t.Fatalf("another project = %v, %v", active, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = now()
WHERE execution_id = $1`, admitted); err != nil {
		t.Fatal(err)
	}
	if active, err := repo.RunActive(ctx, 1, admitted); err != nil || active {
		t.Fatalf("settled = %v, %v", active, err)
	}
	if active, err := repo.RunActive(ctx, 1, uuidHex32()); err != nil || active {
		t.Fatalf("unknown = %v, %v", active, err)
	}

	// CLOUD_RUNNING: the fence names the running turn.
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_message_group SET is_streaming = FALSE WHERE conversation_id = $1`,
		f.thread.conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET state = 'CLOUD_RUNNING', running_execution_id = $2
WHERE project_id = 1 AND conversation_uuid = $1::uuid`, conversation, admitted); err != nil {
		t.Fatal(err)
	}
	if _, err := admissions.Submit(ctx, f.question(owner, nil)); !errors.Is(err, agentapp.ErrCloudTurnRunning) {
		t.Fatalf("cloud running = %v, want ErrCloudTurnRunning", err)
	}
	// A take back pending while the turn runs: refused as Local work.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET reclaim_pending = TRUE
WHERE project_id = 1 AND conversation_uuid = $1::uuid`, conversation); err != nil {
		t.Fatal(err)
	}
	if _, err := admissions.Submit(ctx, f.question(owner, nil)); !errors.Is(err, agentapp.ErrLocalWorkThread) {
		t.Fatalf("reclaim pending = %v, want ErrLocalWorkThread", err)
	}
	if _, jobs := countTurnRows(t, pool, f.thread.conversationID); jobs != 1 {
		t.Fatalf("executions = %d after fenced questions, want 1", jobs)
	}
}

// An occurrence's chain is a cloud chain of the thread; its end reaches the
// owner as run_finished with the origin cloud and the conversation.
func TestPostgresThreadOccurrenceChainNotifiesTheOwner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5503
	admissions := handoffContinueAdmissions(t, pool)
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	f := newHandoffContinueFixture(t, pool, owner)
	f.completeStopped(t, owner, "")
	request := f.question(owner, nil)
	request.Unattended = &agentapp.UnattendedAdmission{Start: &unattended.ChainStart{
		ProjectID: 1, OwnerUserID: owner, ConversationUUID: f.thread.thread.ConversationUUID,
		Origin: unattended.OriginCloud, TriggerOrigin: "schedule",
		Grant:             unattended.Grant{GrantID: uuid.NewString(), ContinueStepLimit: true, ContinueOutputLimit: true},
		Limits:            unattended.Limits{MaxContinuations: 3, Deadline: time.Hour},
		ResponseMessageID: request.CurrentAdhocTurn.ResponseMessageID,
	}}
	outcome, err := admissions.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// The open chain keeps the overlap read active after the run settles.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = now()
WHERE execution_id = $1`, outcome.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if active, err := repo.RunActive(ctx, 1, outcome.ExecutionID); err != nil || !active {
		t.Fatalf("open chain = %v, %v", active, err)
	}
	if ended, err := NewRunChainsRepository(pool).EndChain(ctx, outcome.ExecutionID, unattended.EndCompleted, ""); err != nil || !ended {
		t.Fatalf("end chain = %v, %v", ended, err)
	}
	if active, err := repo.RunActive(ctx, 1, outcome.ExecutionID); err != nil || active {
		t.Fatalf("ended chain = %v, %v", active, err)
	}
	var user int64
	var origin, conversation string
	if err := pool.QueryRow(ctx, `
SELECT user_id, meta ->> 'origin', meta ->> 'conversation_uuid' FROM centry.notifications
WHERE event_type = 'run_finished' AND meta ->> 'chain_root_execution_id' = $1`, outcome.ExecutionID).
		Scan(&user, &origin, &conversation); err != nil {
		t.Fatalf("run_finished: %v", err)
	}
	if user != owner || origin != unattended.OriginCloud || conversation != f.thread.thread.ConversationUUID {
		t.Fatalf("run_finished = user %d, origin %s, conversation %s", user, origin, conversation)
	}
}

// A schedule's turn writes its approval snapshot in its own admission
// transaction, behind the fence: the started turn has the schedule's action
// from its first moment, and a refused occurrence leaves the running turn's
// snapshot as it is.
func TestPostgresThreadRunPolicyIsWrittenWithTheAdmission(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5504
	admissions := handoffContinueAdmissions(t, pool)
	f := newHandoffContinueFixture(t, pool, owner)
	conversation := f.thread.thread.ConversationUUID
	policy := func(action string, version int64) *agentapp.UnattendedRunPolicy {
		return &agentapp.UnattendedRunPolicy{
			ProjectID: 1, ConversationUUID: conversation, TriggerOrigin: "schedule",
			VersionID: version, OwnerUserID: owner, OnApprovalRequired: action,
		}
	}
	read := func() (action string, version int64, found bool) {
		t.Helper()
		err := pool.QueryRow(ctx, `
SELECT on_approval_required, version_id FROM elitea_runtime.unattended_run_policies
WHERE project_id = 1 AND conversation_uuid = $1::uuid`, conversation).Scan(&action, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return action, version, true
	}

	// HANDING_OFF: the fence refuses, and no snapshot is written.
	refused := f.question(owner, nil)
	refused.RunPolicy = policy("skip", 7)
	if _, err := admissions.Submit(ctx, refused); !errors.Is(err, agentapp.ErrLocalWorkThread) {
		t.Fatalf("handing off = %v", err)
	}
	if _, _, found := read(); found {
		t.Fatal("a fenced occurrence wrote a snapshot")
	}

	f.completeStopped(t, owner, "")
	started := f.question(owner, nil)
	started.RunPolicy = policy("skip", 7)
	outcome, err := admissions.Submit(ctx, started)
	if err != nil || !outcome.Created {
		t.Fatalf("start = %+v, %v", outcome, err)
	}
	// One read of the execution row and the snapshot: the snapshot is there
	// when the turn is.
	var action string
	if err := pool.QueryRow(ctx, `
SELECT p.on_approval_required FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.unattended_run_policies p ON p.project_id = 1 AND p.conversation_uuid = $2::uuid
WHERE j.execution_id = $1`, outcome.ExecutionID, conversation).Scan(&action); err != nil || action != "skip" {
		t.Fatalf("snapshot with the turn = %q, %v", action, err)
	}

	// A second occurrence while the answer streams: refused, and the
	// running turn's snapshot stays.
	busy := f.question(owner, nil)
	busy.RunPolicy = policy("notify_and_wait", 8)
	if _, err := admissions.Submit(ctx, busy); err == nil {
		t.Fatal("a second occurrence started while the first streams")
	}
	if action, version, _ := read(); action != "skip" || version != 7 {
		t.Fatalf("snapshot after a refused occurrence = %s, %d", action, version)
	}

	// The next started turn replaces it.
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_message_group SET is_streaming = FALSE WHERE conversation_id = $1`,
		f.thread.conversationID); err != nil {
		t.Fatal(err)
	}
	next := f.question(owner, nil)
	next.RunPolicy = policy("notify_and_wait", 8)
	if _, err := admissions.Submit(ctx, next); err != nil {
		t.Fatal(err)
	}
	if action, version, _ := read(); action != "notify_and_wait" || version != 8 {
		t.Fatalf("snapshot of the next turn = %s, %d", action, version)
	}

	// A snapshot without the cloud thread fence is refused.
	plain := f.question(owner, nil)
	plain.CloudThread = false
	plain.RunPolicy = policy("skip", 7)
	if _, err := admissions.Submit(ctx, plain); !errors.Is(err, agentapp.ErrInvalidAgentAdmission) {
		t.Fatalf("snapshot without the fence = %v", err)
	}
}
