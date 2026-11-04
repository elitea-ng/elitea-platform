package repos

// Send and continue, take back when done (ADR-0045 decision 8, shared 0224)
// against a REAL database (ELITEA_TEST_DATABASE_URL): the complete, the
// person's message and the cloud turn's start commit together or not at all;
// a handoff link carries a desktop chain into the cloud with its limits; the
// flag's lifecycle; and the last cloud run of the state.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	agentapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// handoffContinueFixture is a Local work thread with a send in HANDING_OFF
// whose snapshot the store holds.
type handoffContinueFixture struct {
	harness     handoffHarness
	thread      workspaceThreadFixture
	model       int64
	handoffID   string
	epoch       int64
	snapshotSHA string
}

func newHandoffContinueFixture(t *testing.T, pool *pgxpool.Pool, owner int64) handoffContinueFixture {
	t.Helper()
	ctx := context.Background()
	h := newHandoffHarness(t, pool)
	thread :=seedLocalWorkThread(t, pool, owner, "local_work")
	var model int64
	if err := pool.QueryRow(ctx, `
SELECT p.id FROM p_1.chat_participants p
JOIN p_1.chat_participant_mapping m ON m.participant_id = p.id
WHERE m.conversation_id = $1 AND p.entity_name = 'dummy'`, thread.conversationID).Scan(&model); err != nil {
		t.Fatal(err)
	}
	began, err := h.service.Begin(ctx, beginRequest(thread.thread, owner))
	if err != nil {
		t.Fatal(err)
	}
	sha := h.publishSnapshot(t, thread.thread, owner, began.Handoff.Epoch, uuid.NewString())
	return handoffContinueFixture{harness: h, thread: thread, model: model, handoffID: began.Handoff.HandoffID,
		epoch: began.Handoff.Epoch, snapshotSHA: sha}
}

// submit is the continue turn's admission, as the start use case builds it
// for a model participant.
func (f handoffContinueFixture) submit(owner int64, sha string, chain *agentapp.UnattendedAdmission) agentapp.SubmitRequest {
	question := agentapp.HandoffContinueQuestionID(f.handoffID, f.epoch)
	response := agentapp.HandoffContinueResponseID(question)
	input := agentCapacityInput(question)
	input.UserInput = []byte(`"Finish the migration."`)
	input.ExecutionGeneration = proto.String(question)
	actor := strconv.FormatInt(owner, 10)
	return agentapp.SubmitRequest{
		Identity:       executionapp.AdmissionIdentity{TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", ActorID: actor},
		IdempotencyKey: question, CapabilityID: executiondomain.AgentAdhocCapability,
		ClientStreamID: f.thread.thread.ConversationUUID, ClientMessageID: response, SIOEvent: "chat_predict", Input: input,
		CurrentAdhocTurn: &agentapp.CurrentAdhocTurn{
			ProjectID: 1, ActorUserID: owner, ConversationUUID: f.thread.thread.ConversationUUID,
			TargetParticipantID: f.model, QuestionID: question, QuestionItemID: uuid.NewString(),
			ResponseMessageID: response, QuestionMeta: []byte(`{}`), UserInput: "Finish the migration.",
		},
		Unattended: chain,
		HandoffCompletion: &agentapp.HandoffCompletion{
			HandoffID: f.handoffID, Epoch: f.epoch, HandoffRef: workspacestore.RefHandoff1, SHA: sha,
			TakeBackWhenDone: true,
		},
	}
}

// fenceOpen is the start's HANDING_OFF fence, in the tenant schema.
func (f handoffContinueFixture) fenceOpen(t *testing.T, handoffID string, epoch int64) bool {
	t.Helper()
	ctx := context.Background()
	conversation, err := currentPGUUID(f.thread.thread.ConversationUUID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.harness.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO p_1, public`); err != nil {
		t.Fatal(err)
	}
	open, err := pgxExecutor{queryer: tx}.HandingOffFence(ctx, 1, conversation, handoffID, epoch)
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// handoffContinueAdmissions is the production admission writer with
// execution ids in the runtime's shape (32 hex), which a chain link requires.
func handoffContinueAdmissions(t *testing.T, pool *pgxpool.Pool) *agentapp.AdmissionService {
	t.Helper()
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 1024
	repo, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	newID := func() (string, error) { return uuidHex32(), nil }
	factory, err := agentapp.NewInputBundleFactory(agentapp.InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1"}, newID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentapp.NewAdmissionService(repo, factory, time.Now, newID)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// seedHandoffContinueSchema creates the legacy-owned tables a model turn's
// insert reads (once per database), past the ids the seed takes.
func seedHandoffContinueSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedCurrentAgentContinuationSchema(t, pool)
	if _, err := pool.Exec(context.Background(), `
SELECT setval(pg_get_serial_sequence('p_1.chat_conversations', 'id'), 1000);
SELECT setval(pg_get_serial_sequence('p_1.chat_participants', 'id'), 1000)`); err != nil {
		t.Fatal(err)
	}
}

func (f handoffContinueFixture) read(t *testing.T) workspacehandoff.Handoff {
	t.Helper()
	view, err := f.harness.service.State(context.Background(), f.thread.thread)
	if err != nil || !view.Present {
		t.Fatalf("state = %+v, %v", view, err)
	}
	return view.Handoff
}

func countTurnRows(t *testing.T, pool *pgxpool.Pool, conversationID int64) (groups, jobs int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM p_1.chat_message_group WHERE conversation_id = $1 AND meta ->> 'workspace_event' IS NULL),
       (SELECT count(*) FROM elitea_runtime.agent_execution_jobs a
        JOIN p_1.chat_conversations c ON c.uuid::text = a.client_stream_id WHERE c.id = $1)`,
		conversationID).Scan(&groups, &jobs); err != nil {
		t.Fatal(err)
	}
	return groups, jobs
}

func TestPostgresHandoffContinueCommitsTheSendAndTheTurnTogether(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5401
	f := newHandoffContinueFixture(t, pool, owner)
	admissions := handoffContinueAdmissions(t, pool)

	// A refused complete (a snapshot the store does not hold) admits
	// nothing: the send stays HANDING_OFF and the thread has no new turn.
	if _, err := admissions.Submit(ctx, f.submit(owner, sha1Of("not published"), nil)); !errors.Is(err, workspacehandoff.ErrRefsMissing) {
		t.Fatalf("continue with an unpublished snapshot = %v, want ErrRefsMissing", err)
	}
	if got := f.read(t); got.State != workspacestore.StateHandingOff || got.Epoch != f.epoch || got.TakeBackWhenDone {
		t.Fatalf("after a refused continue = %+v, want HANDING_OFF at epoch %d", got, f.epoch)
	}
	if groups, jobs := countTurnRows(t, pool, f.thread.conversationID); groups != 0 || jobs != 0 {
		t.Fatalf("a refused continue wrote %d messages and %d executions", groups, jobs)
	}

	// The start's fence opens for this send at this epoch only.
	if !f.fenceOpen(t, f.handoffID, f.epoch) || f.fenceOpen(t, f.handoffID, f.epoch+1) ||
		f.fenceOpen(t, uuid.NewString(), f.epoch) {
		t.Fatal("the HANDING_OFF fence opens for the wrong send or epoch")
	}
	outcome, err := admissions.Submit(ctx, f.submit(owner, f.snapshotSHA, nil))
	if err != nil || !outcome.Created {
		t.Fatalf("continue = %+v, %v", outcome, err)
	}
	got := f.read(t)
	question := agentapp.HandoffContinueQuestionID(f.handoffID, f.epoch)
	response := agentapp.HandoffContinueResponseID(question)
	if got.State != workspacestore.StateCloudRunning || got.Epoch != f.epoch+1 || !got.TakeBackWhenDone ||
		got.RunningExecutionID != outcome.ExecutionID || got.RunningResponseID != response {
		t.Fatalf("after continue = %+v, want CLOUD_RUNNING bound to %s", got, outcome.ExecutionID)
	}
	// The event message comes before the person's message, and the answer
	// carries the cloud markers of the new epoch.
	var eventFirst bool
	var executedBy, epoch string
	if err := pool.QueryRow(ctx, `
SELECT (SELECT max(created_at) FROM p_1.chat_message_group WHERE conversation_id = $1 AND meta ->> 'workspace_event' = 'handoff.complete')
     < (SELECT created_at FROM p_1.chat_message_group WHERE uuid = $2::uuid),
       (SELECT meta ->> 'executed_by' FROM p_1.chat_message_group WHERE uuid = $3::uuid),
       (SELECT meta ->> 'workspace_epoch' FROM p_1.chat_message_group WHERE uuid = $3::uuid)`,
		f.thread.conversationID, question, response).Scan(&eventFirst, &executedBy, &epoch); err != nil {
		t.Fatal(err)
	}
	if !eventFirst || executedBy != "cloud_executor" || epoch != strconv.FormatInt(f.epoch+1, 10) {
		t.Fatalf("event first=%t executed_by=%q epoch=%q", eventFirst, executedBy, epoch)
	}
	// The worker's claim redeems the running turn: admitted after the send
	// completed.
	var admittedAfter bool
	if err := pool.QueryRow(ctx, `
SELECT j.admitted_at >= date_trunc('milliseconds', (SELECT g.created_at AT TIME ZONE 'UTC' FROM p_1.chat_message_group g
       WHERE g.conversation_id = $2 AND g.meta ->> 'workspace_event' = 'handoff.complete'))
FROM elitea_runtime.execution_jobs j WHERE j.execution_id = $1`, outcome.ExecutionID, f.thread.conversationID).Scan(&admittedAfter); err != nil {
		t.Fatal(err)
	}
	if !admittedAfter {
		t.Fatal("the continue turn was admitted before the send completed")
	}
	// A retried complete with continue: the start's fence no longer opens
	// (the send is not HANDING_OFF), and a replay of the admission itself
	// answers the same turn without completing anything again.
	if f.fenceOpen(t, f.handoffID, f.epoch) {
		t.Fatal("the fence opens after the send completed")
	}
	replay, err := admissions.Submit(ctx, f.submit(owner, f.snapshotSHA, nil))
	if err != nil || replay.Created || replay.ExecutionID != outcome.ExecutionID {
		t.Fatalf("a replayed continue admission = %+v, %v", replay, err)
	}
	if again := f.read(t); again.Epoch != got.Epoch || again.State != workspacestore.StateCloudRunning {
		t.Fatalf("after a replayed admission = %+v", again)
	}

	// The last cloud run: none while the turn runs; completed once it
	// settles; failed when its answer is an error.
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := repo.LastCloudRun(ctx, got); err != nil || run != nil {
		t.Fatalf("last cloud run while running = %+v, %v", run, err)
	}
	settled := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = $2 WHERE execution_id = $1`,
		outcome.ExecutionID, settled); err != nil {
		t.Fatal(err)
	}
	run, err := repo.LastCloudRun(ctx, got)
	if err != nil || run == nil || run.Outcome != workspacehandoff.CloudRunCompleted ||
		run.ExecutionID != outcome.ExecutionID || !run.FinishedAt.Equal(settled) {
		t.Fatalf("last cloud run = %+v, %v", run, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_message_group SET meta = meta || '{"is_error": true}' WHERE uuid = $1::uuid`,
		response); err != nil {
		t.Fatal(err)
	}
	if run, err := repo.LastCloudRun(ctx, got); err != nil || run == nil || run.Outcome != workspacehandoff.CloudRunFailed {
		t.Fatalf("last cloud run of an error answer = %+v, %v", run, err)
	}
	// Another epoch has no run yet.
	other := got
	other.Epoch++
	if run, err := repo.LastCloudRun(ctx, other); err != nil || run != nil {
		t.Fatalf("last cloud run of another epoch = %+v, %v", run, err)
	}

	// An erase clears take back when done.
	if _, err := f.harness.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: f.thread.thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	var flag bool
	if err := pool.QueryRow(ctx, `
SELECT take_back_when_done FROM elitea_runtime.workspace_handoffs WHERE project_id = 1 AND conversation_uuid = $1::uuid`,
		f.thread.thread.ConversationUUID).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag {
		t.Fatal("an erase kept take_back_when_done")
	}
}

// seedDesktopChain starts a desktop chain whose root is a local turn of the
// thread.
func seedDesktopChain(t *testing.T, pool *pgxpool.Pool, f handoffContinueFixture, owner int64, maxContinuations int) unattended.Chain {
	t.Helper()
	ctx := context.Background()
	root := uuidHex32()
	grant := unattended.Grant{GrantID: uuid.NewString(), ContinueStepLimit: true, ContinueToolCallCap: true}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := startRunChain(ctx, tx, unattended.ChainStart{
		RootExecutionID: root, ProjectID: 1, OwnerUserID: owner, ConversationUUID: f.thread.thread.ConversationUUID,
		Origin: unattended.OriginDesktop, TriggerOrigin: "manual", Grant: grant,
		Limits: unattended.Limits{MaxContinuations: maxContinuations, Deadline: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	chain, found, err := NewRunChainsRepository(pool).LatestChainLink(ctx, root)
	if err != nil || !found || chain.Origin != unattended.OriginDesktop {
		t.Fatalf("latest link = %+v, %t, %v", chain, found, err)
	}
	return chain
}

func uuidHex32() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// handoffContinuation is the chain admission the start use case builds for
// a handoff link (unattendedContinuationGrant).
func handoffContinuation(chain unattended.Chain, f handoffContinueFixture, owner int64) *agentapp.UnattendedAdmission {
	question := agentapp.HandoffContinueQuestionID(f.handoffID, f.epoch)
	response := agentapp.HandoffContinueResponseID(question)
	return &agentapp.UnattendedAdmission{
		Continue: &unattended.ChainContinue{
			RootExecutionID: chain.RootExecutionID, Kind: unattended.LinkHandoff, OwnerUserID: owner,
			GrantID: chain.GrantID, ProjectID: 1, ConversationUUID: chain.ConversationUUID,
			ResponseMessageID: response,
			Start:             &unattended.CloudStart{Contract: agentapp.CloudStartContractAdhoc, ParticipantID: f.model},
		},
		MarkMessageIDs: []string{question, response},
	}
}

func TestPostgresHandoffContinueCarriesTheDesktopChainIntoTheCloud(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5402
	admissions := handoffContinueAdmissions(t, pool)

	// A chain at its limit refuses the turn, and the send stays HANDING_OFF.
	full := newHandoffContinueFixture(t, pool, owner)
	spent := seedDesktopChain(t, pool, full, owner, 0)
	_, err := admissions.Submit(ctx, full.submit(owner, full.snapshotSHA, handoffContinuation(spent, full, owner)))
	var limit *unattended.ChainLimitError
	if !errors.As(err, &limit) || limit.Limit != unattended.LimitContinuations {
		t.Fatalf("continue past the chain's limit = %v, want chain_limit_reached continuations", err)
	}
	if got := full.read(t); got.State != workspacestore.StateHandingOff {
		t.Fatalf("after a refused chain = %+v, want HANDING_OFF", got)
	}
	if groups, jobs := countTurnRows(t, pool, full.thread.conversationID); groups != 0 || jobs != 0 {
		t.Fatalf("a refused chain wrote %d messages and %d executions", groups, jobs)
	}

	// The handoff link continues the chain: one continuation counted, the
	// chain's origin is now cloud, and its later cloud continuations start
	// as this turn did.
	f := newHandoffContinueFixture(t, pool, owner)
	chain := seedDesktopChain(t, pool, f, owner, 3)
	outcome, err := admissions.Submit(ctx, f.submit(owner, f.snapshotSHA, handoffContinuation(chain, f, owner)))
	if err != nil {
		t.Fatalf("continue the chain = %v", err)
	}
	latest, found, err := NewRunChainsRepository(pool).LatestChainLink(ctx, chain.RootExecutionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if latest.ExecutionID != outcome.ExecutionID || latest.Kind != unattended.LinkHandoff ||
		latest.Origin != unattended.OriginCloud || latest.Continuations != 1 || latest.ContinuationIndex != 1 ||
		latest.Grant.Start == nil || latest.Grant.Start.ParticipantID != f.model ||
		latest.Grant.Start.Contract != agentapp.CloudStartContractAdhoc {
		t.Fatalf("latest link = %+v (start %+v)", latest, latest.Grant.Start)
	}
	if got := f.read(t); got.State != workspacestore.StateCloudRunning || got.RunningExecutionID != outcome.ExecutionID {
		t.Fatalf("after the chain continue = %+v", got)
	}

	// The chain's end is the state's last cloud run.
	if _, err := NewRunChainsRepository(pool).EndChain(ctx, chain.RootExecutionID, unattended.EndCompleted, ""); err != nil {
		t.Fatal(err)
	}
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.LastCloudRun(ctx, f.read(t))
	if err != nil || run == nil || run.Outcome != workspacehandoff.CloudRunCompleted || run.ExecutionID != outcome.ExecutionID {
		t.Fatalf("last cloud run of the ended chain = %+v, %v", run, err)
	}
	// The state answers it while take_back_when_done is set, and never reads
	// it once the flag is cleared.
	f.harness.service.WithCloudRuns(repo)
	view, err := f.harness.service.State(ctx, f.thread.thread)
	if err != nil || !view.Handoff.TakeBackWhenDone || view.LastCloudRun == nil ||
		view.LastCloudRun.ExecutionID != outcome.ExecutionID {
		t.Fatalf("state with take back = %+v, %v", view.LastCloudRun, err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.workspace_handoffs SET take_back_when_done = FALSE
WHERE project_id = 1 AND conversation_uuid = $1::uuid`, f.thread.thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	if view, err := f.harness.service.State(ctx, f.thread.thread); err != nil || view.LastCloudRun != nil {
		t.Fatalf("state without take back = %+v, %v; want no last cloud run", view.LastCloudRun, err)
	}
}

// completeStopped completes the fixture's send without `continue`, naming
// the stopped chain.
func (f handoffContinueFixture) completeStopped(t *testing.T, owner int64, root string) workspacehandoff.Handoff {
	t.Helper()
	handoff, err := f.harness.service.Complete(context.Background(), workspacehandoff.CompleteRequest{
		ProjectID: 1, HandoffID: f.handoffID, ActorUserID: owner, Epoch: f.epoch,
		HandoffRef: workspacestore.RefHandoff1, SHA: f.snapshotSHA, AuditRoute: "/complete",
		StoppedChainRootExecutionID: root,
	})
	if err != nil {
		t.Fatalf("complete with the stopped chain = %v", err)
	}
	if handoff.State != workspacestore.StateCloudIdle {
		t.Fatalf("complete = %+v, want CLOUD_IDLE", handoff)
	}
	return handoff
}

func chainEndOf(t *testing.T, pool *pgxpool.Pool, root string) (state *string, origin string, notified int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT end_state, origin,
       (SELECT count(*) FROM centry.notifications
        WHERE event_type = 'run_finished' AND meta ->> 'chain_root_execution_id' = $1
          AND meta ->> 'outcome' = 'waiting' AND meta ->> 'origin' = 'cloud')
FROM elitea_runtime.run_chains WHERE root_execution_id = $1`, root).Scan(&state, &origin, &notified); err != nil {
		t.Fatal(err)
	}
	return state, origin, notified
}

// A send that does not continue ends the stopped desktop chain as `waiting`
// in the complete's transaction (its run_finished names the cloud, where
// the thread now waits). A chain that is not the caller's, not this
// thread's or no longer open is left alone, and the send still completes.
func TestPostgresHandoffCompleteEndsTheStoppedChainAsWaiting(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedHandoffContinueSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 5403

	f := newHandoffContinueFixture(t, pool, owner)
	chain := seedDesktopChain(t, pool, f, owner, 3)
	if got := f.completeStopped(t, owner, chain.RootExecutionID); !got.StoppedChainEnded {
		t.Fatalf("complete = %+v, want the stopped chain ended", got)
	}
	state, origin, notified := chainEndOf(t, pool, chain.RootExecutionID)
	if state == nil || *state != unattended.EndWaiting || origin != unattended.OriginCloud || notified != 1 {
		t.Fatalf("stopped chain = %v, origin %s, run_finished %d; want waiting, cloud, 1", state, origin, notified)
	}

	// Another person's chain on another thread: not ended, the send completes.
	other := newHandoffContinueFixture(t, pool, owner+1)
	foreign := seedDesktopChain(t, pool, other, owner+1, 3)
	g := newHandoffContinueFixture(t, pool, owner)
	if got := g.completeStopped(t, owner, foreign.RootExecutionID); got.StoppedChainEnded {
		t.Fatal("a complete ended another person's chain")
	}
	if state, _, _ := chainEndOf(t, pool, foreign.RootExecutionID); state != nil {
		t.Fatalf("another person's chain = %s, want open", *state)
	}

	// The caller's chain of another thread: not ended.
	h := newHandoffContinueFixture(t, pool, owner)
	elsewhere := seedDesktopChain(t, pool, f, owner, 3)
	if got := h.completeStopped(t, owner, elsewhere.RootExecutionID); got.StoppedChainEnded {
		t.Fatal("a complete ended a chain of another thread")
	}

	// A chain that already ended (chain_limit_reached ended it): unchanged.
	k := newHandoffContinueFixture(t, pool, owner)
	ended := seedDesktopChain(t, pool, k, owner, 3)
	if _, err := NewRunChainsRepository(pool).EndChain(ctx, ended.RootExecutionID, unattended.EndLimitReached,
		unattended.LimitContinuations); err != nil {
		t.Fatal(err)
	}
	if got := k.completeStopped(t, owner, ended.RootExecutionID); got.StoppedChainEnded {
		t.Fatal("a complete ended a chain twice")
	}
	if state, origin, _ := chainEndOf(t, pool, ended.RootExecutionID); state == nil ||
		*state != unattended.EndLimitReached || origin != unattended.OriginDesktop {
		t.Fatalf("an ended chain = %v, %s; want limit_reached, desktop", state, origin)
	}

	// No chain of that id: the send completes.
	m := newHandoffContinueFixture(t, pool, owner)
	if got := m.completeStopped(t, owner, uuidHex32()); got.StoppedChainEnded {
		t.Fatal("a complete ended a chain that does not exist")
	}
}
