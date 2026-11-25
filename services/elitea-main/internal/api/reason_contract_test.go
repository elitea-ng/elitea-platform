package api

// v2.yaml documents the reason of a dangerous action on each marked route it
// documents (ADR-0059 R3.8a, issue 2539): the two request headers, the
// Elitea-Reason-Missing response header and a 400. Many marked routes are not
// in the spec yet; a marked route that is must carry all of it, and no
// unmarked operation may claim the headers.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/generated"
)

var specPlaceholder = regexp.MustCompile(`\{[^}]+\}`)

// reasonSpecKey normalises "METHOD /path/{x}" so the router's and the spec's
// placeholder names compare equal.
func reasonSpecKey(method, path string) string {
	return strings.ToUpper(method) + " " + specPlaceholder.ReplaceAllString(path, "{}")
}

func hasParameterRef(op *openapi3.Operation, name string) bool {
	for _, p := range op.Parameters {
		if p.Ref == "#/components/parameters/"+name {
			return true
		}
	}
	return false
}

func TestTheSpecDocumentsTheReasonOnEveryMarkedOperation(t *testing.T) {
	spec, err := generated.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Reason {
			marked[reasonSpecKey(entry.Method, strings.TrimPrefix(entry.Pattern, "/api/v2"))] = true
		}
	}
	documented := 0
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			key := reasonSpecKey(method, path)
			claims := hasParameterRef(op, "EliteaReasonCode") || hasParameterRef(op, "EliteaReason")
			if !marked[key] {
				if claims {
					t.Errorf("%s documents the reason headers and no entry of the action table asks for a reason", key)
				}
				continue
			}
			documented++
			if !hasParameterRef(op, "EliteaReasonCode") || !hasParameterRef(op, "EliteaReason") {
				t.Errorf("%s (%s) is marked and does not reference both reason header parameters", key, op.OperationID)
			}
			if op.Responses.Value("400") == nil {
				t.Errorf("%s (%s) documents no 400 for reason_invalid", key, op.OperationID)
			}
			flagged := false
			for code, response := range op.Responses.Map() {
				if strings.HasPrefix(code, "2") && response.Value != nil && response.Value.Headers[adminaccess.ReasonMissingHeader] != nil {
					flagged = true
				}
			}
			if !flagged {
				t.Errorf("%s (%s) documents no %s on its success response", key, op.OperationID, adminaccess.ReasonMissingHeader)
			}
		}
	}
	if documented < 12 {
		t.Errorf("found %d documented marked operations, want at least the 12 v2.yaml has", documented)
	}
	if code := spec.Components.Schemas["ActionReasonCode"]; code == nil || len(code.Value.Enum) != len(adminaccess.ActionReasonCodes()) {
		t.Errorf("ActionReasonCode is not the enum of adminaccess.ActionReasonCodes")
	} else {
		for i, want := range adminaccess.ActionReasonCodes() {
			if code.Value.Enum[i] != want {
				t.Errorf("ActionReasonCode[%d] = %v, want %s", i, code.Value.Enum[i], want)
			}
		}
	}
}
