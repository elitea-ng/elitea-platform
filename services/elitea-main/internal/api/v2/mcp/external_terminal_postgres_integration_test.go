package mcp_test

import (
	"strings"
	"testing"

	agent "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/stretchr/testify/require"
)

// The real database holds projected turns. Admission and worker execution are doubles.
func TestExternalAgentAndPipelineRefusePartialOutputAtContinuationBoundary(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "pipeline"}[pipeline], func(t *testing.T) {
			pool := newMCPPool(t)
			seedUser(t, pool, callerUserID)
			if pipeline {
				seedPipeline(t, pool, homeSchema, "Bounded", "bounded pipeline", "mcp")
			} else {
				seedAgent(t, pool, homeSchema, "Bounded", "bounded agent", "mcp")
			}
			const message = "6f1d1d3a-0d0a-4a0e-9a0e-1c2d3e4f5a6d"
			start := &fakeStart{outcome: agent.CurrentApplicationStartOutcome{ExecutionID: "bounded-run", ResponseMessageID: message}}
			start.onStart = func(request agent.CurrentApplicationStartRequest) {
				projectAnswer(t, pool, homeSchema, request.ConversationUUID, message, `{"is_error":false,"output_limit_reached":true}`, "PARTIAL_CANARY")
			}
			result := callTool(t, pool, start, "/app/1/mcp", "Bounded", "go")
			require.Equal(t, true, result["isError"])
			text := resultText(t, result)
			require.Contains(t, text, "output limit")
			require.Contains(t, text, "bounded-run")
			require.False(t, strings.Contains(text, "PARTIAL_CANARY"))
		})
	}
}
