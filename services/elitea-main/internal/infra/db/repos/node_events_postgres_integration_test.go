package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/executions"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// TestPostgresNodeEventThenTerminalProjection crosses a real PostgreSQL
// transaction boundary when ELITEA_TEST_DATABASE_URL is set. It proves the
// existing replay table can order progress followed by terminal output without
// a schema migration; it is not full gRPC/SSE end-to-end evidence.
func TestPostgresNodeEventThenTerminalProjection(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	dispatchPolicy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "node-event").Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-node-event", "node-event"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	terminal := postgresInlineIndexOutputFrame(t, expected, fence, outputapp.IndexIngestSummary{
		Status:  outputapp.IndexIngestStatusOK,
		Message: "No new documents to index.",
	})

	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	nodeRepository, err := NewNodeEventsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	nodeService, err := outputapp.NewNodeEventService(claimService, nodeRepository)
	if err != nil {
		t.Fatal(err)
	}
	wireEvent := []byte("deterministic-node-event-protobuf")
	browserEvent := []byte(`{"type":"agent_index_data_status","stream_id":"index-stream","message_id":null,"question_id":null,"content":{"state":"in_progress"},"thinking":null,"response_metadata":{},"references":[],"sio_event":"test_toolkit_tool","created_at":"2026-07-22T12:00:00Z","parent_message_id":null,"agent_name":null,"execution_generation":null}`)
	progress := outputapp.NodeEventFrame{
		StreamID:            terminal.StreamID,
		TenantID:            terminal.TenantID,
		ResourceProjectID:   terminal.ResourceProjectID,
		ProjectionProjectID: terminal.ProjectionProjectID,
		WorkloadSessionID:   terminal.WorkloadSessionID,
		ProducerID:          terminal.ProducerID,
		EventID:             terminal.Fence.CommandID + ":1",
		LogicalOutputID:     outputapp.NodeEventLogicalOutputID(terminal.Fence.ExecutionID, 1),
		Sequence:            1,
		OccurredAt:          time.Now().UTC(),
		Fence:               terminal.Fence,
		PayloadDigest:       runtimedomain.SHA256(wireEvent),
		EncodedEvent:        wireEvent,
		BrowserData:         browserEvent,
	}
	progressOutcome, err := nodeService.IngestNodeEvent(context.Background(), progress)
	if err != nil {
		t.Fatal(err)
	}
	if !progressOutcome.Inserted || progressOutcome.CommittedSequence != 1 {
		t.Fatalf("unexpected durable progress outcome: %+v", progressOutcome)
	}

	terminal.Sequence = 2
	terminal.EventID = terminal.Fence.CommandID + ":2"
	terminal.Settlement.TerminalSequence = 2
	terminal.Settlement.TerminalEventID = terminal.EventID
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId:              terminal.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: terminal.Settlement.TerminalLogicalOutputID,
		TerminalEventId:         terminal.Settlement.TerminalEventID,
		TerminalSequence:        terminal.Settlement.TerminalSequence,
		TerminalPayloadDigest:   postgresDigestV1(terminal.Settlement.TerminalPayloadDigest),
		PrepareIdempotencyKey:   terminal.Settlement.IdempotencyKey,
	}
	terminal.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	terminal.Settlement.ProposalDigest = runtimedomain.SHA256(terminal.EncodedSettlement)
	terminalOutcome, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(context.Background(), terminal)
	if err != nil {
		t.Fatal(err)
	}
	if !terminalOutcome.Inserted || terminalOutcome.CommittedSequence != 2 || terminalOutcome.Cursor <= progressOutcome.Cursor {
		t.Fatalf("terminal did not follow durable progress: progress=%+v terminal=%+v", progressOutcome, terminalOutcome)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
	  AND event_type IN ('execution.node_event', 'index.ingest.completed')`, terminal.Fence.ExecutionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("unexpected replay event count: %d", count)
	}
}

func TestPostgresReplayRetentionBoundsProgressAndResetsExpiredCursor(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	progress, terminal := preparePostgresNodeEventConcurrencyFixture(t, pool, "retention")
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	nodeRepository, err := newNodeEventsRepositoryWithPolicy(store, replayRetentionPolicy{
		maxProgressEvents: 10,
		maxProgressBytes:  180,
		maxProgressAge:    time.Hour,
		janitorBatchSize:  1,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var lastProgress outputapp.ProjectionOutcome
	for sequence := uint64(1); sequence <= 4; sequence++ {
		frame := progress
		frame.Sequence = sequence
		frame.EventID = frame.Fence.CommandID + ":" + fmt.Sprint(sequence)
		frame.LogicalOutputID = outputapp.NodeEventLogicalOutputID(frame.Fence.ExecutionID, sequence)
		frame.EncodedEvent = []byte(fmt.Sprintf("retained-wire-event-%d", sequence))
		frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedEvent)
		frame.BrowserData = []byte(fmt.Sprintf(
			`{"type":"agent_thinking_step","content":null,"response_metadata":{"message":"progress-%d"}}`,
			sequence,
		))
		lastProgress, err = nodeRepository.ProjectNodeEvent(ctx, frame)
		if err != nil || !lastProgress.Inserted || lastProgress.CommittedSequence != sequence {
			t.Fatalf("append progress %d: outcome=%+v err=%v", sequence, lastProgress, err)
		}
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID)
	var lastSequence, retainedEvents, retainedBytes, prunedThrough int64
	if err := pool.QueryRow(ctx, `
SELECT last_node_sequence,
       retained_progress_events,
       retained_progress_bytes,
       pruned_through_cursor
FROM elitea_runtime.execution_replay_state
WHERE execution_id = $1 AND generation = 1`,
		progress.Fence.ExecutionID,
	).Scan(&lastSequence, &retainedEvents, &retainedBytes, &prunedThrough); err != nil {
		t.Fatal(err)
	}
	if lastSequence != 4 || retainedEvents != 2 || retainedBytes <= 0 || retainedBytes > 180 || prunedThrough <= 0 {
		t.Fatalf(
			"unexpected bounded replay state: sequence=%d events=%d bytes=%d pruned=%d",
			lastSequence,
			retainedEvents,
			retainedBytes,
			prunedThrough,
		)
	}

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_replay_events
SET created_at = clock_timestamp() - interval '2 hours'
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID); err != nil {
		t.Fatal(err)
	}
	deleted, err := nodeRepository.PruneExpiredReplayProgress(ctx)
	if err != nil || deleted != 2 {
		t.Fatalf("expire replay progress: deleted=%d err=%v", deleted, err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID)

	retry := progress
	retry.Sequence = 4
	retry.EventID = retry.Fence.CommandID + ":4"
	retry.LogicalOutputID = outputapp.NodeEventLogicalOutputID(retry.Fence.ExecutionID, 4)
	retry.EncodedEvent = []byte("retained-wire-event-4")
	retry.PayloadDigest = runtimedomain.SHA256(retry.EncodedEvent)
	retry.BrowserData = []byte(`{"type":"agent_thinking_step","content":null,"response_metadata":{"message":"progress-4"}}`)
	retryOutcome, err := nodeRepository.ProjectNodeEvent(ctx, retry)
	if err != nil || retryOutcome.Inserted || retryOutcome.Cursor != lastProgress.Cursor || retryOutcome.CommittedSequence != 4 {
		t.Fatalf("retry pruned latest receipt: outcome=%+v err=%v", retryOutcome, err)
	}

	terminal.Sequence = 5
	terminal.EventID = terminal.Fence.CommandID + ":5"
	terminal.Settlement.TerminalSequence = 5
	terminal.Settlement.TerminalEventID = terminal.EventID
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId:              terminal.Settlement.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: terminal.Settlement.TerminalLogicalOutputID,
		TerminalEventId:         terminal.Settlement.TerminalEventID,
		TerminalSequence:        terminal.Settlement.TerminalSequence,
		TerminalPayloadDigest:   postgresDigestV1(terminal.Settlement.TerminalPayloadDigest),
		PrepareIdempotencyKey:   terminal.Settlement.IdempotencyKey,
	}
	terminal.EncodedSettlement, err = proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	terminal.Settlement.ProposalDigest = runtimedomain.SHA256(terminal.EncodedSettlement)
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    "index-limits-v1",
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalOutcome, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(ctx, terminal)
	if err != nil || !terminalOutcome.Inserted || terminalOutcome.CommittedSequence != 5 {
		t.Fatalf("terminal after pruned progress: outcome=%+v err=%v", terminalOutcome, err)
	}
	if deleted, err := nodeRepository.PruneExpiredReplayProgress(ctx); err != nil || deleted != 0 {
		t.Fatalf("terminal-only janitor pass: deleted=%d err=%v", deleted, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'index.ingest.completed'`, progress.Fence.ExecutionID)

	replay, err := NewReplayEventsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	events, err := replay.Replay(ctx, terminal.ProjectionProjectID, terminal.Fence.ExecutionID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].Type != replayEventReset ||
		events[0].Cursor != lastProgress.Cursor ||
		events[1].Type != "index.ingest.completed" ||
		events[1].Cursor != terminalOutcome.Cursor {
		t.Fatalf("unexpected reset plus terminal replay: %+v", events)
	}
	if _, err := replay.Replay(ctx, "2", terminal.Fence.ExecutionID, 0, 10); !errors.Is(err, executionsapi.ErrInvalidEventStream) {
		t.Fatalf("cross-project replay was not rejected: %v", err)
	}
}

func TestPostgresReplayRetentionCountCap(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	progress, _ := preparePostgresNodeEventConcurrencyFixture(t, pool, "retention-count")
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	nodeRepository, err := newNodeEventsRepositoryWithPolicy(store, replayRetentionPolicy{
		maxProgressEvents: 2,
		maxProgressBytes:  1024,
		maxProgressAge:    time.Hour,
		janitorBatchSize:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for sequence := uint64(1); sequence <= 3; sequence++ {
		frame := progress
		frame.Sequence = sequence
		frame.EventID = frame.Fence.CommandID + ":" + fmt.Sprint(sequence)
		frame.LogicalOutputID = outputapp.NodeEventLogicalOutputID(frame.Fence.ExecutionID, sequence)
		frame.EncodedEvent = []byte(fmt.Sprintf("count-wire-event-%d", sequence))
		frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedEvent)
		frame.BrowserData = []byte(fmt.Sprintf(`{"type":"agent_thinking_step","content":"%d"}`, sequence))
		if outcome, err := nodeRepository.ProjectNodeEvent(ctx, frame); err != nil || !outcome.Inserted {
			t.Fatalf("append count-capped progress %d: outcome=%+v err=%v", sequence, outcome, err)
		}
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID)
	var sequence, retained, pruned int64
	if err := pool.QueryRow(ctx, `
SELECT last_node_sequence, retained_progress_events, pruned_through_cursor
FROM elitea_runtime.execution_replay_state
WHERE execution_id = $1 AND generation = 1`,
		progress.Fence.ExecutionID,
	).Scan(&sequence, &retained, &pruned); err != nil {
		t.Fatal(err)
	}
	if sequence != 3 || retained != 2 || pruned <= 0 {
		t.Fatalf("unexpected count-capped state: sequence=%d retained=%d pruned=%d", sequence, retained, pruned)
	}
}

func TestPostgresReplayRetentionJanitorBoundsAgePass(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	progress, _ := preparePostgresNodeEventConcurrencyFixture(t, pool, "retention-age-pass")
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	nodeRepository, err := newNodeEventsRepositoryWithPolicy(store, replayRetentionPolicy{
		maxProgressEvents: 100,
		maxProgressBytes:  64 * 1024,
		maxProgressAge:    time.Hour,
		janitorBatchSize:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const progressEvents = replayJanitorMaxEventsPerExecution + 2
	for sequence := uint64(1); sequence <= uint64(progressEvents); sequence++ {
		frame := progress
		frame.Sequence = sequence
		frame.EventID = frame.Fence.CommandID + ":" + fmt.Sprint(sequence)
		frame.LogicalOutputID = outputapp.NodeEventLogicalOutputID(frame.Fence.ExecutionID, sequence)
		frame.EncodedEvent = []byte(fmt.Sprintf("age-wire-event-%d", sequence))
		frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedEvent)
		frame.BrowserData = []byte(fmt.Sprintf(`{"type":"agent_thinking_step","content":"%d"}`, sequence))
		if outcome, err := nodeRepository.ProjectNodeEvent(ctx, frame); err != nil || !outcome.Inserted {
			t.Fatalf("append age-capped progress %d: outcome=%+v err=%v", sequence, outcome, err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_replay_events
SET created_at = clock_timestamp() - interval '2 hours'
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID); err != nil {
		t.Fatal(err)
	}
	firstDeleted, err := nodeRepository.PruneExpiredReplayProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstDeleted != replayJanitorMaxEventsPerExecution {
		t.Fatalf("first age pass deleted=%d", firstDeleted)
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID)
	secondDeleted, err := nodeRepository.PruneExpiredReplayProgress(ctx)
	if err != nil || secondDeleted != 2 {
		t.Fatalf("second age pass deleted=%d err=%v", secondDeleted, err)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1
  AND generation = 1
  AND event_type = 'execution.node_event'`, progress.Fence.ExecutionID)
}

// TestPostgresNodeEventSequenceLinearization proves that a sequence N+1
// writer waiting behind sequence N evaluates replay state after N commits. A
// one-statement lock-and-read query uses its pre-wait snapshot and rejects N+1.
func TestPostgresNodeEventSequenceLinearization(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	first, _ := preparePostgresNodeEventConcurrencyFixture(t, pool, "sequence")
	second := first
	second.Sequence = 2
	second.EventID = second.Fence.CommandID + ":2"
	second.LogicalOutputID = outputapp.NodeEventLogicalOutputID(second.Fence.ExecutionID, 2)
	second.EncodedEvent = []byte("deterministic-node-event-protobuf-2")
	second.PayloadDigest = runtimedomain.SHA256(second.EncodedEvent)
	second.BrowserData = []byte(`{"type":"agent_index_data_status","content":{"state":"second"}}`)
	if err := second.Validate(); err != nil {
		t.Fatalf("build second node event: %v", err)
	}

	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondBackendPID := make(chan int32, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstRepository := newGatedPostgresNodeEventsRepository(t, pool, func() {
		close(firstLocked)
		select {
		case <-releaseFirst:
		case <-ctx.Done():
		}
	}, nil)
	secondRepository := newGatedPostgresNodeEventsRepository(t, pool, nil, secondBackendPID)

	firstResult := make(chan postgresNodeEventProjectionResult, 1)
	secondResult := make(chan postgresNodeEventProjectionResult, 1)
	go func() {
		outcome, err := firstRepository.ProjectNodeEvent(ctx, first)
		firstResult <- postgresNodeEventProjectionResult{outcome: outcome, err: err}
	}()
	waitForPostgresLinearizationSignal(t, ctx, firstLocked, "first node-event authority lock")
	go func() {
		outcome, err := secondRepository.ProjectNodeEvent(ctx, second)
		secondResult <- postgresNodeEventProjectionResult{outcome: outcome, err: err}
	}()
	waitForPostgresBackendLock(t, ctx, pool, waitForPostgresBackendPID(t, ctx, secondBackendPID, "second node event"), "second node event")
	close(releaseFirst)

	firstProjection := waitForPostgresProjectionResult(t, ctx, firstResult, "first node event")
	secondProjection := waitForPostgresProjectionResult(t, ctx, secondResult, "second node event")
	if firstProjection.err != nil || !firstProjection.outcome.Inserted || firstProjection.outcome.CommittedSequence != 1 {
		t.Fatalf("first node event outcome=%+v err=%v", firstProjection.outcome, firstProjection.err)
	}
	if secondProjection.err != nil || !secondProjection.outcome.Inserted || secondProjection.outcome.CommittedSequence != 2 {
		t.Fatalf("second node event did not observe committed predecessor: outcome=%+v err=%v", secondProjection.outcome, secondProjection.err)
	}
	assertPostgresCount(t, ctx, pool, 2, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = 1 AND event_type = 'execution.node_event'`, first.Fence.ExecutionID)
}

// TestPostgresTerminalExcludesWaitingNodeEvent proves that progress waiting
// behind a terminal writer sees the committed terminal row and cannot append
// post-terminal replay data from a stale statement snapshot.
func TestPostgresTerminalExcludesWaitingNodeEvent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	progress, terminal := preparePostgresNodeEventConcurrencyFixture(t, pool, "terminal")
	record, _, err := indexOutputRecord(terminal)
	if err != nil {
		t.Fatal(err)
	}

	terminalLocked := make(chan struct{})
	releaseTerminal := make(chan struct{})
	progressBackendPID := make(chan int32, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	terminalStore := &postgresAuthorityGateStore{
		sharedStore: base,
		afterLock: func() {
			close(terminalLocked)
			select {
			case <-releaseTerminal:
			case <-ctx.Done():
			}
		},
	}
	progressRepository := newGatedPostgresNodeEventsRepository(t, pool, nil, progressBackendPID)

	terminalResults := make(chan postgresTerminalInsertResult, 1)
	progressResults := make(chan error, 1)
	go func() {
		var outcome outputInsertResult
		err := terminalStore.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
			var insertErr error
			outcome, insertErr = insertOutputInbox(ctx, tx, record)
			return insertErr
		})
		terminalResults <- postgresTerminalInsertResult{outcome: outcome, err: err}
	}()
	waitForPostgresLinearizationSignal(t, ctx, terminalLocked, "terminal authority lock")
	go func() {
		_, err := progressRepository.ProjectNodeEvent(ctx, progress)
		progressResults <- err
	}()
	waitForPostgresBackendLock(t, ctx, pool, waitForPostgresBackendPID(t, ctx, progressBackendPID, "post-terminal node event"), "post-terminal node event")
	close(releaseTerminal)

	terminalProjection := waitForPostgresTerminalResult(t, ctx, terminalResults)
	if terminalProjection.err != nil || !terminalProjection.outcome.Inserted {
		t.Fatalf("terminal output outcome=%+v err=%v", terminalProjection.outcome, terminalProjection.err)
	}
	select {
	case progressErr := <-progressResults:
		if !errors.Is(progressErr, outputapp.ErrNodeEventOutputConflict) {
			t.Fatalf("waiting post-terminal node event error=%v", progressErr)
		}
	case <-ctx.Done():
		t.Fatalf("wait for post-terminal node event: %v", ctx.Err())
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.output_inbox
WHERE execution_id = $1 AND generation = 1`, terminal.Fence.ExecutionID)
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = $1 AND generation = 1 AND event_type = 'execution.node_event'`, terminal.Fence.ExecutionID)
}

func preparePostgresNodeEventConcurrencyFixture(t *testing.T, pool *pgxpool.Pool, suffix string) (outputapp.NodeEventFrame, outputapp.IndexIngestFrame) {
	t.Helper()
	dispatchPolicy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "node-event-"+suffix).Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-node-event-"+suffix, "node-event-"+suffix),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit concurrent index execution: outcome=%+v err=%v", admitted, err)
	}
	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	terminal := postgresInlineIndexOutputFrame(t, expected, fence, outputapp.IndexIngestSummary{
		Status:  outputapp.IndexIngestStatusOK,
		Message: "No new documents to index.",
	})
	wireEvent := []byte("deterministic-node-event-protobuf-1")
	progress := outputapp.NodeEventFrame{
		StreamID:            terminal.StreamID,
		TenantID:            terminal.TenantID,
		ResourceProjectID:   terminal.ResourceProjectID,
		ProjectionProjectID: terminal.ProjectionProjectID,
		WorkloadSessionID:   terminal.WorkloadSessionID,
		ProducerID:          terminal.ProducerID,
		EventID:             terminal.Fence.CommandID + ":1",
		LogicalOutputID:     outputapp.NodeEventLogicalOutputID(terminal.Fence.ExecutionID, 1),
		Sequence:            1,
		OccurredAt:          time.Now().UTC(),
		Fence:               terminal.Fence,
		PayloadDigest:       runtimedomain.SHA256(wireEvent),
		EncodedEvent:        wireEvent,
		BrowserData:         []byte(`{"type":"agent_index_data_status","content":{"state":"first"}}`),
	}
	return progress, terminal
}

type postgresAuthorityGateStore struct {
	sharedStore
	afterLock  func()
	backendPID chan<- int32
}

type postgresNodeEventProjectionResult struct {
	outcome outputapp.ProjectionOutcome
	err     error
}

type postgresTerminalInsertResult struct {
	outcome outputInsertResult
	err     error
}

func (s *postgresAuthorityGateStore) WithinTx(ctx context.Context, options pgx.TxOptions, fn func(sqlExecutor) error) error {
	return s.sharedStore.WithinTx(ctx, options, func(tx sqlExecutor) error {
		if s.backendPID != nil {
			var pid int32
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			select {
			case s.backendPID <- pid:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return fn(&postgresAuthorityGateExecutor{sqlExecutor: tx, afterLock: s.afterLock})
	})
}

type postgresAuthorityGateExecutor struct {
	sqlExecutor
	afterLock func()
}

func (e *postgresAuthorityGateExecutor) QueryRow(ctx context.Context, query string, args ...any) sqlRow {
	row := e.sqlExecutor.QueryRow(ctx, query, args...)
	if !strings.Contains(query, "SELECT c.claim_id") || !strings.Contains(query, "FOR UPDATE OF j, o, c") {
		return row
	}
	return &postgresAuthorityGateRow{sqlRow: row, afterLock: e.afterLock}
}

type postgresAuthorityGateRow struct {
	sqlRow
	afterLock func()
}

func (r *postgresAuthorityGateRow) Scan(dest ...any) error {
	err := r.sqlRow.Scan(dest...)
	if err == nil && r.afterLock != nil {
		r.afterLock()
	}
	return err
}

func newGatedPostgresNodeEventsRepository(t *testing.T, pool *pgxpool.Pool, afterLock func(), backendPID chan<- int32) *NodeEventsRepository {
	t.Helper()
	base, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := newNodeEventsRepository(&postgresAuthorityGateStore{
		sharedStore: base,
		afterLock:   afterLock,
		backendPID:  backendPID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func waitForPostgresBackendPID(t *testing.T, ctx context.Context, pids <-chan int32, name string) int32 {
	t.Helper()
	select {
	case pid := <-pids:
		return pid
	case <-ctx.Done():
		t.Fatalf("wait for %s backend: %v", name, ctx.Err())
		return 0
	}
}

func waitForPostgresBackendLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int32, name string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
SELECT COALESCE(wait_event_type = 'Lock', FALSE)
FROM pg_stat_activity
WHERE pid = $1`, pid).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("inspect %s lock wait: %v", name, err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("wait for %s PostgreSQL lock: %v", name, ctx.Err())
		}
	}
}

func waitForPostgresLinearizationSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
	}
}

func waitForPostgresProjectionResult(t *testing.T, ctx context.Context, results <-chan postgresNodeEventProjectionResult, name string) postgresNodeEventProjectionResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
		return postgresNodeEventProjectionResult{}
	}
}

func waitForPostgresTerminalResult(t *testing.T, ctx context.Context, results <-chan postgresTerminalInsertResult) postgresTerminalInsertResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-ctx.Done():
		t.Fatalf("wait for terminal output: %v", ctx.Err())
		return postgresTerminalInsertResult{}
	}
}
