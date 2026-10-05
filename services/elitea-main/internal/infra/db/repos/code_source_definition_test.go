package repos

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"google.golang.org/protobuf/proto"
)

func TestCodeDefinitionSourceAdapterConsumesProtectedCapturedRoot(t *testing.T) {
	fixture := newOriginalCodeFixture(t)
	for _, name := range []string{"captured source", "missing protected reference", "editable source pin", "different actor", "missing row", "different node", "wrong thread", "wrong purpose", "changed captured bytes", "unregistered child"} {
		t.Run(name, func(t *testing.T) {
			access := fixture.access
			thread := fixture.visit.Thread
			node := fixture.visit.Node
			purpose := "code_recovery"
			rawScope := json.RawMessage("null")
			rows := []scriptedRow{{values: []any{fixture.source.CanonicalWire, []byte(fixture.source.PreRedemptionVersion)}}}
			if name == "missing row" {
				rows = nil
			}
			if name == "different actor" {
				access.actorID++
			}
			if name == "different node" {
				node = "nonexistent"
			}
			if name == "wrong thread" {
				thread += "/made-up"
			}
			if name == "wrong purpose" {
				purpose = "execute"
			}
			if name == "changed captured bytes" {
				rows[0].values[1] = bytes.Replace([]byte(fixture.source.PreRedemptionVersion), []byte("python"), []byte("bash"), 1)
			}
			if name == "unregistered child" {
				rawScope = []byte(`{"scope_id":"` + strings.Repeat("a", 64) + `","revision":1,"digest_sha256":"` + strings.Repeat("b", 64) + `"}`)
				rows = nil
			}
			if name == "missing protected reference" || name == "editable source pin" {
				var input runtimev1.AgentExecutionInputV1
				if proto.Unmarshal(access.input, &input) != nil {
					t.Fatal("fixture protobuf")
				}
				var app map[string]json.RawMessage
				if json.Unmarshal(input.Application, &app) != nil {
					t.Fatal("fixture application")
				}
				if name == "missing protected reference" {
					delete(app, "source_definition")
				} else {
					ref := fixture.source.Reference
					ref.SourceDefinitionSHA256 = strings.Repeat("e", 64)
					app["source_definition"], _ = json.Marshal(ref)
				}
				input.Application, _ = json.Marshal(app)
				access.input, _ = proto.Marshal(&input)
			}
			tx := &scriptedExecutor{rowResults: rows}
			adapter, err := NewCodeDefinitionSourceAdapter(&ExecutionChildScopesRepository{})
			if err != nil {
				t.Fatal(err)
			}
			source, err := adapter.readOriginalCodeSource(t.Context(), tx, fixture.claim.ExecutionID, fixture.claim.Generation, access, rawScope, purpose, thread, node)
			if name == "captured source" {
				if err != nil || source.Reference != fixture.source.Reference || source.Instructions != fixture.source.Instructions || source.Reference.SourceDefinitionSHA256 == code.Digest([]byte(source.Instructions)) {
					t.Fatal("capture identity was replaced by runtime/YAML hash", source, err)
				}
				return
			}
			if err == nil || source.Reference != (scope.SourceReference{}) {
				t.Fatal("invalid source/member authority admitted", source, err)
			}
		})
	}
}
