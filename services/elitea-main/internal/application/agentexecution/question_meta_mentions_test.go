package agentexecution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// Issue 1062: the mentions a message carries are stored on its question
// group's meta at admission, so transcripts render exact mentions.

func decodeQuestionMeta(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("question meta %s: %v", raw, err)
	}
	return meta
}

func TestCurrentQuestionMetaCarriesMentions(t *testing.T) {
	if got := string(currentQuestionMeta("", nil, false)); got != `{}` {
		t.Fatalf("an ordinary message's meta = %s, want {}", got)
	}
	meta := decodeQuestionMeta(t, currentQuestionMeta("31df012a-300d-4722-9be2-521d987c63a8", []int64{12, 99}, true))
	if meta["interaction_uuid"] != "31df012a-300d-4722-9be2-521d987c63a8" || meta["mentions_everyone"] != true {
		t.Fatalf("meta = %v", meta)
	}
	ids, _ := meta["mentioned_user_ids"].([]any)
	if len(ids) != 2 || ids[0] != float64(12) || ids[1] != float64(99) {
		t.Fatalf("mentioned_user_ids = %v, want [12 99] in the order tagged", meta["mentioned_user_ids"])
	}
}

func TestCurrentApplicationStartStoresMentionsOnTheQuestion(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
		InternalTools:  json.RawMessage(`[]`),
	}}
	admittedAt := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentApplicationStartRequest()
	request.MentionedUserIDs = []int64{12, 13}

	if _, err := service.StartCurrentApplication(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	if len(admissions.requests) != 1 || admissions.requests[0].CurrentTurn == nil {
		t.Fatalf("admissions = %+v", admissions.requests)
	}
	meta := decodeQuestionMeta(t, admissions.requests[0].CurrentTurn.QuestionMeta)
	ids, _ := meta["mentioned_user_ids"].([]any)
	if len(ids) != 2 || ids[0] != float64(12) || ids[1] != float64(13) || meta["interaction_uuid"] != request.InteractionUUID {
		t.Fatalf("question meta = %v, want the mentions and the interaction uuid", meta)
	}
	if _, present := meta["mentions_everyone"]; present {
		t.Fatalf("question meta = %v, want no mentions_everyone for a named list", meta)
	}
}

func TestCurrentAdhocStartStoresMentionsOnTheQuestion(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7,"max_tokens":1024}`),
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{}`),
	}}
	admittedAt := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentAdhocStartRequest()
	request.MentionsEveryone = true

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if len(admissions.requests) != 1 || admissions.requests[0].CurrentAdhocTurn == nil {
		t.Fatalf("admissions = %+v", admissions.requests)
	}
	meta := decodeQuestionMeta(t, admissions.requests[0].CurrentAdhocTurn.QuestionMeta)
	if meta["mentions_everyone"] != true {
		t.Fatalf("question meta = %v, want mentions_everyone", meta)
	}
}
