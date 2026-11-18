package agentexecution

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// #2020: every refusal of a turn used to answer the same sentence, so a person
// whose chat failed could not say why. A refusal that carries a reason shows
// it; one that carries none (and a dependency's own error text, which is never
// a reason) keeps the generic sentence.
func TestWriteStartErrorNamesTheUnsupportedReason(t *testing.T) {
	const generic = "This agent turn requires the current execution path."
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "bare sentinel",
			err:  agentexecutionapp.ErrUnsupportedCurrentAgentStart,
			want: generic,
		},
		{
			name: "one reason",
			err:  agentexecutionapp.UnsupportedStartRefused(agentexecutionapp.RefusalAgentNotInConversation),
			want: "This agent turn cannot start: the agent that should answer is not part of this conversation.",
		},
		{
			name: "typed refusal shown; stage, unknown reason and dependency text stay out",
			err: fmt.Errorf("start: %w", errors.Join(
				agentexecutionapp.UnsupportedCurrentAgentStart("model resolution"),
				agentexecutionapp.UnsupportedCurrentAgentStart("internal stage: join x failed"),
				agentexecutionapp.UnsupportedStartRefused(agentexecutionapp.RefusalModelUnavailable),
				errors.New("dial tcp 10.0.0.1:5432: connection refused"),
			)),
			want: "This agent turn cannot start: " + agentexecutionapp.StartRefusalSentence(agentexecutionapp.RefusalModelUnavailable) + ".",
		},
		{
			name: "unknown free-form reason falls back to the generic sentence",
			err:  agentexecutionapp.UnsupportedCurrentAgentStart("join x on table y failed"),
			want: generic,
		},
		{
			name: "unregistered code is not shown",
			err:  agentexecutionapp.UnsupportedStartRefused(agentexecutionapp.StartRefusal("made_up")),
			want: generic,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeStartError(response, tc.err)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d", response.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["error"] != "unsupported_agent_execution" || body["message"] != tc.want {
				t.Fatalf("body = %v, want message %q", body, tc.want)
			}
		})
	}
}
