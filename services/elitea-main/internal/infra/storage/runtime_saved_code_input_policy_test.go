package storage

import (
	"os"
	"strings"
	"testing"
)

func savedInputPolicyFixture(t *testing.T, selection string) SavedCodeInputPolicy {
	t.Helper()
	instructions := "entry_point: run\nstate:\n input: string\n count: number\n ratio: float\n flag: bool\n items: list\n record: {type: dict, value: {}}\n messages: list\n '10': str\nnodes:\n - id: run\n   type: code\n   code: '7'\n" + selection
	policy, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run")
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestSavedCodeInputPolicyPreservesAllAndPresentSelectionSemantics(t *testing.T) {
	for _, selection := range []string{"", "   input: []\n", "   input: [messages]\n"} {
		policy := savedInputPolicyFixture(t, selection)
		for _, raw := range []string{`{}`, `{"input":"hello"}`, `{"count":18446744073709551615,"ratio":2,"flag":true,"items":[null,7,{"unknown":[false,"opaque"]}],"record":{"nested":null},"10":"quoted"}`} {
			if err := policy.ValidateInput([]byte(raw)); err != nil {
				t.Fatal("valid all-user subset refused", selection, err)
			}
		}
		for _, raw := range []string{`{"messages":[]}`, `{"undeclared":"no"}`, `{"context_info":{}}`, `{"__elitea_application_task_v1":{}}`, `{"input":null}`, `{"count":1.0}`, `{"count":18446744073709551616}`, `{"count":-9223372036854775809}`, `{"ratio":"2"}`, `{"ratio":1e9999}`, `{"flag":1}`, `{"items":{}}`, `{"record":[]}`, `{"count":1,"count":2}`} {
			if err := policy.ValidateInput([]byte(raw)); err == nil {
				t.Fatal("invalid or unapproved input accepted", raw)
			}
		}
	}
	for _, selection := range []string{"   input: [count]\n", "   input: [messages, count]\n"} {
		policy := savedInputPolicyFixture(t, selection)
		for _, raw := range []string{`{}`, `{"count":7}`} {
			if err := policy.ValidateInput([]byte(raw)); err != nil {
				t.Fatal("missing or present explicit selection refused", err)
			}
		}
		for _, raw := range []string{`{"input":"not selected"}`, `{"messages":[]}`, `{"items":[]}`} {
			if policy.ValidateInput([]byte(raw)) == nil {
				t.Fatal("selection was widened", raw)
			}
		}
	}
}

func TestSavedCodeInputPolicyRequiresExactTypedOwningDeclarations(t *testing.T) {
	base := "entry_point: run\nstate: {count: int}\nnodes: [{id: run, type: code, code: '7', input: [count]}]\n"
	for _, instructions := range []string{
		strings.Replace(base, "input: [count]", "input: [count, count]", 1),
		strings.Replace(base, "input: [count]", "input: [undeclared]", 1),
		strings.Replace(base, "state: {count: int}", "state: null", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: mystery}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: {type: null}}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: {type: int, vendor: true}}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: int, input: int}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: int, messages: dict}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: int, result: int}", 1),
		strings.Replace(base, "state: {count: int}", "state: {count: int, __elitea_pipeline_terminal_json_producers_v1: dict}", 1),
	} {
		if _, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
			t.Fatal("malformed saved approval surface accepted")
		}
	}
	if _, err := OriginalSavedCodeInputPolicy(base, strings.Repeat("f", 64), "run"); err == nil {
		t.Fatal("approval surface came from another source digest")
	}
	if (SavedCodeInputPolicy{}).ValidateInput([]byte(`{}`)) == nil {
		t.Fatal("zero policy created approval")
	}
	policy := savedInputPolicyFixture(t, "")
	if policy.ValidateInput([]byte(`{"input":"`+strings.Repeat("x", 512*1024)+`"}`)) == nil {
		t.Fatal("existing selected-input bound expanded")
	}
}

func TestSavedCodeInputPolicyUsesTheSameBoundedAnchorFixture(t *testing.T) {
	fixture, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/fixtures/code_debug_anchored.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := OriginalSavedCodeInputPolicy(string(fixture), CodeDebugSHA256(fixture), "run")
	if err != nil || policy.ValidateInput([]byte(`{"input":"hello"}`)) != nil || policy.ValidateInput([]byte(`{"answer":"unselected"}`)) == nil {
		t.Fatal("anchored owning input policy changed", err)
	}
}
