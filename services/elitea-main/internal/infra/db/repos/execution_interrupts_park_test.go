package repos

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// A parked terminal frame is decoded strictly; a malformed one is a typed
// refusal of the frame, never a silent park. A frame without fanout_parked_v1
// keeps today's root pause path.
func TestDecodeCurrentAgentFanoutParked(t *testing.T) {
	key := strings.Repeat("ab", 32)
	valid := `{"decision_revision_seen":2,"pending":[{"interrupt_id":"ask:1","interrupt_key":"` + key + `"}],"schema":"elitea.pipeline.fanout-parked.v1"}`
	metadata := func(parked string, extra string) json.RawMessage {
		return json.RawMessage(`{"thread_id":"thread-1"` + extra + `,"fanout_parked_v1":` + parked + `}`)
	}
	content := json.RawMessage(`"Parallel branches paused."`)

	parked, isParked, err := decodeCurrentAgentFanoutParked(content, metadata(valid, ""))
	if err != nil || !isParked || parked.ThreadID != "thread-1" || parked.Parked.DecisionRevisionSeen != 2 ||
		len(parked.Parked.Pending) != 1 || string(parked.Parked.Canonical) != valid {
		t.Fatalf("valid park: %+v %v %v", parked, isParked, err)
	}
	if _, isParked, err := decodeCurrentAgentFanoutParked(content, json.RawMessage(`{"thread_id":"t","hitl_interrupts":[]}`)); isParked || err != nil {
		t.Fatalf("root pause taken for a park: %v %v", isParked, err)
	}
	refused := map[string][2]json.RawMessage{
		"unknown member":       {content, metadata(`{"decision_revision_seen":2,"extra":1,"pending":[{"interrupt_id":"ask:1","interrupt_key":"`+key+`"}],"schema":"elitea.pipeline.fanout-parked.v1"}`, "")},
		"nothing pending":      {content, metadata(`{"decision_revision_seen":2,"pending":[],"schema":"elitea.pipeline.fanout-parked.v1"}`, "")},
		"not an object":        {content, metadata(`"parked"`, "")},
		"with root cards":      {content, metadata(valid, `,"hitl_interrupts":[{"interrupt_id":"x"}]`)},
		"with authorization":   {content, metadata(valid, `,"authorization_requests":[]`)},
		"with legacy parallel": {content, metadata(valid, `,"parallel_pause_v1":{}`)},
		"no thread":            {content, json.RawMessage(`{"fanout_parked_v1":` + valid + `}`)},
		"empty content":        {json.RawMessage(`""`), metadata(valid, "")},
		"null content":         {json.RawMessage(`null`), metadata(valid, "")},
	}
	for name, frame := range refused {
		_, isParked, err := decodeCurrentAgentFanoutParked(frame[0], frame[1])
		if !isParked || !errors.Is(err, outputapp.ErrAgentExecutionResultMismatch) || !errors.Is(err, domain.ErrInvalidParked) {
			t.Errorf("%s: parked=%v err=%v", name, isParked, err)
		}
	}
}
