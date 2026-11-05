package repos

import (
	"errors"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// AN AD-HOC START WITH NO TARGET IN A MULTI-AGENT CONVERSATION (ADR-0050
// phase 0, issue 1602).
//
// The web composer sends no `participant_id` when several agents sit in a
// conversation and none is picked. ResolveCurrentAdhocTurn accepts 0 and
// matches the conversation's `dummy` participant, so the message used to be
// answered by the plain model while the agents never saw it. The start must
// refuse instead, and must keep serving the two cases that are not a
// misroute: no target with at most one agent, and an explicit dummy target.
func TestPostgresCurrentAdhocStartWithoutTargetRefusedWithSeveralAgents(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	repository, err := NewCurrentAgentStartRepository(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Conversation 2 of the seed: the user (20), the dummy (23), a toolkit
	// (24) and one agent (30).
	request := func(target int64, question string) agentexecutionapp.CurrentAdhocStartRequest {
		return agentexecutionapp.CurrentAdhocStartRequest{
			ProjectID: 1, ActorUserID: 11,
			ConversationUUID:    "10000000-0000-4000-8000-000000000032",
			TargetParticipantID: target,
			QuestionID:          question,
			UserInput:           "which release is red?",
			LLMSettings:         []byte(`{}`),
			TriggerOrigin:       executiondomain.TriggerOriginManual,
		}
	}

	// One agent: no target still means the plain model, as before.
	target, err := repository.ResolveCurrentAdhoc(t.Context(), request(0, "51000000-0000-4000-8000-000000001602"))
	if err != nil {
		t.Fatalf("one agent, no target: %v", err)
	}
	if target.TargetParticipantID != 23 {
		t.Fatalf("one agent, no target resolved participant %d, want the dummy 23", target.TargetParticipantID)
	}

	// A second agent joins.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta, meta) VALUES
    (31, 'd0000000-0000-4000-8000-000000001602', 'application', '{"id":31,"project_id":1}'::jsonb,
     '{"name":"second-agent","agent_type":"agent"}'::json);
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES
    (2, 31, '{"version_id":41,"variables":[],"agent_type":"agent"}'::jsonb, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}

	// Several agents, no target: refused, never routed to the model.
	_, err = repository.ResolveCurrentAdhoc(t.Context(), request(0, "51000000-0000-4000-8000-000000001603"))
	if !errors.Is(err, agentexecutionapp.ErrAgentTargetRequired) {
		t.Fatalf("several agents, no target: err=%v, want ErrAgentTargetRequired", err)
	}

	// Several agents, the dummy named explicitly: the person chose the model.
	target, err = repository.ResolveCurrentAdhoc(t.Context(), request(23, "51000000-0000-4000-8000-000000001604"))
	if err != nil {
		t.Fatalf("several agents, explicit model target: %v", err)
	}
	if target.TargetParticipantID != 23 {
		t.Fatalf("explicit model target resolved participant %d, want 23", target.TargetParticipantID)
	}
}
