package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

func TestStripClientIdentityHeadersDeletesEveryListedName(t *testing.T) {
	if len(middleware.ClientIdentityHeaders) != 14 {
		t.Fatalf("ClientIdentityHeaders has %d names, want 14; extend this test with the list, never shorten the list",
			len(middleware.ClientIdentityHeaders))
	}
	var seen http.Header
	handler := middleware.StripClientIdentityHeaders(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))

	request := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	for _, name := range middleware.ClientIdentityHeaders {
		// Twice, and in lower case: Header.Del must remove every value of the
		// canonical name, whatever casing the client used on the wire.
		request.Header.Add(name, "client-chosen")
		request.Header.Add(strings.ToLower(name), "client-chosen")
	}
	request.Header.Set("X-Elitea-Execution-Id", "exec-1")
	request.Header.Set("Authorization", "Bearer kept")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	for _, name := range middleware.ClientIdentityHeaders {
		if values := seen.Values(name); len(values) != 0 {
			t.Errorf("%s survived the strip: %v", name, values)
		}
	}
	// Legitimate client input on /llm, verified there (see the doc comment).
	if got := seen.Get("X-Elitea-Execution-Id"); got != "exec-1" {
		t.Errorf("X-Elitea-Execution-Id = %q, want it kept", got)
	}
	if got := seen.Get("Authorization"); got != "Bearer kept" {
		t.Errorf("Authorization = %q, want it untouched", got)
	}
}

func TestClientIdentityHeadersCoverTheGatewayIdentityTuple(t *testing.T) {
	for _, name := range []string{
		"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Signature",
		"X-Elitea-Identity-Signature", "X-Elitea-Project-Id", "X-Elitea-User-Id", "X-Elitea-Tenant-Id",
	} {
		found := false
		for _, listed := range middleware.ClientIdentityHeaders {
			if http.CanonicalHeaderKey(listed) == http.CanonicalHeaderKey(name) {
				found = true
			}
		}
		if !found {
			t.Errorf("ClientIdentityHeaders does not list %s", name)
		}
	}
	for _, listed := range middleware.ClientIdentityHeaders {
		if strings.EqualFold(listed, "X-Elitea-Execution-Id") {
			t.Error("ClientIdentityHeaders lists X-Elitea-Execution-Id, which the desktop and the worker send on /llm")
		}
	}
}
