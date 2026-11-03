package localturns

import (
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/unattendedwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// unattendedBody is the start's `unattended` object (ADR-0045, client
// contract 1.30): the shared grant parts and the chain root.
type unattendedBody struct {
	unattendedwire.Grant
	ChainRootExecutionID string `json:"chain_root_execution_id"`
	// Kind is a continuation's link kind: step_limit, tool_call_cap or
	// goal_not_met (ADR-0045 decision 11). Absent is continue.
	Kind string `json:"kind"`
}

func (b *unattendedBody) request() *unattended.Request {
	if b == nil {
		return nil
	}
	request := b.Request()
	request.ChainRootExecutionID = b.ChainRootExecutionID
	request.Kind = b.Kind
	return &request
}

// unattendedStarted is LocalTurnStarted.unattended.
func unattendedStarted(started localturn.UnattendedStarted) map[string]any {
	return map[string]any{
		"grant_id":                started.GrantID,
		"chain_root_execution_id": started.ChainRootExecutionID,
		"continuation_index":      started.ContinuationIndex,
		"chain":                   unattendedwire.ChainLimits(started.Limits),
	}
}

// writeUnattendedError answers a refused grant (422 unattended_grant_refused)
// and a refused continuation (409 chain_limit_reached, 409 chain_ended).
func writeUnattendedError(writer http.ResponseWriter, err error) bool {
	var limit *unattended.ChainLimitError
	switch {
	case unattendedwire.WriteRefusal(writer, err):
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
