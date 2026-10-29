package control

import (
	"context"
	"errors"
	"log/slog"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
)

// RunSteps persists the worker's step records (ADR-0042 decision 1, #1499)
// and, when a run settles, asks for its chain to be anchored
// (*runevidence.Service).
type RunSteps interface {
	// Report stores the claimed generation's steps and returns how many were
	// new. runevidence.ErrInvalidReport refuses the report as malformed.
	Report(ctx context.Context, executionID string, generation uint64, steps []runevidence.StepInput) (int, error)
	// RequestAnchor marks the run for main.run_step_anchor, which appends its
	// head to the tenant's security audit chain within about a minute. It
	// takes no chain lock, so settlements do not serialise on the chain.
	RequestAnchor(ctx context.Context, executionID string) error
}

// WithRunSteps serves ReportRunSteps and requests each run's anchor when
// its settlement is prepared. Nil answers UNSUPPORTED_CAPABILITY.
func (s *Server) WithRunSteps(steps RunSteps) *Server {
	s.runSteps = steps
	return s
}

// ReportRunSteps stores the step records of one claimed execution. The peer
// is authenticated and the fence verified live, as ReportEgressSummary does;
// a report for a released or stale claim is refused, and so is one outside
// the contract's bounds (non-retryable). A storage failure is retryable: the
// worker resends, and the step_ids already stored are skipped.
func (s *Server) ReportRunSteps(ctx context.Context, request *runtimev1.ReportRunStepsRequestV1) (*runtimev1.ReportRunStepsResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The run step report is malformed.", false), nil
	}
	if len(request.GetSteps()) > runevidence.MaxStepsPerReport {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The run step report is over its bounds.", false), nil
	}
	if s.runSteps == nil {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Run step records are not served by this deployment.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return runStepsRejection(code, message, retryable), nil
	}
	steps := make([]runevidence.StepInput, 0, len(request.GetSteps()))
	for _, step := range request.GetSteps() {
		steps = append(steps, runStepInput(step))
	}
	accepted, err := s.runSteps.Report(ctx, fence.ExecutionID, fence.Generation, steps)
	if errors.Is(err, runevidence.ErrInvalidReport) {
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "A run step is outside the contract.", false), nil
	}
	if err != nil {
		slog.WarnContext(ctx, "run steps not stored", "execution_id", fence.ExecutionID, "err", err)
		return runStepsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The run steps could not be stored; retry.", true), nil
	}
	return &runtimev1.ReportRunStepsResponseV1{Accepted: uint32(accepted)}, nil
}

// requestRunStepAnchor is best effort and off the chain: settlement only
// marks the run's head, and main.run_step_anchor appends the anchor. A
// missed mark is caught by step_record's hourly pass, which anchors every
// settled run whose head moved, so an anchor is late, never lost.
func (s *Server) requestRunStepAnchor(ctx context.Context, executionID string) {
	if s.runSteps == nil {
		return
	}
	if err := s.runSteps.RequestAnchor(ctx, executionID); err != nil {
		slog.WarnContext(ctx, "run step anchor not requested at settlement; the step_record routine anchors it",
			"execution_id", executionID, "err", err)
	}
}

func runStepInput(step *runtimev1.RunStepV1) runevidence.StepInput {
	in := runevidence.StepInput{
		StepID:           step.GetStepId(),
		Kind:             runStepKind(step.GetKind()),
		StartedAt:        unixMillis(step.GetStartedAtUnixMs()),
		EndedAt:          unixMillis(step.GetEndedAtUnixMs()),
		NodeID:           step.GetNodeId(),
		Name:             step.GetName(),
		ToolkitID:        step.GetToolkitId(),
		SummaryJSON:      step.GetSummaryJson(),
		Inputs:           runStepContent(step.GetInputs()),
		Outputs:          runStepContent(step.GetOutputs()),
		CitedStepIDs:     step.GetCitedStepIds(),
		CitedDocumentIDs: step.GetCitedDocumentIds(),
	}
	if r := step.GetRetrieval(); r != nil {
		retrieval := &runevidence.Retrieval{
			IndexName: r.GetIndexName(), Query: r.GetQuery(), FiltersJSON: r.GetFiltersJson(), TopK: r.GetTopK(),
		}
		for _, d := range r.GetDocuments() {
			retrieval.Documents = append(retrieval.Documents, runevidence.Document{
				DocumentID: d.GetDocumentId(), Source: d.GetSource(), Score: d.GetScore(), Rank: d.GetRank(),
			})
		}
		in.Retrieval = retrieval
	}
	return in
}

func runStepContent(content *runtimev1.RunStepContentV1) *runevidence.ContentInput {
	if content == nil {
		return nil
	}
	return &runevidence.ContentInput{
		Bytes: content.GetContent(), MediaType: content.GetMediaType(),
		Truncated: content.GetTruncated(), OriginalSize: content.GetOriginalSizeBytes(),
	}
}

func runStepKind(kind runtimev1.RunStepKindV1) runevidence.Kind {
	switch kind {
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_MODEL_CALL:
		return runevidence.KindModelCall
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_TOOL_CALL:
		return runevidence.KindToolCall
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_RETRIEVAL:
		return runevidence.KindRetrieval
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_APPROVAL:
		return runevidence.KindApproval
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_DELEGATION:
		return runevidence.KindDelegation
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_CAP:
		return runevidence.KindCap
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_ERROR:
		return runevidence.KindError
	case runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_OUTPUT:
		return runevidence.KindOutput
	}
	return "" // refused by validation
}

func unixMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func runStepsRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ReportRunStepsResponseV1 {
	return &runtimev1.ReportRunStepsResponseV1{Rejection: runtimeError(code, message, retryable)}
}
