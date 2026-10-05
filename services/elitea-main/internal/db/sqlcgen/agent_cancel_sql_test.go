package sqlcgen

import (
	"strings"
	"testing"
)

func TestCurrentAgentCancelSQLFencesTerminalJobsAndPreservesCurrentProjection(t *testing.T) {
	for _, fragment := range []string{
		"job.desired_state IN ('RUNNING', 'SUSPENDED')",
		"'PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING'",
		"OR job.desired_state = 'CANCELLED'",
		"conversation.author_id = $3::bigint",
		"question_author.entity_meta ->> 'id'",
	} {
		if !strings.Contains(cancelCurrentAgentExecution, fragment) {
			t.Fatalf("cancellation SQL missing %q", fragment)
		}
	}
	normalizedCancelSQL := strings.Join(strings.Fields(cancelCurrentAgentExecution), " ")
	for _, fence := range []string{
		"job.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED') AND target.is_streaming",
		"job.state = 'SUCCEEDED' AND target.has_pause_projection",
	} {
		if !strings.Contains(normalizedCancelSQL, fence) {
			t.Fatalf("settled cancellation SQL missing conditional fence %q", fence)
		}
	}
	for _, fragment := range []string{
		"job.actor_id = $3::bigint::text",
		"job.desired_state = 'CANCELLED'",
	} {
		if !strings.Contains(isCurrentAgentCancellationReplay, fragment) {
			t.Fatalf("replay SQL missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"SET is_streaming = FALSE",
		"- 'hitl_interrupt'",
		"- 'hitl_interrupts'",
		"- 'authorization_requests'",
		"trace.kind = 'thinking_step'",
		"INSERT INTO chat_messages_text",
		"DELETE FROM chat_message_items AS item",
		"DELETE FROM chat_message_group AS response",
		"DELETE FROM chat_message_group AS question",
	} {
		if !strings.Contains(projectCurrentAgentStop, fragment) {
			t.Fatalf("projection SQL missing %q", fragment)
		}
	}
}
