package api

// "What the agent saw" (ADR-0050 decision 9, #1669): the read is gated on the
// message read's permission and confined to the caller's project. With the
// permission held the request reaches the handler, which, with no database
// in this router, answers 503.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContextViewRouteNeedsTheMessageReadPermission(t *testing.T) {
	const path = "/api/v2/elitea_core/message_context_view/prompt_lib/7/00000000-0000-4000-8000-000000000001"
	serve := func(granted []string, target string) *httptest.ResponseRecorder {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
			fakePermissionResolver{granted: granted, forProject: "7"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, target, nil)))
		return recorder
	}
	if recorder := serve([]string{"models.chat.messages.list"}, path); recorder.Code != http.StatusForbidden {
		t.Fatalf("without models.chat.messages.details: status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := serve([]string{"models.chat.messages.details"}, path); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the permission: status = %d, want the handler's 503; body=%s", recorder.Code, recorder.Body.String())
	}
	other := "/api/v2/elitea_core/message_context_view/prompt_lib/8/00000000-0000-4000-8000-000000000001"
	if recorder := serve([]string{"models.chat.messages.details"}, other); recorder.Code != http.StatusForbidden {
		t.Fatalf("another project: status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
}
