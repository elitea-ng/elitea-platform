package agentexecution

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func withAddressedTo(body, ids string) string {
	return strings.Replace(body, `"project_id":7,`, `"project_id":7,"addressed_to":`+ids+`,`, 1)
}

// ADR-0050 decision 7c: addressed_to reaches the use case as sent.
func TestCurrentStartRouteCarriesAddressedTo(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "e", CommandID: "c"}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(withAddressedTo(validCurrentStartBody(), `[22,21]`)))
	if response.Code != http.StatusOK || useCase.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
	if got := useCase.request.AddressedTo; !reflect.DeepEqual(got, []int64{22, 21}) {
		t.Fatalf("addressed_to = %v", got)
	}
}

func TestCurrentStartRouteRefusesAMalformedAddress(t *testing.T) {
	for _, ids := range []string{`[0]`, `[-1]`, `[3,3]`, `["a"]`, `[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]`} {
		useCase := &currentStartUseCaseStub{}
		route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentStartRequest(withAddressedTo(validCurrentStartBody(), ids)))
		if response.Code != http.StatusBadRequest || useCase.calls != 0 {
			t.Fatalf("%s: status=%d calls=%d", ids, response.Code, useCase.calls)
		}
	}
}

func TestCurrentStartRouteNamesAnAddresseeOutsideTheConversation(t *testing.T) {
	useCase := &currentStartUseCaseStub{err: agentexecutionapp.ErrFloorAddressee}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(withAddressedTo(validCurrentStartBody(), `[99]`)))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "addressee_not_in_conversation") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
