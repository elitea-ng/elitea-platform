package material

import (
	"strings"
	"testing"
)

func liftedBlock(t *testing.T, body string) map[string]any {
	t.Helper()
	env, err := Read(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	block := map[string]any{"api_key": "minted"}
	if err := env.LiftToolLLMSettings(block); err != nil {
		t.Fatal(err)
	}
	return block
}

func TestToolkitReasoningEffortIsTheDefault(t *testing.T) {
	block := liftedBlock(t, `{"configuration":{"parameters":{"llm_settings":{"reasoning_effort":"none","api_key":"evil"}}},"parameters":{}}`)
	if block["reasoning_effort"] != "none" {
		t.Fatalf("toolkit default not used: %v", block)
	}
	if block["api_key"] != "minted" {
		t.Fatalf("toolkit block injected a credential: %v", block)
	}
}

func TestCallerReasoningEffortBeatsToolkit(t *testing.T) {
	block := liftedBlock(t, `{"configuration":{"parameters":{"llm_settings":{"reasoning_effort":"none"}}},"parameters":{"llm_settings":{"reasoning_effort":"high","api_key":"x"}}}`)
	if block["reasoning_effort"] != "high" || block["api_key"] != "minted" {
		t.Fatalf("got %v", block)
	}
}

func TestNoReasoningEffortAnywhere(t *testing.T) {
	block := liftedBlock(t, `{"configuration":{"parameters":{"llm_settings":{"reasoning_effort":5}}},"parameters":{}}`)
	if _, ok := block["reasoning_effort"]; ok {
		t.Fatalf("got %v", block)
	}
}

func TestFlatToolkitReasoningEffortWinsOverNested(t *testing.T) {
	block := liftedBlock(t, `{"configuration":{"parameters":{"reasoning_effort":"low","llm_settings":{"reasoning_effort":"high"}}},"parameters":{}}`)
	if block["reasoning_effort"] != "low" {
		t.Fatalf("got %v", block)
	}
}

func TestUnacceptedReasoningEffortIgnored(t *testing.T) {
	for _, value := range []string{`""`, `"extreme"`, `"minimal"`, `7`} {
		block := liftedBlock(t, `{"configuration":{"parameters":{"reasoning_effort":`+value+`}},"parameters":{}}`)
		if _, ok := block["reasoning_effort"]; ok {
			t.Fatalf("%s accepted: %v", value, block)
		}
	}
}
