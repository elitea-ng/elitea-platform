package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func driftRequest(t *testing.T, method, path string, status int) []int64 {
	t.Helper()
	var triggered []int64
	router := chi.NewRouter()
	router.Use(DelegationDrift(func(projectID int64) { triggered = append(triggered, projectID) }))
	router.HandleFunc("/api/v2/elitea_core/{kind}/prompt_lib/{projectID}/*", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	return triggered
}

func TestDelegationDriftRunsAfterASuccessfulWriteToACalleeSurface(t *testing.T) {
	for _, path := range []string{
		"/api/v2/elitea_core/version/prompt_lib/7/31/41",
		"/api/v2/elitea_core/application/prompt_lib/7/31",
		"/api/v2/elitea_core/tool/prompt_lib/7/51",
		"/api/v2/elitea_core/marketplace_upgrade/prompt_lib/7/31",
		"/api/v2/elitea_core/agent_registry/prompt_lib/7/31",
	} {
		if got := driftRequest(t, http.MethodPut, path, http.StatusOK); len(got) != 1 || got[0] != 7 {
			t.Fatalf("%s: %v", path, got)
		}
	}
}

func TestDelegationDriftIgnoresReadsFailuresAndOtherSurfaces(t *testing.T) {
	cases := map[string]struct {
		method, path string
		status       int
	}{
		"a read":              {http.MethodGet, "/api/v2/elitea_core/version/prompt_lib/7/31/41", http.StatusOK},
		"a refused write":     {http.MethodPut, "/api/v2/elitea_core/version/prompt_lib/7/31/41", http.StatusForbidden},
		"a failed write":      {http.MethodPut, "/api/v2/elitea_core/version/prompt_lib/7/31/41", http.StatusInternalServerError},
		"a grant route":       {http.MethodPost, "/api/v2/elitea_core/delegation_grant/prompt_lib/7/32/1/approve", http.StatusOK},
		"an unrelated write":  {http.MethodPost, "/api/v2/elitea_core/likes/prompt_lib/7/31", http.StatusOK},
		"outside the project": {http.MethodPost, "/api/v1/elitea_core/version/prompt_lib/7/31", http.StatusOK},
	}
	for name, tc := range cases {
		if got := driftRequest(t, tc.method, tc.path, tc.status); len(got) != 0 {
			t.Fatalf("%s must not trigger a check: %v", name, got)
		}
	}
}

func TestDelegationDriftWithoutATriggerIsAPassThrough(t *testing.T) {
	called := false
	handler := DelegationDrift(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/version/prompt_lib/7/31", nil))
	if !called {
		t.Fatal("the handler must run")
	}
}
