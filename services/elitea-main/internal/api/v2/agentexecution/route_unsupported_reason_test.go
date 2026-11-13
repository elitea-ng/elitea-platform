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
			err:  agentexecutionapp.UnsupportedCurrentAgentStart("the agent is not part of this conversation"),
			want: "This agent turn cannot start: the agent is not part of this conversation.",
		},
		{
			name: "stage and cause, dependency text stays out",
			err: fmt.Errorf("start: %w", errors.Join(
				agentexecutionapp.UnsupportedCurrentAgentStart("model resolution"),
				agentexecutionapp.UnsupportedCurrentAgentStart("the requested model is not in the project's catalog"),
				errors.New("dial tcp 10.0.0.1:5432: connection refused"),
			)),
			want: "This agent turn cannot start: model resolution: the requested model is not in the project's catalog.",
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
