package agentexecution

import (
	"context"
	"errors"
	"testing"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The trigger origin reaches the job row the store writes (shared 0140), and
// it does not enter the idempotency digest: it describes the caller, and a
// digest change would turn every replay across a deployment into a conflict.
func TestAdmissionServiceCarriesTheTriggerOriginOutsideTheDigest(t *testing.T) {
	manualStore := successfulRecordingStore()
	scheduleStore := successfulRecordingStore()

	manual := validSubmitRequest()
	if _, err := testAdmissionService(t, manualStore).Submit(context.Background(), manual); err != nil {
		t.Fatalf("manual Submit() error = %v", err)
	}
	scheduled := validSubmitRequest()
	scheduled.TriggerOrigin = executiondomain.TriggerOriginSchedule
	if _, err := testAdmissionService(t, scheduleStore).Submit(context.Background(), scheduled); err != nil {
		t.Fatalf("scheduled Submit() error = %v", err)
	}

	if got := manualStore.admission.Record.Job.TriggerOrigin.Stored(); got != "manual" {
		t.Fatalf("manual origin stored as %q, want manual", got)
	}
	if got := scheduleStore.admission.Record.Job.TriggerOrigin; got != executiondomain.TriggerOriginSchedule {
		t.Fatalf("scheduled origin = %q, want schedule", got)
	}
	if manualStore.admission.Record.RequestDigest != scheduleStore.admission.Record.RequestDigest {
		t.Fatal("the trigger origin changed the idempotency digest")
	}
}

func TestAdmissionServiceRejectsAnUnknownTriggerOriginBeforeStorage(t *testing.T) {
	store := successfulRecordingStore()
	request := validSubmitRequest()
	request.TriggerOrigin = "cron"

	_, err := testAdmissionService(t, store).Submit(context.Background(), request)
	if !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("Submit() error = %v, want %v", err, ErrInvalidAgentAdmission)
	}
	if store.admission.Record.Job.ID != "" {
		t.Fatal("an unknown trigger origin reached durable storage")
	}
}

func TestCurrentApplicationStartRejectsAnUnknownTriggerOrigin(t *testing.T) {
	request := CurrentApplicationStartRequest{
		ProjectID: 7, ActorUserID: 11, TargetParticipantID: 21,
		ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:       "1f0b8d8e-62a5-4d1a-9b0e-5e8b8c1c2d3e",
		UserInput:        "hello",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("baseline request Validate() = %v", err)
	}
	request.TriggerOrigin = executiondomain.TriggerOriginWebhook
	if err := request.Validate(); err != nil {
		t.Fatalf("webhook origin Validate() = %v", err)
	}
	request.TriggerOrigin = "cron"
	if !errors.Is(request.Validate(), ErrInvalidCurrentAgentStart) {
		t.Fatal("an unknown trigger origin passed Validate")
	}
}
