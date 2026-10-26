package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Wave 2a-2 wiring of the per-interrupt decision ledger
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md v1.1): card
// ingestion in the frame-accepting transaction, the Main-emitted
// agent_hitl_resolved frame, and park/wake settlement.

const (
	interruptPendingEventType  = "agent_interrupt_pending"
	interruptResolvedEventType = "agent_hitl_resolved"
	// interruptResolvedEventIDPrefix keys the Main-emitted frame by
	// (response, card, revision), so a retried transaction cannot duplicate it.
	interruptResolvedEventIDPrefix = "interrupt-resolved:"
)

// legacyPauseMembers may not ride on a V2 card frame (contract v1.1 §4.1):
// one frame is one pause authority.
var legacyPauseMembers = []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "parallel_pause_v1", "node_recovery_required_v1"}

// projectExecutionInterrupt raises the card of an accepted
// agent_interrupt_pending frame into the ledger. It runs inside the NodeEvent
// transaction after the frame's fence and sequence were accepted, so the
// frame and its card commit together; any refusal refuses the frame. Other
// frame types are ignored.
func projectExecutionInterrupt(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame, claimID string) error {
	var event struct {
		Type                string                     `json:"type"`
		StreamID            string                     `json:"stream_id"`
		MessageID           string                     `json:"message_id"`
		ExecutionGeneration string                     `json:"execution_generation"`
		SIOEvent            string                     `json:"sio_event"`
		ResponseMetadata    map[string]json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	if event.Type != interruptPendingEventType {
		return nil
	}
	for _, key := range legacyPauseMembers {
		if _, ok := event.ResponseMetadata[key]; ok {
			return outputapp.ErrInvalidNodeEventOutput
		}
	}
	raw, ok := event.ResponseMetadata["interrupt_card_v1"]
	if !ok || claimID == "" {
		return outputapp.ErrInvalidNodeEventOutput
	}
	card, _, err := domain.ParseCard(raw)
	if err != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	// Coordinator cards stay on the root path until root pauses move (§10).
	frontier, err := card.MemberFrontier()
	if err != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	projectDatabaseID, valid := currentAgentDatabaseID(projectID)
	if !valid {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID ||
		event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return outputapp.ErrInvalidNodeEventOutput
	}
	_, err = raiseExecutionInterrupt(ctx, tx, domain.RaiseInput{
		ProjectID: projectID, RootResponseID: binding.messageID, ExecutionID: frame.Fence.ExecutionID,
		Generation: int64(frame.Fence.Generation), SourceEventID: frame.EventID, SourceClaimID: claimID,
		Frontier: frontier, Card: raw,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrRaiseConflict):
		return outputapp.ErrNodeEventOutputConflict
	case errors.Is(err, domain.ErrInvalidCard), errors.Is(err, domain.ErrInvalidRaise),
		errors.Is(err, domain.ErrInvalidFrontier), errors.Is(err, domain.ErrCapReached):
		return outputapp.ErrInvalidNodeEventOutput
	default:
		return err
	}
}

// interruptResolvedFrame is the browser NodeEvent shape (the thirteen fields
// in browser contract order, as the Worker encodes them).
type interruptResolvedFrame struct {
	Type                string          `json:"type"`
	StreamID            string          `json:"stream_id"`
	MessageID           string          `json:"message_id"`
	QuestionID          *string         `json:"question_id"`
	Content             json.RawMessage `json:"content"`
	Thinking            *string         `json:"thinking"`
	ResponseMetadata    json.RawMessage `json:"response_metadata"`
	References          json.RawMessage `json:"references"`
	SIOEvent            string          `json:"sio_event"`
	CreatedAt           *string         `json:"created_at"`
	ParentMessageID     *string         `json:"parent_message_id"`
	AgentName           *string         `json:"agent_name"`
	ExecutionGeneration string          `json:"execution_generation"`
}

// emitInterruptResolved appends one durable agent_hitl_resolved frame per
// resolved card to the replay log of the response's latest execution, on the
// caller's transaction (contract §9). The frame carries the action of a
// decision and never its value. A response with no agent execution in this
// project (impossible for a raised card) is a ledger fault.
//
// The returned notice names the highest replay cursor this call inserted; the
// caller hands it to ExecutionInterruptSignals after its transaction commits,
// so open tabs read the frames without waiting for the replay poll. A frame
// that already existed (a replayed write) produces no notice.
func emitInterruptResolved(ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID string, resolved []domain.Resolved) (resolvedNotice, error) {
	if len(resolved) == 0 {
		return resolvedNotice{}, nil
	}
	notice := resolvedNotice{projectID: projectID}
	var executionID, streamID, executionGeneration, sioEvent string
	var generation int64
	// The conversation's readers ride on the same statement (no extra round
	// trip): the live project-plane copy goes to them only.
	err := tx.QueryRow(ctx, `
SELECT binding.execution_id, binding.generation, binding.client_stream_id, binding.client_execution_generation, binding.sio_event,
       `+conversationReadersSQL("binding.client_stream_id")+`
FROM elitea_runtime.agent_execution_jobs binding
JOIN elitea_runtime.execution_jobs job USING (execution_id, generation)
WHERE binding.client_message_id = $1 AND job.projection_project_id = $2 AND job.resource_project_id = $2
ORDER BY job.admitted_at DESC, binding.generation DESC, binding.execution_id DESC
LIMIT 1`, rootResponseID, projectID).Scan(&executionID, &generation, &streamID, &executionGeneration, &sioEvent, &notice.audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvedNotice{}, domain.ErrLedgerFault
	}
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("execution interrupt resolved frame: %w", err)
	}
	for _, card := range resolved {
		metadata, err := card.ResolvedMetadataJSON()
		if err != nil {
			return resolvedNotice{}, err
		}
		frame, err := json.Marshal(interruptResolvedFrame{
			Type: interruptResolvedEventType, StreamID: streamID, MessageID: rootResponseID,
			Content: json.RawMessage("null"), References: json.RawMessage("[]"),
			ResponseMetadata: json.RawMessage(`{"interrupt_resolved_v1":` + string(metadata) + `}`),
			SIOEvent:         sioEvent, ExecutionGeneration: executionGeneration,
		})
		if err != nil {
			return resolvedNotice{}, fmt.Errorf("execution interrupt resolved frame: %w", err)
		}
		eventID := interruptResolvedEventIDPrefix + rootResponseID + ":" + card.InterruptKey + ":" + strconv.FormatInt(card.Revision, 10)
		digest := runtimedomain.SHA256(frame)
		var stored []byte
		var cursor int64
		var inserted bool
		err = tx.QueryRow(ctx, `
WITH inserted AS (
  INSERT INTO elitea_runtime.execution_replay_events (
      event_id, execution_id, generation, projection_project_id, event_type, event_bytes, event_digest)
  VALUES ($1, $2, $3, $4, $5, $6, $7)
  ON CONFLICT (event_id) DO NOTHING
  RETURNING event_bytes, cursor
)
SELECT event_bytes, cursor, TRUE FROM inserted
UNION ALL
SELECT event_bytes, cursor, FALSE FROM elitea_runtime.execution_replay_events WHERE event_id = $1 AND NOT EXISTS (SELECT 1 FROM inserted)`,
			eventID, executionID, generation, projectID, replayEventNodeEvent, frame, digest[:]).Scan(&stored, &cursor, &inserted)
		if err != nil {
			return resolvedNotice{}, fmt.Errorf("execution interrupt resolved frame: %w", err)
		}
		if !bytes.Equal(stored, frame) {
			return resolvedNotice{}, domain.ErrLedgerFault
		}
		if inserted && cursor > notice.cursor {
			notice.executionID, notice.cursor = executionID, cursor
		}
		if inserted {
			event, err := liveEventData(metadata, streamID, rootResponseID)
			if err != nil {
				return resolvedNotice{}, err
			}
			notice.live = append(notice.live, liveEvent{eventType: interruptResolvedEventType, data: event})
		}
	}
	notice.conversationID = streamID
	return notice, nil
}

// LiveFanoutWakeAdmittedEventType tells open tabs that a wake job was
// admitted for a parked response, so they attach to its execution stream.
const LiveFanoutWakeAdmittedEventType = "agent_fanout_wake_admitted"

// maxLiveAudience bounds the readers one live frame names. A conversation
// with more readers gets no live copy (the durable replay copy remains).
const maxLiveAudience = 256

// conversationReadersSQL selects, as a bigint[] in id order, the users who
// may read the conversation whose uuid text is in streamExpression: its
// author and its user participants. At most maxLiveAudience+1 ids, so an
// oversized audience is detectable without an unbounded read. A stream id
// that is not a conversation uuid yields an empty array.
func conversationReadersSQL(streamExpression string) string {
	return `COALESCE((
  SELECT array_agg(readers.user_id ORDER BY readers.user_id) FROM (
    SELECT DISTINCT reader.user_id FROM chat_conversations conversation
    CROSS JOIN LATERAL (
      SELECT conversation.author_id::bigint AS user_id
      UNION
      SELECT (participant.entity_meta ->> 'id')::bigint
      FROM chat_participant_mapping mapping
      JOIN chat_participants participant ON participant.id = mapping.participant_id AND participant.entity_name = 'user'
      WHERE mapping.conversation_id = conversation.id AND participant.entity_meta ->> 'id' ~ '^[1-9][0-9]{0,17}$'
    ) reader
    WHERE conversation.uuid = CASE WHEN ` + streamExpression + ` ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                    THEN (` + streamExpression + `)::uuid END
      AND reader.user_id > 0
    ORDER BY reader.user_id
    LIMIT ` + strconv.Itoa(maxLiveAudience+1) + `
  ) readers), '{}'::bigint[])`
}

// liveEventData is the project-plane payload of agent_hitl_resolved: the
// fanout-interrupt-resolved object (never a decision value) plus the
// conversation and root response it belongs to.
func liveEventData(metadata []byte, conversationID, rootResponseID string) (map[string]any, error) {
	var data map[string]any
	if err := json.Unmarshal(metadata, &data); err != nil {
		return nil, domain.ErrLedgerFault
	}
	data["conversation_id"] = conversationID
	data["root_response_id"] = rootResponseID
	return data, nil
}

// liveEvent is one project-plane event owed after commit.
type liveEvent struct {
	eventType string
	data      map[string]any
}

// resolvedNotice is what one committed change owes open tabs: the replay
// wake of the execution whose replay log received agent_hitl_resolved frames
// (highest cursor), and the project-plane events for the conversation's
// readers (audience), computed in the committing transaction.
type resolvedNotice struct {
	projectID      int64
	executionID    string
	cursor         int64
	conversationID string
	audience       []int64
	live           []liveEvent
}

func (n resolvedNotice) valid() bool {
	return n.projectID > 0 && n.executionID != "" && n.cursor > 0
}

// InterruptReplayNotifier wakes live replay streams of one execution (the
// replay wake bus, runtimecomposition/execution_replay_wake.go).
type InterruptReplayNotifier interface {
	Notify(projectID, executionID string, cursor uint64)
}

// FanoutWakeKicker starts wake admission (agentexecution.FanoutWakeAdmitter).
type FanoutWakeKicker interface {
	Kick()
}

// InterruptLiveEvents publishes one event on the project SSE plane's
// audience-scoped conversations family (events.ConversationsChannel). The payload carries an "audience" member (the user
// ids allowed to receive it); the SSE relay delivers the frame only to those
// callers and strips the member.
type InterruptLiveEvents interface {
	PublishInterruptEvent(ctx context.Context, projectID int64, eventType string, payload map[string]any)
}

// ExecutionInterruptSignals are the ledger's after-commit signals. Every
// method is called only after the transaction that made the change visible
// has committed, and both are advisory: a lost signal costs latency (the
// replay poll, the wake sweep), never correctness. The zero value and a nil
// pointer are silent. Fields are set once during composition, before any
// request or output is served.
type ExecutionInterruptSignals struct {
	Replay InterruptReplayNotifier
	Wakes  FanoutWakeKicker
	Live   InterruptLiveEvents
}

func (s *ExecutionInterruptSignals) resolved(ctx context.Context, notices ...resolvedNotice) {
	if s == nil {
		return
	}
	for _, notice := range notices {
		if s.Replay != nil && notice.valid() {
			s.Replay.Notify(strconv.FormatInt(notice.projectID, 10), notice.executionID, uint64(notice.cursor))
		}
		if s.Live == nil || len(notice.live) == 0 || notice.projectID <= 0 {
			continue
		}
		if len(notice.audience) == 0 || len(notice.audience) > maxLiveAudience {
			// No live copy; the durable replay copy and a reload still show it.
			slog.Info("execution interrupt live event not published", "reason", "audience_out_of_bounds",
				"readers", len(notice.audience))
			continue
		}
		for _, event := range notice.live {
			payload := make(map[string]any, len(event.data)+1)
			for key, value := range event.data {
				payload[key] = value
			}
			payload["audience"] = notice.audience
			s.Live.PublishInterruptEvent(context.WithoutCancel(ctx), notice.projectID, event.eventType, payload)
		}
	}
}

func (s *ExecutionInterruptSignals) wakeQueued(queued bool) {
	if queued && s != nil && s.Wakes != nil {
		s.Wakes.Kick()
	}
}

func wakeClosedFailureCode(state domain.State) string {
	if state == domain.StateSuperseded {
		return "response_superseded"
	}
	return "response_cancelled"
}

// insertWake writes the wake row and clears the park in one statement. The
// partial unique index allows one active wake per response; the caller holds
// the per-response row lock, so a second active wake is a ledger fault.
func insertWake(ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID string, decisionRevision int64, parkedExecution string, parkedGeneration int64) error {
	if err := retireSettledWakes(ctx, tx, rootResponseID); err != nil {
		return err
	}
	var queued int
	err := tx.QueryRow(ctx, `
WITH unparked AS (
  UPDATE elitea_runtime.execution_interrupt_responses
  SET parked_execution_id = NULL, parked_generation = NULL, parked_revision_seen = NULL, parked_at = NULL,
      updated_at = clock_timestamp()
  WHERE root_response_id = $1::uuid AND project_id = $2
  RETURNING root_response_id
), queued AS (
  INSERT INTO elitea_runtime.execution_interrupt_wakes (
      root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state)
  SELECT root_response_id, $3, $2, $4, $5, $6, 'QUEUED' FROM unparked
  ON CONFLICT DO NOTHING
  RETURNING 1
)
SELECT count(*) FROM queued`, rootResponseID, projectID, decisionRevision, parkedExecution, parkedGeneration,
		agentexecutionapp.FanoutWakeIdempotencyKey(rootResponseID, decisionRevision)).Scan(&queued)
	if err != nil {
		return fmt.Errorf("execution interrupt wake: %w", err)
	}
	if queued != 1 {
		return domain.ErrLedgerFault
	}
	return nil
}

// ParkSettlement is the outcome of settling a parked execution.
type ParkSettlement struct {
	// WakeQueued: a decision is already waiting, so one wake was written in
	// the settlement transaction. Otherwise the response is recorded parked.
	WakeQueued       bool
	DecisionRevision int64
}

// SettleParkedResponse records that an execution parked with open cards
// (terminal frame fanout_parked_v1, contract §8). It must run on the
// transaction that accepts that terminal frame, after the chat_message_group
// response row is locked (the terminal projection's lock), so it serializes
// with Decide. Under the per-response row lock:
//   - a wake admitted for this very execution is DONE (it ran and parked);
//   - if decision_revision > seen, or any card is DECIDED, one wake is
//     queued now;
//   - otherwise the response is recorded parked, and the next decision
//     queues the wake.
func (r *ExecutionInterruptRepository) SettleParkedResponse(ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID, executionID string, generation, decisionRevisionSeen int64) (ParkSettlement, error) {
	return settleParkedResponse(ctx, tx, projectID, rootResponseID, executionID, generation, decisionRevisionSeen)
}

func settleParkedResponse(ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID, executionID string, generation, decisionRevisionSeen int64) (ParkSettlement, error) {
	if tx == nil || projectID <= 0 || !domain.ValidResponseMessageID(rootResponseID) || !domain.ValidExecutionID(executionID) ||
		generation <= 0 || decisionRevisionSeen < 0 {
		return ParkSettlement{}, domain.ErrInvalidRaise
	}
	var current int64
	var decided bool
	err := tx.QueryRow(ctx, `
SELECT response.decision_revision,
       EXISTS (SELECT 1 FROM elitea_runtime.execution_interrupts card
               WHERE card.root_response_id = response.root_response_id AND card.state = 'DECIDED')
FROM elitea_runtime.execution_interrupt_responses response
WHERE response.root_response_id = $1::uuid AND response.project_id = $2
FOR UPDATE OF response`, rootResponseID, projectID).Scan(&current, &decided)
	if errors.Is(err, pgx.ErrNoRows) {
		// A park always has open cards, and every card has this row.
		return ParkSettlement{}, domain.ErrLedgerFault
	}
	if err != nil {
		return ParkSettlement{}, fmt.Errorf("execution interrupt park: %w", err)
	}
	if decisionRevisionSeen > current {
		return ParkSettlement{}, domain.ErrLedgerFault
	}
	if _, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_interrupt_wakes
SET state = 'DONE', updated_at = clock_timestamp()
WHERE root_response_id = $1::uuid AND state = 'ADMITTED' AND wake_execution_id = $2`, rootResponseID, executionID); err != nil {
		return ParkSettlement{}, fmt.Errorf("execution interrupt park: %w", err)
	}
	settlement := ParkSettlement{DecisionRevision: current}
	if current > decisionRevisionSeen || decided {
		if err := retireSettledWakes(ctx, tx, rootResponseID); err != nil {
			return ParkSettlement{}, err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.execution_interrupt_wakes
WHERE root_response_id = $1::uuid AND state IN ('QUEUED', 'ADMITTED'))`, rootResponseID).Scan(&active); err != nil {
			return ParkSettlement{}, fmt.Errorf("execution interrupt park: %w", err)
		}
		if active {
			// Another continuation is queued or running; its restore fetch
			// delivers the decision.
			return settlement, nil
		}
		if err := insertWake(ctx, tx, projectID, rootResponseID, current, executionID, generation); err != nil {
			return ParkSettlement{}, err
		}
		settlement.WakeQueued = true
		return settlement, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_interrupt_responses
SET parked_execution_id = $3, parked_generation = $4, parked_revision_seen = $5, parked_at = clock_timestamp(),
    updated_at = clock_timestamp()
WHERE root_response_id = $1::uuid AND project_id = $2`, rootResponseID, projectID, executionID, generation, decisionRevisionSeen)
	if err != nil {
		return ParkSettlement{}, fmt.Errorf("execution interrupt park: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ParkSettlement{}, domain.ErrLedgerFault
	}
	return settlement, nil
}

// fenceFromClaim turns the verified transport claim into the ledger's fence.
// The workload identity comes only from the verified mTLS peer certificate.
func fenceFromClaim(claim storage.ContentClaim) (domain.ClaimFence, error) {
	identity, err := workloadidentity.Certificate(claim.PeerCertificate)
	if err != nil || claim.Generation == 0 || claim.Generation > math.MaxInt64 {
		return domain.ClaimFence{}, storage.ErrInterruptClaimRefused
	}
	fence := domain.ClaimFence{
		ClaimID: claim.ClaimID, ExecutionID: claim.ExecutionID, Generation: int64(claim.Generation),
		WorkloadIdentity: identity, FenceToken: claim.FenceToken,
	}
	if !fence.Valid() {
		return domain.ClaimFence{}, storage.ErrInterruptClaimRefused
	}
	return fence, nil
}

// privateInterruptError maps ledger refusals to the contract §7.1 classes.
// Only a fixed reason is logged: never a key, value, body, header or token.
func privateInterruptError(route string, err error) error {
	switch {
	case errors.Is(err, domain.ErrStaleFence), errors.Is(err, storage.ErrInterruptClaimRefused):
		return storage.ErrInterruptClaimRefused
	case errors.Is(err, domain.ErrInvalidAck):
		return storage.ErrInterruptRequestInvalid
	case errors.Is(err, domain.ErrNotFound):
		return storage.ErrInterruptNotFound
	case errors.Is(err, domain.ErrAckConflict):
		return storage.ErrInterruptConflict
	case errors.Is(err, domain.ErrLedgerFault):
		slog.Error("execution interrupt private route failed", "route", route, "reason", "ledger_invariant")
		return storage.ErrContentUnavailable
	default:
		slog.Error("execution interrupt private route failed", "route", route, "reason", "unavailable")
		return storage.ErrContentUnavailable
	}
}

// FetchExecutionInterrupts serves POST …/interrupts/fetch (contract §7).
func (r *ExecutionInterruptRepository) FetchExecutionInterrupts(ctx context.Context, claim storage.ContentClaim) ([]byte, error) {
	fence, err := fenceFromClaim(claim)
	if err != nil {
		return nil, err
	}
	fetch, err := r.FetchDecided(ctx, fence)
	if err != nil {
		return nil, privateInterruptError("fetch", err)
	}
	body, err := fetch.CanonicalJSON()
	if err != nil {
		return nil, privateInterruptError("fetch", domain.ErrLedgerFault)
	}
	return body, nil
}

// AckExecutionInterrupt serves POST …/interrupts/ack (contract §7).
func (r *ExecutionInterruptRepository) AckExecutionInterrupt(ctx context.Context, claim storage.ContentClaim, raw []byte) ([]byte, error) {
	fence, err := fenceFromClaim(claim)
	if err != nil {
		return nil, err
	}
	result, err := r.Ack(ctx, fence, raw)
	if err != nil {
		return nil, privateInterruptError("ack", err)
	}
	body, err := result.CanonicalJSON()
	if err != nil {
		return nil, privateInterruptError("ack", domain.ErrLedgerFault)
	}
	return body, nil
}

var _ storage.ExecutionInterruptControlStore = (*ExecutionInterruptRepository)(nil)

// isFanoutWakeExecution reports whether the execution is a fanout_wake
// continuation (the job a wake row was admitted as).
func isFanoutWakeExecution(ctx context.Context, tx sqlExecutor, executionID string) (bool, error) {
	var wake bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.execution_interrupt_wakes WHERE wake_execution_id = $1)`, executionID).Scan(&wake)
	if err != nil {
		return false, fmt.Errorf("load fanout wake binding: %w", err)
	}
	return wake, nil
}
