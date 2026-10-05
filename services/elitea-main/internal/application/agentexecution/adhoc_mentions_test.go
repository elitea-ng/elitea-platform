package agentexecution

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

// A model (ad-hoc) chat's `@` mentions reach the same notifier the agent
// path uses (client contract 1.1). Before, the route parsed `user_ids` and
// never passed it to StartCurrentAdhoc, so the mention was dropped silently.
func adhocMentionService(t *testing.T, created bool, writer *recordingMentionWriter) *CurrentApplicationStartService {
	t.Helper()
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved","model_project_id":7,"max_tokens":1024}`),
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{}`),
	}}
	admittedAt := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc", Created: created,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithMentionNotifications(writer)
}

func TestCurrentAdhocStartNotifiesMentionedProjectMembers(t *testing.T) {
	// The sender is user 11. Members are 11, 12 and 13; 99 is not a member.
	writer := &recordingMentionWriter{members: []int64{11, 12, 13}}
	service := adhocMentionService(t, true, writer)
	request := validCurrentAdhocStartRequest()
	request.MentionedUserIDs = []int64{12, 99, 11}

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if len(writer.rows) != 1 {
		t.Fatalf("rows = %+v, want one: the member (12), not the non-member (99) or the sender (11)", writer.rows)
	}
	row := writer.rows[0]
	if row.UserID != 12 || row.SenderUserID != 11 || row.ProjectID != 7 ||
		row.ConversationUUID != request.ConversationUUID || row.MessageID != request.QuestionID {
		t.Fatalf("row = %+v", row)
	}
}

func TestCurrentAdhocStartNotifiesEveryoneButTheSender(t *testing.T) {
	writer := &recordingMentionWriter{members: []int64{11, 12, 13}}
	service := adhocMentionService(t, true, writer)
	request := validCurrentAdhocStartRequest()
	request.MentionsEveryone = true

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	recipients := []int64{}
	for _, row := range writer.rows {
		recipients = append(recipients, row.UserID)
	}
	if fmt.Sprint(recipients) != "[12 13]" {
		t.Fatalf("@everyone notified %v, want [12 13]", recipients)
	}
}

// A replay of the same question_id is the same message: it must not ring the
// audience's bell a second time.
func TestCurrentAdhocStartReplayDoesNotNotifyAgain(t *testing.T) {
	writer := &recordingMentionWriter{members: []int64{11, 12}}
	service := adhocMentionService(t, false, writer)
	request := validCurrentAdhocStartRequest()
	request.MentionedUserIDs = []int64{12}

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	if writer.writes != 0 {
		t.Fatalf("a replayed admission wrote %d notification batches, want none", writer.writes)
	}
}
