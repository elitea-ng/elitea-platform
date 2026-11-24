package api

// The central override reaches no content (ADR-0059 R0.6b, issue 2585): a
// holder of the built-in project administrator set, who is not a member of a
// project, reaches its guarded settings and its membership, and still gets
// 403 or 404 on its conversations, its artifacts and its secret values. Each
// request goes through the composed router (NewRouter).

import (
	"net/http"
	"testing"
)

func expectNoContent(t *testing.T, method, path string) {
	t.Helper()
	router := newModeRouter(t, modeResolver{central: projectAdministratorSet(t)})
	got := servePlatformGate(t, router, method, path, "").Code
	if got != http.StatusForbidden && got != http.StatusNotFound {
		t.Fatalf("%s %s = %d, want 403 or 404", method, path, got)
	}
}

func TestTheProjectAdministratorReadsNoConversation(t *testing.T) {
	expectNoContent(t, http.MethodGet, "/api/v2/elitea_core/conversation/prompt_lib/7/1")
	expectNoContent(t, http.MethodGet, "/api/v2/elitea_core/conversations/prompt_lib/7")
}

func TestTheProjectAdministratorReadsNoArtifact(t *testing.T) {
	expectNoContent(t, http.MethodGet, "/api/v2/artifacts/objects/7/files/report.txt")
	expectNoContent(t, http.MethodGet, "/api/v2/artifacts/objects/7/files")
}

func TestTheProjectAdministratorReadsNoSecretValue(t *testing.T) {
	expectNoContent(t, http.MethodGet, "/api/v2/secrets/secret/7/api_key")
	expectNoContent(t, http.MethodGet, "/api/v2/secrets/secret/prompt_lib/7/api_key")
}
