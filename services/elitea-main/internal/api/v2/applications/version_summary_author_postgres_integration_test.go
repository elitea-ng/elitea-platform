package applications_test

// Issue 940/A11 (ELITEA-3278): the version-selector dropdown's search box
// filters by CREATOR as well as by name (`AgentPipelineVersionSelector.tsx`),
// which needs the creator's name/email on the version LIST, not merely on
// the single-version detail read `fetchVersionDetails` already joins for.
//
// `getVersions` (the projection behind `GET /application/prompt_lib/{project}/{id}`'s
// `versions[]`) now carries the identical `author: {id, email, name}` object
// `versionDetailsResponse` already answers for one version — same join
// (`public.auth_core__user`), same "absent when there is genuinely no
// author, present-with-empty-strings when the join finds no matching
// account" convention.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestHandlerPostgres_VersionSummaryCarriesAuthor(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "creator@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "creator@elitea.ai"})

	_, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1", j14CreateBody("summary-author"))
	applicationID, _ := created["id"].(string)
	if applicationID == "" {
		t.Fatalf("create answered no id: %v", created)
	}
	baseVersionID, _ := created["version_details"].(map[string]any)["id"].(string)

	_, fetched := do(t, router, http.MethodGet, "/application/prompt_lib/1/"+applicationID, nil)
	summary := versionByID(t, fetched, baseVersionID)

	author, ok := summary["author"].(map[string]any)
	if !ok {
		t.Fatalf("versions[].author is not an object: %#v (issue 940/A11)", summary["author"])
	}
	if got := author["email"]; got != "creator@elitea.ai" {
		t.Errorf("versions[].author.email = %v, want %q", got, "creator@elitea.ai")
	}
	if got, ok := author["name"].(string); !ok || got == "" {
		t.Errorf("versions[].author.name = %#v, want the seeded user's name", author["name"])
	}

	// The detail half of the SAME version already answers this object
	// (`versionDetailsResponse`) — the two must agree, or a client that reads
	// the summary and the detail of the same version sees two different
	// creators.
	detail, _ := fetched["version_details"].(map[string]any)
	detailAuthor, _ := detail["author"].(map[string]any)
	if detailAuthor["email"] != author["email"] {
		t.Errorf("the summary and the detail of the SAME version disagree on author.email: %v vs %v", author["email"], detailAuthor["email"])
	}
}
