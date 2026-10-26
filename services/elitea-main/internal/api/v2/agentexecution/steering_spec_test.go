package agentexecution

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/elitea-ng/elitea-platform/services/elitea-main/api/openapi"
)

// TestSteeringSpecMatchesTheRoute: client contract 1.26 publishes the four
// steering operations. Each must sit on the route's own path, be a client
// operation, and declare every status the route answers, so the lock never
// records behaviour the server does not have.
func TestSteeringSpecMatchesTheRoute(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	specPath := func(chiPath string) string {
		replacer := strings.NewReplacer("/api/v2", "", "{projectID}", "{project_id}",
			"{responseMessageID}", "{response_message_id}", "{steeringID}", "{steering_id}")
		return replacer.Replace(chiPath)
	}
	for _, test := range []struct {
		operationID string
		method      string
		path        string
		statuses    []string
	}{
		{"steerChatExecution", http.MethodPost, CurrentAgentSteeringPath, []string{"202", "400", "401", "403", "409", "426", "429", "502", "504"}},
		{"listChatExecutionSteering", http.MethodGet, CurrentAgentSteeringPath, []string{"200", "400", "401", "403", "426", "502", "504"}},
		{"updateChatExecutionSteering", http.MethodPatch, CurrentAgentSteeringItemPath, []string{"200", "400", "401", "403", "404", "409", "426", "502", "504"}},
		{"removeChatExecutionSteering", http.MethodDelete, CurrentAgentSteeringItemPath, []string{"204", "400", "401", "403", "404", "409", "426", "502", "504"}},
	} {
		t.Run(test.operationID, func(t *testing.T) {
			item := doc.Paths.Value(specPath(test.path))
			if item == nil {
				t.Fatalf("v2.yaml has no path %s", specPath(test.path))
			}
			op := item.GetOperation(test.method)
			if op == nil || op.OperationID != test.operationID {
				t.Fatalf("%s %s is not %s", test.method, specPath(test.path), test.operationID)
			}
			if len(op.Tags) == 0 || op.Tags[len(op.Tags)-1] != "client" || !slices.Contains(op.Tags, "chat") {
				t.Fatalf("tags=%v, want chat and client (last)", op.Tags)
			}
			for _, status := range test.statuses {
				if op.Responses.Value(status) == nil {
					t.Errorf("%s declares no %s", test.operationID, status)
				}
			}
		})
	}
}
