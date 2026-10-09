package agentexecution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/EliteaAI/elitea-platform/services/elitea-main/api/openapi"
)

// chiParam matches a chi path parameter such as {projectID}.
var chiParam = regexp.MustCompile(`\{[^}/]+\}`)

// specPathForChi maps a chi route to the shape the document uses: parameters
// become `{}` and the `/api/v2` server base is dropped, so the two spellings
// ({projectID} against {project_id}) compare by structure.
func specPathForChi(path string) string {
	const base = "/api/v2"
	if len(path) >= len(base) && path[:len(base)] == base {
		path = path[len(base):]
	}
	return chiParam.ReplaceAllString(path, "{}")
}

// TestExecutionInterruptSpecMatchesRoute pins the two Wave 1 operations in
// api/openapi/v2.yaml to the hand-written route: the paths, the status codes
// the handler writes, the request body it parses, and that the contract-locked
// `client` tag is NOT on them (no client uses this API yet, and the lock is
// additive-only).
func TestExecutionInterruptSpecMatchesRoute(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}

	find := func(operationID string) (string, string, *openapi3.Operation) {
		for path, item := range doc.Paths.Map() {
			for method, op := range item.Operations() {
				if op.OperationID == operationID {
					return path, method, op
				}
			}
		}
		t.Fatalf("v2.yaml has no %s operation", operationID)
		return "", "", nil
	}

	listPath, listMethod, list := find("listExecutionInterrupts")
	decidePath, decideMethod, decide := find("decideExecutionInterrupt")

	if listMethod != "GET" || specPathForChi(CurrentExecutionInterruptsPath) != specPathForChi(listPath) {
		t.Errorf("listExecutionInterrupts is %s %s, route is GET %s", listMethod, listPath, CurrentExecutionInterruptsPath)
	}
	if decideMethod != "POST" || specPathForChi(CurrentExecutionInterruptDecisionPath) != specPathForChi(decidePath) {
		t.Errorf("decideExecutionInterrupt is %s %s, route is POST %s", decideMethod, decidePath, CurrentExecutionInterruptDecisionPath)
	}

	for name, op := range map[string]*openapi3.Operation{"listExecutionInterrupts": list, "decideExecutionInterrupt": decide} {
		if slices.Contains(op.Tags, "client") {
			t.Errorf("%s is tagged client, but no client uses this API and the contract lock is additive-only", name)
		}
	}
	for _, code := range []string{"200", "401", "403", "404"} {
		if list.Responses.Value(code) == nil {
			t.Errorf("listExecutionInterrupts declares no %s", code)
		}
	}
	for _, code := range []string{"200", "400", "401", "403", "404", "409"} {
		if decide.Responses.Value(code) == nil {
			t.Errorf("decideExecutionInterrupt declares no %s", code)
		}
	}

	if decide.RequestBody == nil || decide.RequestBody.Value == nil {
		t.Fatal("decideExecutionInterrupt declares no request body")
	}
	media := decide.RequestBody.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("decideExecutionInterrupt request body has no application/json schema")
	}
	body := media.Schema.Value
	if body.AdditionalProperties.Has == nil || *body.AdditionalProperties.Has {
		t.Error("the decision request schema must set additionalProperties: false")
	}
	for _, field := range []string{"request_id", "expected_revision", "action", "value"} {
		if !slices.Contains(body.Required, field) {
			t.Errorf("the decision request does not require %q", field)
		}
	}
	action := body.Properties["action"]
	if action == nil || action.Value == nil {
		t.Fatal("the decision request has no action property")
	}
	got := make([]string, 0, len(action.Value.Enum))
	for _, value := range action.Value.Enum {
		got = append(got, value.(string))
	}
	want := []string{"approve", "reject", "edit", "block_with_comment", "answer", "authorize", "skip", "continue"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("action enum = %v, want %v", got, want)
	}
	if value := body.Properties["value"]; value == nil || value.Value == nil || value.Value.MaxLength == nil || *value.Value.MaxLength != 8192 {
		t.Error("the decision request value must be capped at 8192")
	}
}

// TestExecutionInterruptCardSpecAcceptsContractCards keeps the OpenAPI card
// schema a faithful mirror of fanout-interrupt-card.v1: every valid contract
// card validates against it, and the closed-object and bound fixtures the
// OpenAPI subset can express are refused.
func TestExecutionInterruptCardSpecAcceptsContractCards(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	ref := doc.Components.Schemas["ExecutionInterruptCard"]
	if ref == nil || ref.Value == nil {
		t.Fatal("v2.yaml has no ExecutionInterruptCard schema")
	}
	paths, err := filepath.Glob(filepath.Join(interruptContractDir, "fixtures", "fanout-interrupt-card-v1*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatal(paths, err)
	}
	expressible := map[string]bool{
		"fanout-interrupt-card-v1.invalid.secret-display-field.json":  true,
		"fanout-interrupt-card-v1.invalid.plain-http-server-url.json": true,
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var card any
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		err = ref.Value.VisitJSON(card)
		switch {
		case !strings.Contains(name, ".invalid.") && err != nil:
			t.Errorf("%s: valid contract card refused by v2.yaml: %v", name, err)
		case expressible[name] && err == nil:
			t.Errorf("%s: invalid contract card accepted by v2.yaml", name)
		}
	}
}
