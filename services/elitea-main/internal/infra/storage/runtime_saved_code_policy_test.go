package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSavedCodeDeclarationReconstructionPreservesFrozenSemantics(t *testing.T) {
	instructions := `entry_point: run
state: {input: str, result: str}
nodes:
 - id: run
   type: code
   language: rust
   code: {type: variable, value: input}
   input: [input]
   output: [result]
   debug: false
   recovery: {on_failure: {route: handler}}
   failure_handler: null
`
	policies, err := SavedCodePolicies(instructions)
	if err != nil {
		t.Fatal(err)
	}
	config := `{"id":"run","type":"code","language":"rust","code":{"type":"variable","value":"input"},"input":["input"],"output":["result"],"structured_output":false,"debug":false,"transition":null}`
	admitted, err := MatchOriginalSavedCodeConfiguration(policies["run"], config)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), policies["run"]...)
	reconstructed, err := ReadSavedCodeDeclaration(policies["run"], admitted.ConfigurationDigest)
	if err != nil || !reflect.DeepEqual(admitted, reconstructed) {
		t.Fatal("immutable declaration reconstruction changed semantics", err)
	}
	if reconstructed.Debug || string(reconstructed.FailureHandler) != "null" || len(reconstructed.Recovery) == 0 || !bytes.Equal(original, policies["run"]) {
		t.Fatal("debug/recovery/null or source policy changed")
	}
	reconstructed.Source[0] = 'x'
	again, err := ReadSavedCodeDeclaration(policies["run"], admitted.ConfigurationDigest)
	if err != nil || !json.Valid(again.Source) || !bytes.Equal(original, policies["run"]) {
		t.Fatal("result aliases stored policy")
	}
}

func TestSavedCodeAnchorsMatchOwningRustFixtureSemantics(t *testing.T) {
	fixture, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/fixtures/code_debug_anchored.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := OriginalRootSavedCodePolicy(string(fixture), CodeDebugSHA256(fixture), "run")
	if err != nil {
		t.Fatal("valid saved anchors were refused", err)
	}
	// The Rust test parses these same fixture bytes through PipelineDefinition
	// and then the owning Code serde parser; both compare this emitted contract.
	configuration := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"input['input']\n"},"input":["input"],"output":["answer"],"structured_output":false,"debug":true,"transition":"END"}`
	declaration, err := MatchOriginalSavedCodeConfiguration(policy, configuration)
	if err != nil || !declaration.Debug || !reflect.DeepEqual(declaration.Input, []string{"input"}) || !reflect.DeepEqual(declaration.Output, []string{"answer"}) {
		t.Fatal("anchored saved declaration changed semantics", err)
	}
	var source struct{ Value string }
	if json.Unmarshal(declaration.Source, &source) != nil || source.Value != "input['input']\n" {
		t.Fatal("resolved anchored source changed")
	}
	if _, err := OriginalRootSavedCodePolicy(string(fixture), CodeDebugSHA256(append(fixture, '\n')), "run"); err == nil {
		t.Fatal("valid aliases bypassed exact original YAML identity")
	}
}

func TestSavedCodeAliasResolutionRejectsCyclesDuplicatesAndMultipleDocuments(t *testing.T) {
	base := "entry_point: run\nnodes:\n - id: run\n   type: code\n   code: '7'\n"
	for name, instructions := range map[string]string{
		"cycle":              "entry_point: run\nloop: &loop [*loop]\nnodes: [{id: run, type: code, code: '7'}]\n",
		"duplicate":          strings.Replace(base, "   code: '7'", "   code: &source '7'\n   code: *source", 1),
		"duplicate-alias-id": base + " - id: run\n   type: code\n   code: '8'\n",
		"multi-document":     base + "---\nentry_point: other\nnodes: []\n",
		"unknown-alias":      strings.Replace(base, "code: '7'", "code: *absent", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := OriginalSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
				t.Fatal("invalid alias document admitted")
			}
		})
	}
}

func TestSavedCodeAliasesCannotApplyImplicitMergeAuthority(t *testing.T) {
	instructions := "entry_point: run\ndefaults: &defaults {debug: true}\nnodes:\n - id: run\n   type: code\n   code: '7'\n   <<: *defaults\n"
	policy, err := OriginalSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(policy, &fields) != nil || fields["debug"] != false || fields["<<"] == nil {
		t.Fatal("literal merge field was applied or discarded")
	}
	configuration := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"7"},"input":[],"output":[],"structured_output":false,"debug":true,"transition":null}`
	if _, err := MatchOriginalSavedCodeConfiguration(policy, configuration); err == nil {
		t.Fatal("merge inferred debug authority absent from owning Rust configuration")
	}
}

func TestSavedCodeSelectedAliasExpansionIsBoundedBeforeMaterialization(t *testing.T) {
	for name, value := range map[string]string{
		"bytes": strings.Repeat("*value,", 4097),
		"nodes": strings.Repeat("*value,", 33000),
	} {
		t.Run(name, func(t *testing.T) {
			literal := "x"
			if name == "bytes" {
				literal = strings.Repeat("x", 512)
			}
			instructions := "entry_point: run\nliteral: &value '" + literal + "'\nexpanded: &expanded [" + value + "]\nnodes:\n - id: run\n   type: code\n   code: '7'\n   vendor: *expanded\n"
			// Raw input and unique document graph are bounded. The selected
			// declaration would expand over its byte/node budget; never Decode it.
			if len(instructions) > 1024*1024 {
				t.Fatal("test exceeded raw input bound")
			}
			nodes, err := savedCodeNodes(instructions)
			if err != nil {
				t.Fatal("bounded unique alias graph refused", err)
			}
			if _, _, err := decodeSavedCodePolicy(nodes.Content[0]); err == nil {
				t.Fatal("selected expansion exceeded budget")
			}
		})
	}
	var nested yaml.Node
	if yaml.NewDecoder(strings.NewReader("&deep ["+strings.Repeat("[", 65)+"0"+strings.Repeat("]", 65)+"]")).Decode(&nested) != nil {
		t.Fatal("depth fixture was not valid YAML")
	}
	if validateDebugYAML(nested.Content[0], 0, new(int)) == nil {
		t.Fatal("nested declaration exceeded depth limit")
	}
}

func TestSavedCodeLookupDoesNotExpandAnUnselectedDeclaration(t *testing.T) {
	instructions := "entry_point: run\nliteral: &value '" + strings.Repeat("x", 512) + "'\nexpanded: &expanded [" + strings.Repeat("*value,", 4097) + "]\nnodes:\n - id: run\n   type: code\n   code: '7'\n - id: other\n   type: code\n   code: '8'\n   vendor: *expanded\n"
	if _, err := OriginalSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err != nil {
		t.Fatal("unselected alias expansion was materialized", err)
	}
	if _, err := OriginalSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "other"); err == nil {
		t.Fatal("selected excessive expansion admitted")
	}
}

func TestSavedCodeDeclarationReconstructionRefusesMalformedPolicies(t *testing.T) {
	for _, policy := range []string{`null`, `[]`, `{"id":"run","type":"code","language":"rust","code":{"type":"fixed","value":"ok","grant":"secret"}}`, `{"id":"run","id":"other","type":"code","language":"rust","code":{"type":"fixed","value":"ok"}}`, `{"id":"run","type":"code","language":"unknown","code":{"type":"fixed","value":"ok"}}`} {
		if _, err := ReadSavedCodeDeclaration([]byte(policy), [32]byte{1}); err == nil {
			t.Fatal("malformed policy reconstructed", policy)
		}
	}
}

func TestRootSavedCodeDeclarationExcludesMapAndParallelOwnedNodes(t *testing.T) {
	base := "entry_point: root\nnodes:\n - id: run\n   type: code\n   code: '7'\n - id: root\n   type: llm\n"
	for name, owned := range map[string]string{
		"map":           " - id: map_1\n   type: map\n   worker: run\n",
		"parallel":      " - id: parallel_1\n   type: parallel\n   branches: [{id: stable_branch, node: run}]\n",
		"legacy-branch": " - id: parallel_1\n   type: parallel\n   branches: [run]\n",
		"duplicate-id":  " - id: run\n   type: llm\n",
	} {
		t.Run(name, func(t *testing.T) {
			instructions := base + owned
			// Declaration membership alone intentionally does not mint ownership authority.
			if _, err := OriginalSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err != nil {
				t.Fatal(err)
			}
			if _, err := OriginalRootSavedCodePolicy(instructions, CodeDebugSHA256([]byte(instructions)), "run"); err == nil {
				t.Fatal("root read accepted owned or ambiguous Code")
			}
		})
	}
	if _, err := OriginalRootSavedCodePolicy(base, CodeDebugSHA256([]byte(base)), "run"); err != nil {
		t.Fatal("unowned root declaration refused", err)
	}
	changed := base + " - id: worker\n   type: agent\n - id: map_1\n   type: map\n   worker: worker\n - id: parallel_1\n   type: parallel\n   branches: [{id: run, node: worker}]\n"
	if _, err := OriginalRootSavedCodePolicy(changed, CodeDebugSHA256([]byte(changed)), "run"); err != nil {
		t.Fatal("stable branch id was confused with owned node reference", err)
	}
}

func TestSavedCodePlatformClientMatchesSkipFalseProducerAndRejectsNulls(t *testing.T) {
	base := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"7"},"input":[],"output":[],"structured_output":false,"debug":false,"transition":null}`
	for _, selection := range []string{"", "false", "true"} {
		policy := base
		actual := base
		if selection != "" {
			policy = base[:len(base)-1] + `,"platform_client":` + selection + `}`
		}
		if selection == "true" {
			actual = policy
		}
		declaration, err := MatchOriginalSavedCodeConfiguration([]byte(policy), actual)
		if err != nil || declaration.PlatformClient != (selection == "true") {
			t.Fatal("skip-false policy mismatch", selection, err)
		}
	}
	for _, field := range []string{"platform_client", "input", "output", "debug", "structured_output"} {
		var value map[string]any
		if err := json.Unmarshal([]byte(base), &value); err != nil {
			t.Fatal(err)
		}
		value[field] = nil
		policy, _ := json.Marshal(value)
		if _, err := MatchOriginalSavedCodeConfiguration(policy, string(policy)); err == nil {
			t.Fatal("null typed declaration accepted", field)
		}
	}
	policy := base[:len(base)-1] + `,"platform_client":false}`
	if _, err := MatchOriginalSavedCodeConfiguration([]byte(policy), policy); err == nil {
		t.Fatal("attested serialization did not follow actual skip_false producer")
	}
}

func TestSavedCodeMatchingJSONCannotInventUnknownTypedFieldsOrExpandSourceBounds(t *testing.T) {
	base := `{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"7"},"input":[],"output":[],"structured_output":false,"debug":false,"transition":null}`
	unknown := base[:len(base)-1] + `,"vendor":null}`
	if _, err := MatchOriginalSavedCodeConfiguration([]byte(unknown), unknown); err == nil {
		t.Fatal("matching arbitrary JSON invented typed Code authority")
	}
	var fields map[string]any
	if json.Unmarshal([]byte(base), &fields) != nil {
		t.Fatal("invalid fixture")
	}
	fields["code"].(map[string]any)["value"] = strings.Repeat("x", 256*1024+1)
	oversized, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MatchOriginalSavedCodeConfiguration(oversized, string(oversized)); err == nil {
		t.Fatal("alias-compatible policy expanded existing source bounds")
	}
}
