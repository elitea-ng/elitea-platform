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
		strings.Replace(base, "state: {count: int}", "state: {count: int, __elitea_pipeline_result_trace_v1: dict}", 1),
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

func TestSavedCodeReservedStateKeyHoldsTheFixedParallelPrivateChannels(t *testing.T) {
	for _, key := range []string{
		"__elitea_parallel_resume_v1",
		"__elitea_parallel_agent_inputs_v1",
		"__elitea_parallel_llm_inputs_v1",
		"__elitea_executed_once_v1",
	} {
		if !savedCodeReservedStateKey(key) {
			t.Fatalf("%s is not reserved", key)
		}
	}
	reservedInState := "entry_point: run\nstate:\n  __elitea_executed_once_v1: dict\n  out: str\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    output: [out]\n"
	if _, err := OriginalSavedCodeInputPolicy(reservedInState, CodeDebugSHA256([]byte(reservedInState)), "run"); err == nil {
		t.Fatal("reserved __elitea_executed_once_v1 admitted in state")
	}
}

func TestSavedCodeInputPolicySynthesizesNodeErrorsForSettingsOnError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instructions string
		nodeID       string
		validInputs  []string
		badInputs    []string
	}{
		{
			name: "code on_error error_output all inputs and handler explicit node_errors",
			instructions: `entry_point: broken_code
state:
  out: str
  verdict: str
nodes:
  - id: broken_code
    type: code
    code: '1'
    output: [out]
    settings:
      on_error: error_output
      error_transition: handle_err
    transition: END
  - id: handle_err
    type: code
    code: '2'
    input: [node_errors]
    output: [verdict]
    transition: END
`,
			nodeID:      "broken_code",
			validInputs: []string{`{}`, `{"input":"hi","node_errors":[]}`, `{"input":"hi","node_errors":[{"node":"broken_code"}]}`},
			badInputs:   []string{`{"node_errors":"bad"}`, `{"node_errors":1}`},
		},
		{
			name: "sibling deterministic node on_error continue synthesizes node_errors for code",
			instructions: `entry_point: shape
state:
  out: str
nodes:
  - id: shape
    type: state_modifier
    template: x
    output: [out]
    settings:
      on_error: continue
    transition: run
  - id: run
    type: code
    code: '1'
    input: [node_errors]
    output: [out]
    transition: END
`,
			nodeID:      "run",
			validInputs: []string{`{}`, `{"node_errors":[]}`, `{"node_errors":[{"node":"shape","handled":"continue"}]}`},
			badInputs:   []string{`{"input":"unselected"}`, `{"node_errors":"bad"}`},
		},
		{
			name: "explicit selection excluding node_errors rejects node_errors",
			instructions: `entry_point: run
state:
  count: int
  out: str
nodes:
  - id: run
    type: code
    code: '1'
    input: [count]
    output: [out]
    settings:
      on_error: continue
    transition: END
`,
			nodeID:      "run",
			validInputs: []string{`{}`, `{"count":3}`},
			badInputs:   []string{`{"node_errors":[]}`},
		},
		{
			name: "explicit node_errors list with append reducer accepted",
			instructions: `entry_point: run
state:
  node_errors:
    type: list
    value: []
    reducer: append
  out: str
nodes:
  - id: run
    type: code
    code: '1'
    input: [node_errors]
    output: [out]
    settings:
      on_error: continue
    transition: END
`,
			nodeID:      "run",
			validInputs: []string{`{}`, `{"node_errors":[]}`},
			badInputs:   []string{`{"node_errors":{}}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := OriginalSavedCodeInputPolicy(tc.instructions, CodeDebugSHA256([]byte(tc.instructions)), tc.nodeID)
			if err != nil {
				t.Fatalf("OriginalSavedCodeInputPolicy(%s): %v", tc.nodeID, err)
			}
			for _, raw := range tc.validInputs {
				if err := policy.ValidateInput([]byte(raw)); err != nil {
					t.Fatalf("valid input %s rejected: %v", raw, err)
				}
			}
			for _, raw := range tc.badInputs {
				if policy.ValidateInput([]byte(raw)) == nil {
					t.Fatalf("invalid input %s admitted", raw)
				}
			}
		})
	}

	// Verify explicit handle_err node in case 1 too.
	case1 := `entry_point: broken_code
state:
  out: str
  verdict: str
nodes:
  - id: broken_code
    type: code
    code: '1'
    output: [out]
    settings:
      on_error: error_output
      error_transition: handle_err
    transition: END
  - id: handle_err
    type: code
    code: '2'
    input: [node_errors]
    output: [verdict]
    transition: END
`
	handlerPolicy, err := OriginalSavedCodeInputPolicy(case1, CodeDebugSHA256([]byte(case1)), "handle_err")
	if err != nil {
		t.Fatalf("handle_err policy: %v", err)
	}
	if err := handlerPolicy.ValidateInput([]byte(`{"node_errors":[{"node":"broken_code"}]}`)); err != nil {
		t.Fatalf("handle_err valid input rejected: %v", err)
	}
	if handlerPolicy.ValidateInput([]byte(`{"input":"not-selected"}`)) == nil {
		t.Fatal("handle_err widened beyond [node_errors]")
	}
}

func TestSavedCodeInputPolicyDoesNotSynthesizeNodeErrorsWithoutOnErrorAndChecksReducers(t *testing.T) {
	// C3 parity: retry_on_fail / empty settings / no settings never synthesize node_errors,
	// and a user's own node_errors key keeps working when no node continues/routes errors.
	for _, settingsBlock := range []string{
		"",
		"    settings: {}\n",
		"    settings:\n      retry_on_fail: true\n      on_error: stop\n",
	} {
		instructions := "entry_point: run\nstate:\n  node_errors: str\n  out: str\nnodes:\n  - id: run\n    type: code\n    code: '1'\n    output: [out]\n" + settingsBlock + "    transition: END\n"
		policy, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run")
		if err != nil {
			t.Fatalf("unexpected error without continue/error_output: %v", err)
		}
		if err := policy.ValidateInput([]byte(`{"node_errors":"user string"}`)); err != nil {
			t.Fatalf("user node_errors:str rejected: %v", err)
		}
		if policy.ValidateInput([]byte(`{"node_errors":[]}`)) == nil {
			t.Fatal("synthesized list admitted when on_error is stop/absent")
		}
	}

	noUserKey := "entry_point: run\nstate:\n  out: str\nnodes:\n  - id: run\n    type: code\n    code: '1'\n    output: [out]\n    settings:\n      retry_on_fail: true\n    transition: END\n"
	noUserPolicy, err := OriginalSavedCodeInputPolicy(noUserKey, CodeDebugSHA256([]byte(noUserKey)), "run")
	if err != nil {
		t.Fatal(err)
	}
	if noUserPolicy.ValidateInput([]byte(`{"node_errors":[]}`)) == nil {
		t.Fatal("node_errors admitted under retry_on_fail only")
	}

	// Decision 2: invalid user node_errors declaration when usesNodeErrors is true must fail closed.
	for _, badNodeErrors := range []string{
		"  node_errors: list\n",
		"  node_errors: str\n",
		"  node_errors:\n    type: dict\n    value: {}\n    reducer: merge\n",
	} {
		instructions := "entry_point: run\nstate:\n" + badNodeErrors + "  out: str\nnodes:\n  - id: run\n    type: code\n    code: '1'\n    output: [out]\n    settings:\n      on_error: continue\n    transition: END\n"
		if _, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
			t.Fatalf("invalid node_errors declaration accepted:\n%s", badNodeErrors)
		}
	}

	// Reducers parity: valid pairings pass, mismatched / unknown / built-in reducers fail closed.
	validReducers := `entry_point: run
state:
  items: {type: list, value: [], reducer: append}
  total: {type: int, value: 0, reducer: sum_int}
  bag: {type: dict, value: {}, reducer: merge}
  note: {type: str, value: "", reducer: overwrite}
  nilred: {type: str, value: "", reducer: null}
nodes:
  - id: run
    type: code
    code: '1'
`
	if _, err := OriginalSavedCodeInputPolicy(validReducers, CodeDebugSHA256([]byte(validReducers)), "run"); err != nil {
		t.Fatalf("valid reducers rejected: %v", err)
	}
	for _, badState := range []string{
		"  total: {type: int, reducer: append}\n",
		"  items: {type: list, reducer: sum_int}\n",
		"  items: {type: list, reducer: merge}\n",
		"  items: {type: list, reducer: unknown}\n",
		"  input: {type: str, reducer: overwrite}\n",
		"  messages: {type: list, reducer: append}\n",
		"  items: {type: list, reducer: {nested: true}}\n",
	} {
		instructions := "entry_point: run\nstate:\n" + badState + "nodes:\n  - id: run\n    type: code\n    code: '1'\n"
		if _, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
			t.Fatalf("invalid reducer admitted:\n%s", badState)
		}
	}

	// Malformed settings fail closed.
	for _, badSettings := range []string{
		"    settings: null\n",
		"    settings: [retry_on_fail]\n",
		"    settings:\n      on_error: ignore\n",
		"    settings:\n      on_error: [continue]\n",
	} {
		instructions := "entry_point: run\nstate:\n  out: str\nnodes:\n  - id: run\n    type: code\n    code: '1'\n    output: [out]\n" + badSettings
		if _, err := OriginalSavedCodeInputPolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
			t.Fatalf("malformed settings accepted:\n%s", badSettings)
		}
	}
}
