package policy

import (
	"testing"
	"time"
)

func guardrailRow(name string, scope map[string]any, stage string) Row {
	data := map[string]any{"guardrail": map[string]any{
		"stage": stage, "detectors": []any{map[string]any{"type": "secrets", "action": "mask"}},
	}}
	if scope != nil {
		data["scope"] = scope
	}
	return Row{ID: name, Type: TypeGuardrail, Name: name, Data: data, Enabled: true}
}

func TestGuardrailRowsAllApplyFromWidestToNarrowest(t *testing.T) {
	snap := Compile([]Row{
		guardrailRow("project", map[string]any{"project_ids": []any{7.0}}, "output"),
		guardrailRow("platform", nil, "output"),
		guardrailRow("other-project", map[string]any{"project_ids": []any{8.0}}, "output"),
		guardrailRow("input", nil, "input"),
		guardrailRow("agent", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}}, "output"),
	}, time.Now())
	if len(snap.Rejected) != 0 {
		t.Fatalf("rejected: %+v", snap.Rejected)
	}
	if !snap.HasAgentGuardrails() {
		t.Fatal("HasAgentGuardrails = false")
	}
	rows := snap.Guardrails(Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}, "output", TrafficGeneration)
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if len(names) != 3 || names[0] != "platform" || names[1] != "project" || names[2] != "agent" {
		t.Fatalf("rows = %v", names)
	}
	if got := snap.Guardrails(Subject{ProjectID: 7}, "output", TrafficEmbedding); len(got) != 0 {
		t.Fatalf("an output row applied to embeddings: %d", len(got))
	}
	if got := snap.Guardrails(Subject{ProjectID: 7}, "input", TrafficEmbedding); len(got) != 0 {
		t.Fatalf("an input row without apply_to_embeddings applied to embeddings: %d", len(got))
	}
}

func TestGuardrailRowRejections(t *testing.T) {
	snap := Compile([]Row{
		guardrailRow("users", map[string]any{"user_ids": []any{1.0}}, "output"),
		guardrailRow("agents-two-projects", map[string]any{"project_ids": []any{1.0, 2.0}, "agent_ids": []any{3.0}}, "output"),
		{ID: "bad", Type: TypeGuardrail, Name: "bad", Data: map[string]any{"guardrail": map[string]any{"stage": "middle"}}, Enabled: true},
	}, time.Now())
	if len(snap.Rejected) != 3 {
		t.Fatalf("rejected = %+v", snap.Rejected)
	}
	if snap.HasGuardrails() {
		t.Fatal("a rejected row loaded")
	}
}
