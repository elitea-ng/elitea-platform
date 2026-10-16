package repos

// A handed-off thread's cloud turns, end to end in Main (ADR-0034): the REAL
// gRPC WorkspaceStoreService (under the control listener's strict codec and
// bounds) over the REAL executor adapter (workspaceexecutor) over the REAL
// handoff and store services, against a real PostgreSQL and the real project
// vault. Only the workload authorizer and the live-fence check are stubs:
// they are the control plane's, proven elsewhere.
//
// Begin and complete as a desktop would; the claim starts the cloud turn
// (StartCloudTurn: answer stamped); fetch the plan and the snapshot bytes;
// redeem the env secret; upload and publish; an identical replay, a real
// compare-and-set conflict and a thread_busy that is retried; the turn ends
// at its projection; a regenerate of that newest cloud turn rewinds the cloud
// ref (through the same compare-and-set) and the next claim no longer asks
// for it; the reconciler ends a terminal turn; then take back and release.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceexecutor"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	workspacegrpc "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/workspace"
)

const executorConversation = "10000000-0000-4000-8000-000000000031"

type executorWorkload struct{}

func (executorWorkload) AuthorizeWorkload(context.Context, string, string) (string, error) {
	return "spiffe://elitea/worker", nil
}

type executorFence struct{}

func (executorFence) VerifyActive(context.Context, runtimedomain.Fence) error { return nil }

// executorThreads is cmd/elitea-main's workspaceThreads, here over the same
// repository.
type executorThreads struct{ repo *WorkspaceHandoffsRepository }

func (t executorThreads) Read(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	return t.repo.Read(ctx, thread)
}

func (t executorThreads) Execution(ctx context.Context, executionID string, generation uint64) (workspaceexecutor.Execution, bool, error) {
	execution, ok, err := t.repo.CloudTurnExecution(ctx, executionID, generation)
	return workspaceexecutor.Execution(execution), ok, err
}

func (t executorThreads) FinishedCloudTurns(ctx context.Context, limit int) ([]workspacehandoff.Handoff, error) {
	return t.repo.FinishedCloudTurns(ctx, limit)
}

func (t executorThreads) ReclaimPending(ctx context.Context, executionID string) (bool, error) {
	return t.repo.ReclaimPending(ctx, executionID)
}

func dialExecutorStore(t *testing.T, store workspaceruntime.Store) runtimev1.WorkspaceStoreServiceClient {
	t.Helper()
	server, err := workspacegrpc.NewServer(executorWorkload{}, executorFence{}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := runtimegrpc.NewDirectionalStrictProtoCodec(64*1024, 80*1024)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(runtimegrpc.StrictServerOptions(codec)...)
	runtimev1.RegisterWorkspaceStoreServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return runtimev1.NewWorkspaceStoreServiceClient(connection)
}

func executorClaim(executionID string, epoch uint64) *runtimev1.WorkspaceClaimV1 {
	return &runtimev1.WorkspaceClaimV1{
		Identity: &runtimev1.ExecutionIdentityV1{CommandId: "command-" + executionID, ExecutionId: executionID, Generation: 1},
		Fence: &runtimev1.ExecutionFenceV1{
			WorkloadSessionId: "session-1", ProducerId: "producer-1", ClaimAttempt: 1, LeaseEpoch: 1,
			FenceToken: bytes.Repeat([]byte{7}, 32),
		},
		ClaimId: "claim-" + executionID, ThreadUuid: executorConversation, Epoch: epoch,
	}
}

// seedCloudTurnExecution is an admitted agent execution of the thread whose
// chat answer is response.
func seedCloudTurnExecution(t *testing.T, pool *pgxpool.Pool, executionID, response string) {
	t.Helper()
	ctx := t.Context()
	const inputBundleID, requestEntryID = "input-bundle-cloud-turn", "request"
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES ($1, '1', 'application/x-protobuf', 1, decode(repeat('ab', 32), 'hex'), 2, '{}'::bytea, 'tests', clock_timestamp())
ON CONFLICT (input_bundle_id) DO NOTHING`, inputBundleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ($1, $2, '1', 'request', 'application/json', decode(repeat('cd', 32), 'hex'), 2, 'inline://request',
    'internal', 'worker', '{}'::bytea)
ON CONFLICT (input_bundle_id, entry_id) DO NOTHING`, inputBundleID, requestEntryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES ($1, 1, $2, '1', 1, 1, '11', 'user:11', $3, '1', $4, decode(repeat('ef', 32), 'hex'),
    'cloud-turn', $5, 'RUNNING', 'RUNNING', clock_timestamp())`,
		executionID, "command-"+executionID, executiondomain.AgentApplicationCapability, inputBundleID, "idempotency-"+executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event
) VALUES ($1, 1, $2, $3, $4, $5, $6, '1', 'chat_predict')`,
		executionID, executiondomain.AgentApplicationCapability, inputBundleID, requestEntryID, executorConversation, response); err != nil {
		t.Fatal(err)
	}
}

func fetchExecutorFrames(t *testing.T, client runtimev1.WorkspaceStoreServiceClient, claim *runtimev1.WorkspaceClaimV1) []*runtimev1.WorkspaceFetchFrameV1 {
	t.Helper()
	stream, err := client.WorkspaceFetchPlan(context.Background(), &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim})
	if err != nil {
		t.Fatal(err)
	}
	var frames []*runtimev1.WorkspaceFetchFrameV1
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
}

func uploadExecutorSegment(t *testing.T, client runtimev1.WorkspaceStoreServiceClient, claim *runtimev1.WorkspaceClaimV1, content []byte) *runtimev1.WorkspaceUploadResponseV1 {
	t.Helper()
	stream, err := client.WorkspaceUpload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if err := stream.Send(&runtimev1.WorkspaceUploadFrameV1{Frame: &runtimev1.WorkspaceUploadFrameV1_Header{
		Header: &runtimev1.WorkspaceUploadHeaderV1{Claim: claim, Sha256: hex.EncodeToString(sum[:]), Bytes: uint64(len(content))}}}); err != nil {
		t.Fatal(err)
	}
	for rest := content; len(rest) > 0; {
		size := min(workspacegrpc.ChunkBytes, len(rest))
		if err := stream.Send(&runtimev1.WorkspaceUploadFrameV1{Frame: &runtimev1.WorkspaceUploadFrameV1_Chunk{Chunk: rest[:size]}}); err != nil {
			t.Fatal(err)
		}
		rest = rest[size:]
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func executorPublish(claim *runtimev1.WorkspaceClaimV1, content []byte, old *string, next string) *runtimev1.WorkspacePublishRequestV1 {
	sum := sha256.Sum256(content)
	return &runtimev1.WorkspacePublishRequestV1{
		Claim: claim, Kind: workspacestore.KindCompact, SegmentSha256: hex.EncodeToString(sum[:]), Bytes: uint64(len(content)),
		ObjectFormat: workspacestore.ObjectFormatSHA1,
		Refs:         []*runtimev1.WorkspaceRefUpdateV1{{Name: workspacestore.RefCloud, Old: old, New: next}},
	}
}

func answerMeta(t *testing.T, pool *pgxpool.Pool, response string) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := pool.QueryRow(context.Background(), `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, response).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestPostgresCloudTurnThroughTheGRPCAdapterEndToEnd(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentMutationVault(t, pool)
	seedCurrentAgentContinuationSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 11
	ensureWorkspaceTestUser(t, pool, owner)
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET source = 'local_work' WHERE uuid = $1::uuid`, executorConversation); err != nil {
		t.Fatal(err)
	}
	h, _, reader := newEnvHarnessOn(t, pool)
	thread := workspacestore.Thread{ProjectID: 1, ConversationUUID: executorConversation}
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	// The executor's store: the same database and object store as the
	// desktop's, with its own audit recorder to read the publish rows.
	objects, storeAudit := newWorkspaceStoreService(t, pool, h.store, defaultWorkspaceLimits())
	adapter, err := workspaceexecutor.New(h.service, objects, executorThreads{repo: repo}, workspaceexecutor.Resources{TurnWallSeconds: 600}, nil)
	if err != nil {
		t.Fatal(err)
	}
	contexts := workspaceruntime.NewContextResolver(adapter, adapter)
	client := dialExecutorStore(t, adapter)
	state := func() workspacehandoff.Handoff {
		t.Helper()
		view, err := h.service.State(ctx, thread)
		if err != nil {
			t.Fatal(err)
		}
		return view.Handoff
	}

	// ---- The desktop sends the thread (begin, snapshot, complete). ----
	begin := beginRequest(thread, owner)
	begin.Env = map[string]string{"DATABASE_URL": harvestedValue}
	began, err := h.service.Begin(ctx, begin)
	if err != nil {
		t.Fatal(err)
	}
	snapshotBytes := []byte("snapshot e2e")
	handoffSHA := h.publishSnapshot(t, thread, owner, 1, "e2e")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: began.Handoff.HandoffID,
		ActorUserID: owner, Epoch: 1, HandoffRef: workspacestore.RefHandoff1, SHA: handoffSHA}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got.State != workspacestore.StateCloudIdle || got.Epoch != 2 {
		t.Fatalf("after complete = %s/%d", got.State, got.Epoch)
	}

	// ---- Turn 1: admitted (its answer row and execution), then claimed. ----
	conversationID := mustCurrentPGUUID(t, executorConversation)
	const question1, response1 = "20000000-0000-4000-8000-0000000000e1", "40000000-0000-4000-8000-0000000000e1"
	withTenantTx(t, pool, func(tx pgx.Tx) {
		insertPostgresCurrentApplicationTurn(t, sqlcgen.New(tx), conversationID, question1,
			"30000000-0000-4000-8000-0000000000e1", response1, "fix the build", "exec-cloud-1")
	})
	seedCloudTurnExecution(t, pool, "exec-cloud-1", response1)
	claim1 := workspaceruntime.TurnClaim{ExecutionID: "exec-cloud-1", Generation: 1, ProjectID: 1, ActorID: "11", ConversationUUID: executorConversation}
	field, err := contexts.ResolveWorkspaceContext(ctx, claim1)
	if err != nil || field == nil {
		t.Fatalf("claim 1 = %+v %v", field, err)
	}
	if field.Epoch != 2 || field.ExpectedOldCloud != nil || !field.EnvSecret || field.Rewind || field.RewindTo != nil ||
		field.Thread != executorConversation || field.Limits.UnpackedBytes != 5555 || field.Limits.TurnWallSeconds != 600 {
		t.Fatalf("workspace field = %+v", field)
	}
	if got := state(); got.State != workspacestore.StateCloudRunning || got.Epoch != 2 || got.RunningExecutionID != "exec-cloud-1" ||
		got.RunningResponseID != response1 {
		t.Fatalf("after claim = %+v", got)
	}
	if meta := answerMeta(t, pool, response1); meta["executed_by"] != "cloud_executor" || meta["workspace_epoch"] != float64(2) ||
		meta[workspacehandoff.MetaWorkspaceBase] != handoffSHA {
		t.Fatalf("answer meta at claim = %v", meta)
	}
	// A repeated redemption answers the same turn and starts nothing new.
	if again, err := contexts.ResolveWorkspaceContext(ctx, claim1); err != nil || again.Epoch != 2 {
		t.Fatalf("repeated claim = %+v %v", again, err)
	}
	// Another execution of the thread is refused while this turn runs.
	if _, err := contexts.ResolveWorkspaceContext(ctx, workspaceruntime.TurnClaim{ExecutionID: "exec-other", Generation: 1,
		ProjectID: 1, ConversationUUID: executorConversation}); !errors.Is(err, workspaceruntime.ErrNotCloudTurn) {
		t.Fatalf("a second turn = %v", err)
	}

	// ---- Fetch: the plan, then the snapshot's bytes, over gRPC. ----
	wire1 := executorClaim("exec-cloud-1", 2)
	frames := fetchExecutorFrames(t, client, wire1)
	if len(frames) < 3 || frames[0].GetPlan() == nil {
		t.Fatalf("fetch frames = %v", frames)
	}
	plan := frames[0].GetPlan()
	refs := map[string]string{}
	for _, ref := range plan.GetRefs() {
		refs[ref.GetName()] = ref.GetSha()
	}
	if refs[workspacestore.RefHandoff1] != handoffSHA || len(plan.GetSegments()) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	var streamed []byte
	for _, frame := range frames[1 : len(frames)-1] {
		streamed = append(streamed, frame.GetSegmentChunk()...)
	}
	if !bytes.Equal(streamed, snapshotBytes) || frames[len(frames)-1].GetSegmentEnd() == nil {
		t.Fatalf("segment bytes = %q", streamed)
	}
	// The worker's belief is checked: a stale epoch is refused.
	if stale := fetchExecutorFrames(t, client, executorClaim("exec-cloud-1", 1)); stale[0].GetRefusal().GetCode() !=
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH {
		t.Fatalf("stale fetch = %v", stale)
	}

	// ---- The env secret, redeemed for the running turn only. ----
	envStream, err := client.WorkspaceEnvSecret(ctx, &runtimev1.WorkspaceEnvSecretRequestV1{Claim: wire1})
	if err != nil {
		t.Fatal(err)
	}
	redeemed := map[string]string{}
	for {
		frame, err := envStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range frame.GetVars().GetVars() {
			redeemed[variable.GetName()] = variable.GetValue()
		}
	}
	if redeemed["DATABASE_URL"] != harvestedValue || len(redeemed) != 1 {
		t.Fatalf("redeemed env = %v", redeemed)
	}

	// ---- Upload and publish the turn's commit; replay; conflict. ----
	turnBundle := []byte("cloud turn 1 bundle")
	if response := uploadExecutorSegment(t, client, wire1, turnBundle); response.GetRejection() != nil || response.GetRefusal() != nil || response.GetExists() {
		t.Fatalf("upload = %v", response)
	}
	commit1 := sha1Of("cloud commit 1")
	published, err := client.WorkspacePublish(ctx, executorPublish(wire1, turnBundle, nil, commit1))
	if err != nil || published.GetRefusal() != nil || published.GetRejection() != nil || published.GetSeq() == 0 {
		t.Fatalf("publish = %v %v", published, err)
	}
	replay, err := client.WorkspacePublish(ctx, executorPublish(wire1, turnBundle, nil, commit1))
	if err != nil || replay.GetSeq() != published.GetSeq() {
		t.Fatalf("identical replay = %v %v, want seq %d", replay, err, published.GetSeq())
	}
	conflict, err := client.WorkspacePublish(ctx, executorPublish(wire1, turnBundle, nil, sha1Of("elsewhere")))
	if err != nil || conflict.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT ||
		len(conflict.GetRefusal().GetCurrent()) != 1 || conflict.GetRefusal().GetCurrent()[0].GetSha() != commit1 {
		t.Fatalf("conflict = %v %v", conflict, err)
	}
	if meta := answerMeta(t, pool, response1); meta["workspace_commit"] != commit1 {
		t.Fatalf("answer meta after publish = %v", meta)
	}

	// ---- thread_busy: another change holds the thread's row. ABORTED, then
	// the SAME publish applies once. ----
	secondBundle := []byte("cloud turn 1, after the pause")
	uploadExecutorSegment(t, client, wire1, secondBundle)
	commit2 := sha1Of("cloud commit 2")
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM elitea_runtime.workspace_handoffs WHERE project_id = 1 AND conversation_uuid = $1::uuid FOR UPDATE`,
		executorConversation); err != nil {
		t.Fatal(err)
	}
	old1 := commit1
	if response, err := client.WorkspacePublish(ctx, executorPublish(wire1, secondBundle, &old1, commit2)); status.Code(err) != codes.Aborted ||
		status.Convert(err).Message() != workspacegrpc.ThreadBusyMessage {
		t.Fatalf("publish under a held row = %v %v, want ABORTED thread_busy", response, err)
	}
	_ = holder.Rollback(ctx)
	retried, err := client.WorkspacePublish(ctx, executorPublish(wire1, secondBundle, &old1, commit2))
	if err != nil || retried.GetRefusal() != nil || retried.GetSeq() <= published.GetSeq() {
		t.Fatalf("retried publish = %v %v", retried, err)
	}
	if state().CloudSHA == nil || *state().CloudSHA != commit2 {
		t.Fatalf("cloud ref = %v", state().CloudSHA)
	}

	// ---- The turn ends at its projection (the lifecycle hook). ----
	withTenantTx(t, pool, func(tx pgx.Tx) {
		completePostgresCurrentApplicationTurn(t, tx, mustCurrentPGUUID(t, response1), "done")
	})
	if err := adapter.FinishExecution(ctx, "exec-cloud-1"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.FinishExecution(ctx, "exec-cloud-1"); err != nil {
		t.Fatalf("a second finish (the settlement hook) = %v", err)
	}
	if got := state(); got.State != workspacestore.StateCloudIdle || got.Epoch != 2 || got.RunningExecutionID != "" {
		t.Fatalf("after finish = %+v", got)
	}
	// The finished turn's claim can no longer read the env or publish.
	if response, err := client.WorkspacePublish(ctx, executorPublish(wire1, secondBundle, &old1, commit2)); err != nil ||
		response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_NOT_A_WORKSPACE_TURN {
		t.Fatalf("publish after finish = %v %v", response, err)
	}

	// ---- Regenerate the newest cloud turn: the admission marks a rewind
	// from the turn's last commit back to the commit it started from. ----
	time.Sleep(5 * time.Millisecond) // the regenerate is admitted after the finish
	withTenantTx(t, pool, func(tx pgx.Tx) {
		if err := resetCurrentAgentResponse(ctx, tx, sqlcgen.New(tx), "exec-cloud-2", agentexecutionapp.CurrentRegenerateTurn{
			ProjectID: 1, ActorUserID: owner, ConversationUUID: executorConversation, TargetParticipantID: 21,
			Kind: agentexecutionapp.CurrentRegenerationKind("application"), ApplicationID: 31, ApplicationVersionID: 41,
			QuestionID: question1, ResponseMessageID: response1, ExecutionGeneration: uuid.NewString(),
		}); err != nil {
			t.Fatal(err)
		}
	})
	meta := answerMeta(t, pool, response1)
	if meta[workspacehandoff.MetaRewindFrom] != commit2 || meta[workspacehandoff.MetaRewindTo] != handoffSHA ||
		meta["executed_by"] != nil || meta["workspace_commit"] != nil || meta[workspacehandoff.MetaWorkspaceBase] != nil {
		t.Fatalf("regenerated answer meta = %v", meta)
	}
	seedCloudTurnExecution(t, pool, "exec-cloud-2", response1)
	claim2 := workspaceruntime.TurnClaim{ExecutionID: "exec-cloud-2", Generation: 1, ProjectID: 1, ConversationUUID: executorConversation}
	field, err = contexts.ResolveWorkspaceContext(ctx, claim2)
	if err != nil || !field.Rewind || field.RewindTo == nil || *field.RewindTo != handoffSHA ||
		field.ExpectedOldCloud == nil || *field.ExpectedOldCloud != commit2 {
		t.Fatalf("regenerate claim = %+v %v", field, err)
	}
	if meta := answerMeta(t, pool, response1); meta[workspacehandoff.MetaWorkspaceBase] != handoffSHA || meta["executed_by"] != "cloud_executor" {
		t.Fatalf("answer meta at the regenerate's claim = %v", meta)
	}
	// Before the rewind lands, a repeated redemption still asks for it.
	if again, err := contexts.ResolveWorkspaceContext(ctx, claim2); err != nil || !again.Rewind {
		t.Fatalf("repeated regenerate claim = %+v %v", again, err)
	}
	// The worker's rewind: the normal compare-and-set, commit2 -> handoffSHA.
	wire2 := executorClaim("exec-cloud-2", 2)
	rewindBundle := []byte("rewound to the handoff snapshot")
	uploadExecutorSegment(t, client, wire2, rewindBundle)
	old2 := commit2
	if response, err := client.WorkspacePublish(ctx, executorPublish(wire2, rewindBundle, &old2, handoffSHA)); err != nil ||
		response.GetRefusal() != nil || response.GetRejection() != nil {
		t.Fatalf("rewind publish = %v %v", response, err)
	}
	// Applied once: the next redemption does not rewind again.
	if again, err := contexts.ResolveWorkspaceContext(ctx, claim2); err != nil || again.Rewind || again.RewindTo != nil ||
		again.ExpectedOldCloud == nil || *again.ExpectedOldCloud != handoffSHA {
		t.Fatalf("redemption after the rewind = %+v %v", again, err)
	}

	// ---- The reconciler ends a turn whose execution is terminal. ----
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp() WHERE execution_id = 'exec-cloud-2'`); err != nil {
		t.Fatal(err)
	}
	if ended, err := adapter.ReconcileFinished(ctx); err != nil || ended != 1 {
		t.Fatalf("reconcile = %d %v", ended, err)
	}
	if got := state(); got.State != workspacestore.StateCloudIdle || got.Epoch != 2 {
		t.Fatalf("after reconcile = %+v", got)
	}

	// ---- Take back and release, as the desktop does. ----
	reclaimed, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 2, DeviceID: "device-1"})
	if err != nil || reclaimed.State != workspacestore.StateReclaiming || reclaimed.Epoch != 3 {
		t.Fatalf("reclaim = %+v %v", reclaimed, err)
	}
	applied := handoffSHA
	released, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: 3,
		Outcome: "applied", AppliedSHA: &applied})
	if err != nil || released.State != workspacestore.StateLocal || released.Epoch != 4 {
		t.Fatalf("release = %+v %v", released, err)
	}
	// A claim of the released thread is refused, not run without files.
	if _, err := contexts.ResolveWorkspaceContext(ctx, claim2); !errors.Is(err, workspaceruntime.ErrNotCloudTurn) {
		t.Fatalf("claim after release = %v", err)
	}

	// ---- The audit: the executor's publishes and the transitions, never the
	// env value. ----
	storeActions := strings.Join(storeAudit.actions(), "\n")
	if strings.Count(storeActions, "store.publish, writer cloud_executor, epoch 2") != 3 ||
		!strings.Contains(storeActions, "store.publish_refused, writer cloud_executor, epoch 2") ||
		!strings.Contains(storeActions, "reason ref_conflict") || !strings.Contains(storeActions, "reason thread_busy") {
		t.Fatalf("store audit (two turn publishes and the rewind; the replay is not a new row):\n%s", storeActions)
	}
	actions := strings.Join(h.recorder.actions(), "\n")
	for _, want := range []string{"handoff.begin", "handoff.complete", "handoff.reclaim", "handoff.release"} {
		if !strings.Contains(actions, want) {
			t.Fatalf("audit lacks %q:\n%s", want, actions)
		}
	}
	if strings.Contains(actions, harvestedValue) {
		t.Fatalf("the audit carries the env value")
	}
	if !secretPresent(t, reader, envSecretNameOf(t, pool)) {
		t.Fatalf("the env secret is gone before the release grace")
	}
}

func withTenantTx(t *testing.T, pool *pgxpool.Pool, body func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	body(tx)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func envSecretNameOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT env_secret_name FROM elitea_runtime.workspace_handoffs WHERE conversation_uuid = $1::uuid`,
		executorConversation).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// A take back during a cloud turn (ADR-0034 decision 7, scope A): Reclaim
// sets reclaim_pending, and the lease poll's signal (WrapUpRequested, which
// the control server sends as workspace_wrap_up) says so for that execution
// only. The graceful path: the worker wraps up and the turn's finish moves
// the thread to RECLAIMING. The fallback, with no worker answering: after
// the 60 s grace the sweeper forces RECLAIMING and cancels the turn, and the
// signal is gone.
func TestPostgresTakeBackDuringACloudTurnSignalsTheWorkerAndFallsBack(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	ctx := context.Background()
	const owner int64 = 11
	ensureWorkspaceTestUser(t, pool, owner)
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET source = 'local_work' WHERE uuid = $1::uuid`, executorConversation); err != nil {
		t.Fatal(err)
	}
	h := newHandoffHarness(t, pool)
	thread := workspacestore.Thread{ProjectID: 1, ConversationUUID: executorConversation}
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := workspaceexecutor.New(h.service, h.objects, executorThreads{repo: repo}, workspaceexecutor.Resources{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	began, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := h.publishSnapshot(t, thread, owner, 1, "take back")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: began.Handoff.HandoffID,
		ActorUserID: owner, Epoch: 1, HandoffRef: workspacestore.RefHandoff1, SHA: snapshot}); err != nil {
		t.Fatal(err)
	}
	const response = "40000000-0000-4000-8000-0000000000f1"
	withTenantTx(t, pool, func(tx pgx.Tx) {
		insertPostgresCurrentApplicationTurn(t, sqlcgen.New(tx), mustCurrentPGUUID(t, executorConversation),
			"20000000-0000-4000-8000-0000000000f1", "30000000-0000-4000-8000-0000000000f1", response, "long task", "exec-take-back")
	})
	seedCloudTurnExecution(t, pool, "exec-take-back", response)
	if _, ok, err := adapter.ResolveCloudTurn(ctx, workspaceruntime.TurnClaim{ExecutionID: "exec-take-back", Generation: 1,
		ProjectID: 1, ConversationUUID: executorConversation}); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if signalled, err := adapter.WrapUpRequested(ctx, "exec-take-back"); err != nil || signalled {
		t.Fatalf("signal before the take back = %v %v", signalled, err)
	}
	pending, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 2, DeviceID: "device-1"})
	if err != nil || pending.State != workspacestore.StateCloudRunning || !pending.ReclaimPending || pending.Epoch != 2 {
		t.Fatalf("reclaim during a turn = %+v %v", pending, err)
	}
	if signalled, err := adapter.WrapUpRequested(ctx, "exec-take-back"); err != nil || !signalled {
		t.Fatalf("signal after the take back = %v %v", signalled, err)
	}
	if signalled, _ := adapter.WrapUpRequested(ctx, "exec-other"); signalled {
		t.Fatal("another execution was signalled")
	}

	// Fallback: no worker wrapped up. Past the grace the sweeper forces the
	// take back and cancels the turn.
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * workspacehandoff.ReclaimGrace) })
	if _, err := h.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := h.service.State(ctx, thread)
	if err != nil || view.Handoff.State != workspacestore.StateReclaiming || view.Handoff.Epoch != 3 {
		t.Fatalf("after the grace = %+v %v", view.Handoff, err)
	}
	if len(h.canceller.cancelled) != 1 || h.canceller.cancelled[0] != response {
		t.Fatalf("cancelled = %v", h.canceller.cancelled)
	}
	if signalled, _ := adapter.WrapUpRequested(ctx, "exec-take-back"); signalled {
		t.Fatal("the signal outlived the forced take back")
	}
	// The worker's late finish is stale and changes nothing.
	if err := adapter.FinishExecution(ctx, "exec-take-back"); err != nil {
		t.Fatal(err)
	}
	if view, _ := h.service.State(ctx, thread); view.Handoff.State != workspacestore.StateReclaiming || view.Handoff.Epoch != 3 {
		t.Fatalf("a late finish moved the thread: %+v", view.Handoff)
	}
}
