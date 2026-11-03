package agentexecution

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/unattendedwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// startUnattendedBody is the start body's `unattended` object (ADR-0045,
// client contract 1.30). The shared parts are unattendedwire.Grant; the
// desktop-only parts there (continue_tool_call_cap, hook_asks,
// remote_confirmations) refuse a cloud start.
type startUnattendedBody struct {
	unattendedwire.Grant
	// GrantID (in unattendedwire.Grant) is optional here: Main derives a
	// stable one from the question.
	ContinueOutputLimit bool              `json:"continue_output_limit"`
	SensitiveTools      []unattended.Tool `json:"sensitive_tools"`
}

func (b *startUnattendedBody) request() *unattended.Request {
	if b == nil {
		return nil
	}
	request := b.Request()
	request.ContinueOutputLimit = b.ContinueOutputLimit
	request.SensitiveTools = append(request.SensitiveTools, b.SensitiveTools...)
	return &request
}
