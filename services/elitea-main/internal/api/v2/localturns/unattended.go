package localturns

import (
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// unattendedBody is the start's `unattended` object (ADR-0045, client
// contract 1.30).
type unattendedBody struct {
	GrantID              string     `json:"grant_id"`
	ChainRootExecutionID string     `json:"chain_root_execution_id"`
	ContinueStepLimit    bool       `json:"continue_step_limit"`
	ContinueToolCallCap  bool       `json:"continue_tool_call_cap"`
	HookAsks             bool       `json:"hook_asks"`
	RemoteConfirmations  bool       `json:"remote_confirmations"`
	Chain                *chainBody `json:"chain"`
}

// chainBody is a grant's chain limits. Each field is optional.
type chainBody struct {
	MaxContinuations *int64   `json:"max_continuations"`
	MaxSpend         *float64 `json:"max_spend"`
	DeadlineSeconds  *int64   `json:"deadline_seconds"`
}

func (b *unattendedBody) request() *unattended.Request {
	if b == nil {
		return nil
	}
	request := &unattended.Request{
		GrantID: b.GrantID, ChainRootExecutionID: b.ChainRootExecutionID,
		ContinueStepLimit: b.ContinueStepLimit, ContinueToolCallCap: b.ContinueToolCallCap,
		HookAsks: b.HookAsks, RemoteConfirmations: b.RemoteConfirmations,
	}
	if b.Chain != nil {
		request.Chain = unattended.ChainRequest{
			MaxContinuations: b.Chain.MaxContinuations, MaxSpendUSD: b.Chain.MaxSpend,
			DeadlineSeconds: b.Chain.DeadlineSeconds,
		}
	}
	return request
}

// unattendedStarted is LocalTurnStarted.unattended.
func unattendedStarted(started localturn.UnattendedStarted) map[string]any {
	return map[string]any{
		"grant_id":                started.GrantID,
		"chain_root_execution_id": started.ChainRootExecutionID,
		"continuation_index":      started.ContinuationIndex,
		"chain":                   ChainLimits(started.Limits),
	}
}

// ChainLimits is the wire form of a chain's effective limits: the request's
// shape, with max_spend in USD (0 is no spend limit).
func ChainLimits(limits unattended.Limits) map[string]any {
	return map[string]any{
		"max_continuations": limits.MaxContinuations,
		"max_spend":         limits.MaxSpendUSD,
		"deadline_seconds":  int64(limits.Deadline.Seconds()),
	}
}

// writeUnattendedError answers a refused grant (422 unattended_grant_refused)
// and a refused continuation (409 chain_limit_reached, 409 chain_ended).
func writeUnattendedError(writer http.ResponseWriter, err error) bool {
	var refusal *unattended.RefusalError
	var limit *unattended.ChainLimitError
	switch {
	case errors.As(err, &refusal):
		WriteGrantRefusal(writer, refusal)
	case errors.As(err, &limit):
		desktopwire.WriteJSON(writer, http.StatusConflict, map[string]any{
			"error":   "chain_limit_reached",
			"message": "The unattended chain reached its " + limit.Limit + " limit. It does not continue.",
			"limit":   limit.Limit,
		})
	case errors.Is(err, unattended.ErrChainEnded):
		desktopwire.WriteError(writer, http.StatusConflict, "chain_ended",
			"The unattended chain has ended. Start a new turn without chain_root_execution_id.")
	case errors.Is(err, localturn.ErrConfirmationGrantRefused):
		desktopwire.WriteError(writer, http.StatusForbidden, "confirmation_grant_refused",
			"The turn's unattended grant does not answer remote toolkit confirmations.")
	default:
		return false
	}
	return true
}

// WriteGrantRefusal answers 422 unattended_grant_refused with the field, the
// tool (when one is refused) and the reason.
func WriteGrantRefusal(writer http.ResponseWriter, refusal *unattended.RefusalError) {
	body := map[string]any{
		"error":   "unattended_grant_refused",
		"message": "The unattended grant was refused: " + refusal.Reason + ".",
		"reason":  refusal.Reason,
	}
	if refusal.Field != "" {
		body["field"] = refusal.Field
	}
	if refusal.Tool != "" {
		body["tool"] = refusal.Tool
	}
	desktopwire.WriteJSON(writer, http.StatusUnprocessableEntity, body)
}
