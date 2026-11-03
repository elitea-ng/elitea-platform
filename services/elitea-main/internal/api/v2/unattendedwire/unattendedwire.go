// Package unattendedwire holds the wire form of an unattended grant
// (ADR-0045, client contract 1.30) that the cloud start
// (api/v2/agentexecution) and the desktop local turn (api/v2/localturns)
// share: the grant body, its chain limits and the 422
// unattended_grant_refused answer.
package unattendedwire

import (
	"encoding/json"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// Chain is a grant's chain limits. Each field is optional; an absent one
// takes the platform default.
type Chain struct {
	MaxContinuations *int64   `json:"max_continuations"`
	MaxSpend         *float64 `json:"max_spend"`
	DeadlineSeconds  *int64   `json:"deadline_seconds"`
}

// Request is the application form of the chain limits. A nil chain is all
// defaults.
func (c *Chain) Request() unattended.ChainRequest {
	if c == nil {
		return unattended.ChainRequest{}
	}
	return unattended.ChainRequest{
		MaxContinuations: c.MaxContinuations, MaxSpendUSD: c.MaxSpend, DeadlineSeconds: c.DeadlineSeconds,
	}
}

// Grant is the part of the `unattended` object that the cloud start and the
// local turn both carry. Each caller embeds it and adds its own parts.
type Grant struct {
	GrantID             string `json:"grant_id"`
	ContinueStepLimit   bool   `json:"continue_step_limit"`
	ContinueToolCallCap bool   `json:"continue_tool_call_cap"`
	HookAsks            bool   `json:"hook_asks"`
	RemoteConfirmations bool   `json:"remote_confirmations"`
	Chain               *Chain `json:"chain"`
	// GoalCheck asks for the run brief and the goal check (ADR-0045
	// decision 11). Absent is true.
	GoalCheck *bool `json:"goal_check"`
}

// Request is the application form of the shared parts.
func (g Grant) Request() unattended.Request {
	return unattended.Request{
		GrantID: g.GrantID, ContinueStepLimit: g.ContinueStepLimit, ContinueToolCallCap: g.ContinueToolCallCap,
		HookAsks: g.HookAsks, RemoteConfirmations: g.RemoteConfirmations, Chain: g.Chain.Request(),
		GoalCheck: g.GoalCheck == nil || *g.GoalCheck,
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

// WriteRefusal answers 422 unattended_grant_refused with the field, the tool
// (when one is refused) and the reason. It answers false, and writes
// nothing, when err is not a refused grant.
func WriteRefusal(writer http.ResponseWriter, err error) bool {
	refusal, ok := unattended.AsRefusal(err)
	if !ok {
		return false
	}
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
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(writer).Encode(body)
	return true
}
