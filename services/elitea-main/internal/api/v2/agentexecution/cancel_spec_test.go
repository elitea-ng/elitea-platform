package agentexecution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specfiles "github.com/EliteaAI/elitea-platform/services/elitea-main/api/openapi"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TestCancelChatExecutionSpecMatchesProjectIDRefusal: the route refuses an
// unparseable project id in RequireResolvedPermissionsForProject, before the
// handler, with 403 — TestCurrentAgentCancelRouteRejectsInvalidForbiddenAndNonOwner
// pins it. Client contract 1.3 publishes cancelChatExecution, so its 400 must
// not claim the project id and its text must say an invalid one answers 403;
// otherwise the lock records behaviour the server does not have, and a client
// reading 403 as "lost access" misreads its own bad request.
func TestCancelChatExecutionSpecMatchesProjectIDRefusal(t *testing.T) {
	canceller := &currentAgentCancellerStub{}
	route := newCurrentAgentCancelRoute(t, canceller, currentStartPermissionResolverFunc(func(
		context.Context, auth.User, string, string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentAgentCancelPermission}}, nil
	}))
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAgentCancelRequest(http.MethodDelete, "no", "10000000-0000-4000-8000-000000000044"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("invalid project id status=%d, want 403 (update the spec with the route)", response.Code)
	}

	doc, err := openapi3.NewLoader().LoadFromData(specfiles.SpecYAML)
	if err != nil {
		t.Fatal(err)
	}
	var op *openapi3.Operation
	for _, item := range doc.Paths.Map() {
		if item.Delete != nil && item.Delete.OperationID == "cancelChatExecution" {
			op = item.Delete
		}
	}
	if op == nil {
		t.Fatal("v2.yaml has no cancelChatExecution operation")
	}
	bad := op.Responses.Value("400")
	if bad == nil || bad.Value == nil || bad.Value.Description == nil {
		t.Fatal("cancelChatExecution declares no 400")
	}
	if strings.Contains(strings.ToLower(*bad.Value.Description), "project id") {
		t.Errorf("cancelChatExecution's 400 claims an invalid project id, but the route answers 403: %q", *bad.Value.Description)
	}
	if op.Responses.Value("403") == nil {
		t.Error("cancelChatExecution declares no 403")
	}
	if !strings.Contains(strings.ToLower(op.Description), "invalid project id answers 403") {
		t.Error("cancelChatExecution's description does not say an invalid project id answers 403")
	}
}
