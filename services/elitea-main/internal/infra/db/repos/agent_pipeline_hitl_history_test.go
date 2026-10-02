package repos

import (
	"encoding/json"
	"testing"
)

func TestDirectPipelineHITLReviewScope(t *testing.T) {
	for _, test := range []struct {
		name      string
		agentType string
		metadata  string
		mutation  string
		want      bool
	}{
		{name: "direct participant", agentType: "pipeline", metadata: `{}`, want: true},
		{name: "pipeline test chat", agentType: "pipeline", metadata: `{"test":true}`, want: true},
		{name: "agent participant", agentType: "agent", metadata: `{}`},
		{name: "nested interrupt", agentType: "pipeline", metadata: `{}`, mutation: `{"parent_agent_call_id":"call-1"}`},
		{name: "nested response", agentType: "pipeline", metadata: `{"parent_agent_name":"parent"}`},
		{name: "nested lineage", agentType: "pipeline", metadata: `{"metadata":{"parent_agent_path":[{"name":"parent","call_id":"call-1"}]}}`},
		{name: "parallel child", agentType: "pipeline", metadata: `{"resume_strategy":"aggregate_child"}`},
		{name: "supervised child", agentType: "pipeline", metadata: `{"resume_strategy":"supervised_child"}`},
		{name: "multiple interrupts", agentType: "pipeline", metadata: `{}`, mutation: "multiple"},
		{name: "tool guard", agentType: "pipeline", metadata: `{}`, mutation: `{"interaction_type":"tool_approval"}`},
		{name: "unknown contract", agentType: "pipeline", metadata: `{}`, mutation: `{"history_contract_version":2}`},
		{name: "missing review", agentType: "pipeline", metadata: `{}`, mutation: `{"message":null}`},
		{name: "malformed metadata", agentType: "pipeline", metadata: `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var interrupt map[string]any
			if err := json.Unmarshal([]byte(`{"interaction_type":"pipeline_hitl_node","history_contract_version":1,"interrupt_id":"review-1","node_name":"review","message":"Here is the joke for review:\n\nA joke."}`), &interrupt); err != nil {
				t.Fatal(err)
			}
			if test.mutation != "" && test.mutation != "multiple" {
				if err := json.Unmarshal([]byte(test.mutation), &interrupt); err != nil {
					t.Fatal(err)
				}
			}
			interrupts := []map[string]any{interrupt}
			if test.mutation == "multiple" {
				interrupts = append(interrupts, interrupt)
			}
			review := directPipelineHITLReview(test.agentType, []byte(test.metadata), interrupts)
			if (review != nil) != test.want {
				t.Fatalf("review eligibility = %v, want %v", review != nil, test.want)
			}
			if review != nil && (review.Message != interrupt["message"] || review.InterruptID != "review-1" || review.NodeName != "review") {
				t.Fatalf("review content or identity changed: %+v", review)
			}
		})
	}
}
