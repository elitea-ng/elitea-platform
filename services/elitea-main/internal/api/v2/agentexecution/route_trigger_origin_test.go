package agentexecution

import (
	"net/http"
	"net/http/httptest"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The start route stamps how the run began (shared 0140): a browser session
// is the chat composer (manual), an access token is a programmatic client
// (api). Both are a person acting; only the execution row and
// /analytics_execution tell them apart.
func TestCurrentStartRouteStampsTheTriggerOrigin(t *testing.T) {
	asToken := func(request *http.Request) *http.Request {
		request.Header.Set("X-Auth-Type", "token")
		request.Header.Set("X-Auth-ID", "11")
		request.Header.Set("X-Auth-User-ID", "11")
		return request
	}
	cases := []struct {
		name    string
		request *http.Request
		adhoc   bool
		want    executiondomain.TriggerOrigin
	}{
		{"application start from a session", currentStartRequest(validCurrentStartBody()), false, executiondomain.TriggerOriginManual},
		{"application start from a token", asToken(currentStartRequest(validCurrentStartBody())), false, executiondomain.TriggerOriginAPI},
		{"ad-hoc start from a session", currentAdhocStartRequest(validCurrentAdhocStartBody()), true, executiondomain.TriggerOriginManual},
		{"ad-hoc start from a token", asToken(currentAdhocStartRequest(validCurrentAdhocStartBody())), true, executiondomain.TriggerOriginAPI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
				ExecutionID: "execution-1", CommandID: "command-1", ResponseMessageID: "response-1", Created: true,
			}}
			route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
			response := httptest.NewRecorder()
			route.ServeHTTP(response, tc.request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			got := useCase.request.TriggerOrigin
			if tc.adhoc {
				got = useCase.adhocRequest.TriggerOrigin
			}
			if got != tc.want {
				t.Fatalf("trigger origin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStartTriggerOriginReadsTheCredentialKind(t *testing.T) {
	if got := startTriggerOrigin(auth.User{ID: "11", UserID: "11", AuthType: "session"}); got != executiondomain.TriggerOriginManual {
		t.Fatalf("session = %q, want manual", got)
	}
	if got := startTriggerOrigin(auth.User{ID: "11", UserID: "11", TokenID: "5", AuthType: "token"}); got != executiondomain.TriggerOriginAPI {
		t.Fatalf("token = %q, want api", got)
	}
}
