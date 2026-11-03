package agentexecution

import (
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// startUnattendedBody is the start body's `unattended` object (ADR-0045,
// client contract 1.30).
type startUnattendedBody struct {
	// GrantID is optional: Main derives a stable one from the question.
	GrantID             string `json:"grant_id"`
	ContinueStepLimit   bool   `json:"continue_step_limit"`
	ContinueOutputLimit bool   `json:"continue_output_limit"`
	SensitiveTools      []struct {
		ToolkitName string `json:"toolkit_name"`
		ToolName    string `json:"tool_name"`
	} `json:"sensitive_tools"`
	Chain *struct {
		MaxContinuations *int64   `json:"max_continuations"`
		MaxSpend         *float64 `json:"max_spend"`
		DeadlineSeconds  *int64   `json:"deadline_seconds"`
	} `json:"chain"`
	// The desktop-only parts. A cloud start that sets one is refused.
	ContinueToolCallCap bool `json:"continue_tool_call_cap"`
	HookAsks            bool `json:"hook_asks"`
	RemoteConfirmations bool `json:"remote_confirmations"`
}

func (b *startUnattendedBody) request() *unattended.Request {
	if b == nil {
		return nil
	}
	request := &unattended.Request{
		GrantID: b.GrantID, ContinueStepLimit: b.ContinueStepLimit, ContinueOutputLimit: b.ContinueOutputLimit,
		ContinueToolCallCap: b.ContinueToolCallCap, HookAsks: b.HookAsks, RemoteConfirmations: b.RemoteConfirmations,
	}
	for _, tool := range b.SensitiveTools {
		request.SensitiveTools = append(request.SensitiveTools,
			unattended.Tool{ToolkitName: tool.ToolkitName, ToolName: tool.ToolName})
	}
	if b.Chain != nil {
		request.Chain = unattended.ChainRequest{
			MaxContinuations: b.Chain.MaxContinuations, MaxSpendUSD: b.Chain.MaxSpend,
			DeadlineSeconds: b.Chain.DeadlineSeconds,
		}
	}
	return request
}

// writeUnattendedStartError answers a refused grant: 422
// unattended_grant_refused with the field, the tool and the reason.
func writeUnattendedStartError(writer http.ResponseWriter, err error) bool {
	var refusal *unattended.RefusalError
	if !errors.As(err, &refusal) {
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
	writeJSON(writer, http.StatusUnprocessableEntity, body)
	return true
}
