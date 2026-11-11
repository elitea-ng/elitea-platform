package pipelinelimits_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func readContract(t *testing.T, name string, into any) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// The constants, the family table and the refusal texts are the shared
// contract's (libs/proto/contracts/pipeline-node-settings-v1.json).
func TestNodeSettingsMatchTheSharedContract(t *testing.T) {
	var contract struct {
		NodeErrorsKey string              `json:"node_errors_key"`
		Limits        map[string]int64    `json:"limits"`
		Families      map[string][]string `json:"families"`
		PauseFamilies []string            `json:"pause_families"`
		Refusals      map[string]string   `json:"refusals"`
	}
	readContract(t, "pipeline-node-settings-v1.json", &contract)
	want := map[string]int64{
		"min_node_tries":                pipelinelimits.MinNodeTries,
		"max_node_tries":                pipelinelimits.MaxNodeTries,
		"min_wait_between_tries_ms":     pipelinelimits.MinWaitBetweenTriesMs,
		"max_wait_between_tries_ms":     pipelinelimits.MaxWaitBetweenTriesMs,
		"default_wait_between_tries_ms": pipelinelimits.DefaultWaitBetweenTriesMs,
		"min_node_timeout_ms":           pipelinelimits.MinNodeTimeoutMs,
		"max_node_timeout_ms":           pipelinelimits.MaxNodeTimeoutMs,
		"max_node_errors":               pipelinelimits.MaxNodeErrors,
		"max_node_error_entry_bytes":    pipelinelimits.MaxNodeErrorEntryBytes,
		"max_node_error_object_bytes":   pipelinelimits.MaxNodeErrorObjectBytes,
		"max_executed_once_nodes":       pipelinelimits.MaxExecutedOnceNodes,
	}
	if !reflect.DeepEqual(contract.Limits, want) {
		t.Fatalf("limits = %v, contract = %v", want, contract.Limits)
	}
	if contract.NodeErrorsKey != pipelinelimits.NodeErrorsKey {
		t.Fatalf("node_errors key = %q, contract = %q", pipelinelimits.NodeErrorsKey, contract.NodeErrorsKey)
	}
	if !reflect.DeepEqual(pipelinelimits.SettingsFamilies(), contract.Families) {
		t.Fatalf("families = %v, contract = %v", pipelinelimits.SettingsFamilies(), contract.Families)
	}
	pauses := append([]string{}, contract.PauseFamilies...)
	sort.Strings(pauses)
	if !reflect.DeepEqual(pipelinelimits.PauseFamilies(), pauses) {
		t.Fatalf("pause families = %v, contract = %v", pipelinelimits.PauseFamilies(), pauses)
	}
	if !reflect.DeepEqual(pipelinelimits.NodeSettingsRefusalTexts, contract.Refusals) {
		t.Fatalf("refusal texts differ from the contract")
	}
}

// Every layer admits or refuses the shared vectors the same way, with the
// same readable text.
func TestNodeSettingsSharedAdmissionVectors(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name    string  `json:"name"`
			YAML    string  `json:"yaml"`
			Refusal *string `json:"refusal"`
			Node    *string `json:"node"`
		} `json:"cases"`
	}
	readContract(t, "pipeline-node-settings-v1-vectors.json", &vectors)
	if len(vectors.Cases) < 40 {
		t.Fatalf("only %d vectors", len(vectors.Cases))
	}
	for _, vector := range vectors.Cases {
		err := pipelinelimits.Check(vector.YAML)
		if vector.Refusal == nil {
			if err != nil {
				t.Errorf("%s: refused: %v", vector.Name, err)
			}
			continue
		}
		api := pipelinelimits.Refusal(err)
		if api == nil || api.Code != pipelinelimits.CodeNodeSettingsInvalid {
			t.Errorf("%s: got %v, want %s", vector.Name, err, *vector.Refusal)
			continue
		}
		text := pipelinelimits.NodeSettingsRefusalTexts[*vector.Refusal]
		want := text
		if vector.Node != nil {
			want = "Node " + *vector.Node + ": " + text
		}
		if api.Message != want || api.Status != 400 {
			t.Errorf("%s: %d %q, want 400 %q", vector.Name, api.Status, api.Message, want)
		}
	}
}

func TestNodeSettingsRefusalIsTypedAndBranchable(t *testing.T) {
	doc := "state:\n  out: str\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    output: [out]\n    settings: {retry_on_fail: true, max_tries: 6}\n    transition: END\n"
	for _, check := range []func(string) error{
		pipelinelimits.Check,
		func(doc string) error { return pipelinelimits.CheckStart(doc, nil) },
	} {
		err := check(doc)
		var api *apierr.APIError
		if !errors.As(err, &api) || api.Code != pipelinelimits.CodeNodeSettingsInvalid {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(api.Message, "Max tries must be between 2 and 5.") {
			t.Fatalf("message %q", api.Message)
		}
	}
	// A node id that is not a graph id is never echoed.
	odd := strings.Replace(doc, "id: a", "id: 'a b<script>'", 1)
	if api := pipelinelimits.Refusal(pipelinelimits.Check(odd)); api == nil || strings.Contains(api.Message, "script") {
		t.Fatalf("got %v", api)
	}
}

// A document without settings blocks is never refused by them, whatever its
// node_errors key (C3), and pays one map lookup per node.
func TestNodeSettingsLeaveDocumentsWithoutBlocksAlone(t *testing.T) {
	doc := "state:\n  node_errors: str\n  out: str\nentry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    input: [node_errors]\n    output: [out]\n    transition: END\n"
	if err := pipelinelimits.Check(doc); err != nil {
		t.Fatal(err)
	}
}
