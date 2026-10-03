package agentexecution

// An unattended pipeline start may carry no text (regression finding UI-PD-3).
//
// A webhook that only says "something happened" and a schedule with no input
// both reach StartCurrentApplication with UserInput "". Validate refused that,
// pipelinetriggers wrapped the refusal, and the sender read an opaque 503
// "the pipeline run could not be started". AllowEmptyUserInput is the opt-in
// those two callers set; a typed chat turn does not set it and keeps refusing
// an empty message.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

func TestCurrentApplicationStartRequestEmptyInputNeedsTheUnattendedOptIn(t *testing.T) {
	for _, test := range []struct {
		name       string
		input      string
		allowEmpty bool
		wantValid  bool
	}{
		{"typed turn with text", "hello", false, true},
		{"typed turn with no text is refused", "", false, false},
		{"unattended start with no text", "", true, true},
		{"unattended start with text", "push to main", true, true},
		{"the opt-in admits nothing else: NUL", "a\x00b", true, false},
		{"the opt-in admits nothing else: invalid UTF-8", string([]byte{0xff, 0xfe}), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validCurrentApplicationStartRequest()
			request.UserInput = test.input
			request.AllowEmptyUserInput = test.allowEmpty
			if err := request.Validate(); (err == nil) != test.wantValid {
				t.Fatalf("Validate() = %v, want valid=%v", err, test.wantValid)
			}
		})
	}
}

func TestCurrentApplicationStartAdmitsAnUnattendedRunWithNoInput(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"pipeline","instructions":"state: {}","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}

	request := validCurrentApplicationStartRequest()
	request.UserInput = ""
	if _, err := service.StartCurrentApplication(context.Background(), request); !errors.Is(err, ErrInvalidCurrentAgentStart) {
		t.Fatalf("a typed turn with no text: error = %v, want ErrInvalidCurrentAgentStart", err)
	}

	request.AllowEmptyUserInput = true
	outcome, err := service.StartCurrentApplication(context.Background(), request)
	if err != nil {
		t.Fatalf("an unattended start with no input: %v", err)
	}
	if outcome.ExecutionID != "execution-1" || len(admissions.requests) != 1 {
		t.Fatalf("outcome = %+v, admissions = %d", outcome, len(admissions.requests))
	}
	turn := admissions.requests[0].CurrentTurn
	// The repository re-validates the turn inside its transaction. The flag
	// must travel with it, or the empty input is refused one layer down.
	if turn == nil || !turn.AllowEmptyUserInput || turn.UserInput != "" || turn.Validate() != nil {
		t.Fatalf("current turn = %+v", turn)
	}
	// The model is not handed an empty message: skill projection substitutes
	// "continue", which is how a graph started from its entry node proceeds.
	if got := admissions.requests[0].Input.GetUserInput(); !bytes.Equal(got, []byte(`"continue"`)) {
		t.Fatalf("runtime user_input = %s, want \"continue\"", got)
	}
}
