package indexschedule

// Mandate-bound index schedules (ADR-0039, issue #1122).
//
// With credential governance on, the composition root calls WithMandates.
// Every scheduled index occurrence then needs the mandate attached to its
// index (tenant 0158, index_schedule_mandates), checked at the occurrence:
//
//   - no mandate, or one that is not usable or no longer covers the
//     schedule's cron: the occurrence is not admitted. It is recorded as an
//     initialization failure whose reason leads with the result code
//     (MANDATE_REQUIRED, MANDATE_NOT_USABLE, ...), and a security audit
//     decision event is appended;
//   - a usable mandate: the admission names it in the run's security
//     context (securitycontext.WithRunAuthority), and the project-system
//     identity preflight is skipped. Neither it nor the claim-time token
//     resolution (storage.EliteaClientTokenService) calls IssueProjectToken
//     for a governed index run.
//
// Without WithMandates the executor is unchanged.

import (
	"context"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// ScheduleMandates reads an index's attached mandate; empty for none.
type ScheduleMandates interface {
	IndexScheduleMandate(ctx context.Context, projectID, toolkitID int64, indexName string) (string, error)
}

// MandateGate is the fire-time mandate check (runmandate.Service).
type MandateGate interface {
	Admit(ctx context.Context, mandateID string, target runmandate.Target) (securitycontext.RunAuthority, error)
	Refuse(ctx context.Context, target runmandate.Target, mandateID string, cause error) error
}

// WithMandates makes the executor mandate-bound. Both must be set; a nil
// either leaves the executor as it was.
func (executor *CurrentExecutor) WithMandates(mandates ScheduleMandates, gate MandateGate) *CurrentExecutor {
	if executor != nil && mandates != nil && gate != nil {
		executor.mandates, executor.gate = mandates, gate
	}
	return executor
}

// admitMandate checks the candidate's mandate. It answers the context the
// admission must run in and the user the toolkit is read as (the mandate's
// subject, when it has one), or the outcome of a refused occurrence. An
// error is a dependency failure.
func (executor *CurrentExecutor) admitMandate(
	ctx context.Context, candidate Candidate, accessUserID int64,
) (context.Context, int64, *ExecutionOutcome, error) {
	mandateID, err := executor.mandates.IndexScheduleMandate(ctx, candidate.ProjectID, candidate.ToolkitID, candidate.IndexMetaID)
	if err != nil {
		return ctx, 0, nil, err
	}
	target := runmandate.Target{
		Origin:          runmandate.OriginIndexSchedule,
		TenantProjectID: candidate.ProjectID,
		ResourceID:      strconv.FormatInt(candidate.ToolkitID, 10) + ":" + candidate.IndexMetaID,
		Cron:            candidate.Schedule.Cron,
	}
	authority, err := executor.gate.Admit(ctx, mandateID, target)
	if err != nil {
		code := runmandate.Code(err)
		if code == "" {
			return ctx, 0, nil, err
		}
		if auditErr := executor.gate.Refuse(ctx, target, mandateID, err); auditErr != nil {
			return ctx, 0, nil, auditErr
		}
		refused := scheduleInitializationFailure(code + ": " + mandateRefusalReason(code))
		return ctx, 0, &refused, nil
	}
	if authority.SubjectUserID > 0 {
		accessUserID = authority.SubjectUserID
	}
	return securitycontext.WithRunAuthority(ctx, authority), accessUserID, nil, nil
}

func mandateRefusalReason(code string) string {
	switch code {
	case runmandate.CodeMandateRequired:
		return "attach a mandate to this index schedule; governed schedules run only under one"
	case runmandate.CodeMandateScopeMismatch:
		return "the mandate's scope does not cover this schedule's cron"
	}
	return "the attached mandate is not usable for this index schedule"
}
