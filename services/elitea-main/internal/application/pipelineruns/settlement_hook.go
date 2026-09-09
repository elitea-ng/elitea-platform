// Package pipelineruns is the "isolated hook" internal/events.
// EventPipelineRunSucceeded and EventPipelineRunFailed's own doc comment
// said did not exist: a way to report "this execution — one that started as
// an UNATTENDED pipeline run — just finished, successfully or not" without
// editing the claim-fence/settlement engine's own fencing logic.
//
// # The two halves, and why they are split across two producers
//
//  1. internal/api/v2/pipelinetriggers' admit() (the one admission path both
//     the inbound trigger and the schedule tick use) calls RecordRunStart the
//     moment it emits pipeline.run.started — an ordinary write, in the SAME
//     request, to `public.pipeline_runs` (migrations/shared/0124), nowhere
//     near elitea_runtime's claim-fence tables. This is what lets a LATER,
//     completely separate settlement event answer "is this execution a
//     pipeline run, and if so, which pipeline/version/conversation".
//
//  2. execution.SettlementService's AfterSettle hook (NewSettlementHook,
//     below) fires once a run's outcome is DURABLY settled — after
//     PrepareSettlement's own transaction has committed, never before, never
//     from inside it. It looks the execution id up in the table (1) wrote,
//     and if a row is there, emits pipeline.run.succeeded or
//     pipeline.run.failed. If no row is there, this was not a pipeline run
//     (an ordinary chat turn, an MCP tools/call, a config validation, an
//     index ingest — settlement is capability-generic and fires for all of
//     them) and the hook does nothing.
//
// output.RuntimeFailureService's FailureObserver (NewFailureObserver, also
// below) is a THIRD, smaller producer: it captures a FAILED run's error text
// — RuntimeFailureFrame.Failure.SafeMessage, the one place that text exists
// — into the same row, before settlement, so the AfterSettle hook has an
// error_summary to put in the payload by the time it fires. The
// PrepareSettlement protocol's own ordering rule (a settlement cannot
// prepare until the terminal output row's `projected_at IS NOT NULL`)
// guarantees the observer's write always lands before the hook's read.
package pipelineruns

import (
	"context"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
)

// Run is one admitted pipeline run's tracked identity — what RecordRunStart
// writes and ClaimForEvent reads back.
type Run struct {
	ExecutionID      string
	ProjectID        string
	ApplicationID    int64
	VersionID        int64
	ConversationUUID string
	// Origin mirrors pipeline.run.started's own `origin` field
	// (pipelinetriggers.OriginWebhook or OriginSchedule) so an operator
	// correlating the two events sees one vocabulary.
	Origin string
}

// Tracker is public.pipeline_runs' persistence seam — the
// infra/db/repos.PipelineRunsRepo implements it structurally (this package
// does not import internal/infra/db/repos, which would be a cycle: repos
// already imports application-layer packages this module sits beside).
type Tracker interface {
	// RecordRunStart inserts one row. Called once, from admit()'s own
	// request, alongside the chat rows it already writes. A duplicate
	// execution_id (should not happen — ids are minted fresh per run) is not
	// an error: ON CONFLICT DO NOTHING, so a retried admission cannot fail on
	// this write alone.
	RecordRunStart(ctx context.Context, run Run) error
	// RecordExecutionError sets error_summary for an execution id that may or
	// may not be a tracked pipeline run — a no-op (not an error) when it is
	// not, since the caller (output.RuntimeFailureService) has no way to
	// know in advance and must not treat "not a pipeline run" as a failure of
	// its own ordinary work.
	RecordExecutionError(ctx context.Context, executionID, safeMessage string) error
	// ClaimForEvent atomically claims ONE emission for executionID —
	// `UPDATE ... SET event_emitted_at = now() WHERE execution_id = $1 AND
	// event_emitted_at IS NULL RETURNING ...`. found is false when either no
	// row exists (not a pipeline run) or the row was already claimed (a
	// settlement replay, or two hook goroutines racing on the same
	// execution) — both cases mean "say nothing", and the caller does not
	// need to tell them apart.
	ClaimForEvent(ctx context.Context, executionID string) (run Run, startedAt time.Time, errorSummary string, found bool, err error)
}

// EventEmitter is internal/events.Publisher's Emit method, declared locally
// so this package does not import internal/events for more than its two
// event-type constants (which carry no import-cycle risk: events does not
// import this package). *events.Publisher satisfies this structurally.
type EventEmitter interface {
	Emit(ctx context.Context, projectID, eventType string, payload any)
}

// RunOutcomePayload is pipeline.run.succeeded's and pipeline.run.failed's
// shared `payload` shape — see webhooks-and-triggers.mdx's own table for the
// user-facing description of each field.
type RunOutcomePayload struct {
	ExecutionID      string `json:"execution_id"`
	ApplicationID    int64  `json:"application_id"`
	VersionID        int64  `json:"version_id"`
	ConversationUUID string `json:"conversation_uuid"`
	Status           string `json:"status"`
	DurationMs       int64  `json:"duration_ms"`
	ErrorSummary     string `json:"error_summary,omitempty"`
}

// outcomeStatus renders a SettlementOutcome the way the payload documents it
// — lowercase, matching every other event's JSON convention — rather than
// the Go constant's own SCREAMING_CASE spelling.
func outcomeStatus(outcome executionapp.SettlementOutcome) (status string, isTerminalReportable bool) {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return "succeeded", true
	case executionapp.SettlementFailed:
		return "failed", true
	case executionapp.SettlementCancelled:
		return "cancelled", true
	default:
		// SettlementOutcomeUnknown, or any future value: PrepareSettlement's
		// own proposal.Validate() already refuses a settlement carrying this,
		// so a real caller never reaches here — this exists so a hook can
		// never emit a webhook event with no defined vocabulary for "status".
		return "", false
	}
}

// NewSettlementHook builds the execution.AfterSettleHook that turns a
// committed settlement into pipeline.run.succeeded/failed, when — and only
// when — tracker has a row for the settled execution id. See the package doc
// for the full sequencing story.
func NewSettlementHook(tracker Tracker, emitter EventEmitter) executionapp.AfterSettleHook {
	return func(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
		if tracker == nil || emitter == nil {
			return
		}
		status, ok := outcomeStatus(proposal.Outcome)
		if !ok {
			return
		}
		run, startedAt, errorSummary, found, err := tracker.ClaimForEvent(ctx, proposal.Fence.ExecutionID)
		if err != nil || !found {
			// err != nil is logged by the caller of this hook
			// (execution.runAfterSettleHook's own recover/log wraps every
			// hook call) if it panics — a plain repository error here is NOT
			// a panic, so it is swallowed deliberately: a database blip on
			// this best-effort, secondary signal must not be escalated into
			// anything that could be mistaken for a settlement problem. The
			// far more common `found == false` (not a pipeline run, or
			// already emitted) is the expected, silent case for the vast
			// majority of settlements this hook sees.
			return
		}

		eventType := events.EventPipelineRunSucceeded
		if status != "succeeded" {
			eventType = events.EventPipelineRunFailed
		}
		payload := RunOutcomePayload{
			ExecutionID:      run.ExecutionID,
			ApplicationID:    run.ApplicationID,
			VersionID:        run.VersionID,
			ConversationUUID: run.ConversationUUID,
			Status:           status,
			DurationMs:       time.Since(startedAt).Milliseconds(),
		}
		if status != "succeeded" {
			payload.ErrorSummary = errorSummary
		}
		emitter.Emit(ctx, run.ProjectID, eventType, payload)
	}
}

// NewFailureObserver builds the output.FailureObserver that records a
// failed AGENT execution's safe error text into the tracked row BEFORE
// settlement — see the package doc's "why split across two producers".
func NewFailureObserver(tracker Tracker) func(ctx context.Context, executionID, safeMessage string) {
	return func(ctx context.Context, executionID, safeMessage string) {
		if tracker == nil {
			return
		}
		_ = tracker.RecordExecutionError(ctx, executionID, safeMessage)
	}
}
