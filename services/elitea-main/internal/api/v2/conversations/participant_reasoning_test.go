package conversations

import (
	"context"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

type fakeParticipantModels configurationapp.ModelReasoningInfo

func (f fakeParticipantModels) ModelReasoning(context.Context, string, string) (configurationapp.ModelReasoningInfo, error) {
	return configurationapp.ModelReasoningInfo(f), nil
}

// ADR-0038, one rule on every surface: a chat participant's level on a model
// that cannot think is refused at save, a non-reasoning model included; off on
// a model that always thinks too. A non-reasoning model gets no auto default.
func TestParticipantReasoningTakesTheOneSaveRule(t *testing.T) {
	plain := &Handler{reasoningModels: fakeParticipantModels{Found: true, Control: "fixed_off"}}
	if got := plain.refuseParticipantReasoning(context.Background(), "1", "gpt-4o",
		map[string]any{"reasoning_effort": "high"}); !strings.Contains(got, "this model cannot think") {
		t.Fatalf("a level on a non-reasoning model: %q", got)
	}
	settings := map[string]any{}
	if got := plain.refuseParticipantReasoning(context.Background(), "1", "gpt-4o", settings); got != "" || settings["reasoning_effort"] != nil {
		t.Fatalf("no setting on a non-reasoning model: %q %v", got, settings)
	}
	always := &Handler{reasoningModels: fakeParticipantModels{Found: true, SupportsReasoning: true, Control: "fixed_on"}}
	if got := always.refuseParticipantReasoning(context.Background(), "1", "qwq",
		map[string]any{"reasoning_effort": "off"}); !strings.Contains(got, "this model always thinks") {
		t.Fatalf("off on fixed_on: %q", got)
	}
	unknown := &Handler{reasoningModels: fakeParticipantModels{}}
	if got := unknown.refuseParticipantReasoning(context.Background(), "1", "x",
		map[string]any{"reasoning_effort": "high"}); got != "" {
		t.Fatalf("a model the catalogue does not know is not judged: %q", got)
	}
}
