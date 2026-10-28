package agentexecution

import (
	"context"
	"errors"
	"log/slog"
)

// GovernedRefusalErrorCode is the `error_code` a governed refusal settles a
// paused turn with: the same machine code the refused continuation answers.
const GovernedRefusalErrorCode = "governed_credential_refused"

// GovernedRefusalSettlement names the paused turn a refused governed
// continuation settles (#1485). Reason is shown to the user as the answer's
// error; it names the rule, never a credential, a reference or a project.
type GovernedRefusalSettlement struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
	Code              string
	Reason            string
}

// GovernedRefusalSettler settles a paused turn whose continuation admission
// refused: it clears the pause keys, stamps `is_error` with the safe reason
// and cancels the paused execution, the way a stop does. settled=false when
// there was no paused turn of this actor to settle (already settled, or
// stopped by a concurrent request).
type GovernedRefusalSettler interface {
	SettleGovernedRefusal(context.Context, GovernedRefusalSettlement) (settled bool, err error)
}

// WithGovernedRefusalSettlement attaches the settler a refused governed
// continuation uses. A service without it answers the refusal and leaves the
// pause as it was.
func (service *CurrentApplicationStartService) WithGovernedRefusalSettlement(
	settler GovernedRefusalSettler,
) *CurrentApplicationStartService {
	if service != nil {
		service.governedRefusals = settler
	}
	return service
}

// governedRefusalReason is the user-visible reason for a refusal code. It
// says what happened to the agent's access, never which credential.
func governedRefusalReason(code string) string {
	switch code {
	case "credential_grant_missing":
		return "This agent's access to a toolkit was revoked while it waited for your decision, so the step did not run."
	case "credential_reference_not_active", "credential_reference_not_found":
		return "The credential this agent's toolkit used was retired while it waited for your decision, so the step did not run."
	}
	return "This agent may no longer use a toolkit credential it needed, so the step did not run."
}

// settleGovernedRefusal settles the paused turn of a continuation that
// admission refused under the governed profile. The refusal itself is what
// the caller answers; a failed settlement is logged and does not change it.
func (service *CurrentApplicationStartService) settleGovernedRefusal(
	ctx context.Context,
	request CurrentContinuationRequest,
	refusal *GovernedCredentialAdmissionError,
) {
	if service.governedRefusals == nil || refusal == nil {
		return
	}
	settled, err := service.governedRefusals.SettleGovernedRefusal(ctx, GovernedRefusalSettlement{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ResponseMessageID: request.ResponseMessageID,
		Code:              refusal.Code,
		Reason:            governedRefusalReason(refusal.Code),
	})
	switch {
	case err != nil && !errors.Is(err, context.Canceled):
		slog.ErrorContext(ctx, "settle a refused governed continuation",
			"response_message_id", request.ResponseMessageID, "code", refusal.Code, "err", err)
	case err == nil && !settled:
		slog.InfoContext(ctx, "a refused governed continuation found no paused turn to settle",
			"response_message_id", request.ResponseMessageID, "code", refusal.Code)
	}
}
