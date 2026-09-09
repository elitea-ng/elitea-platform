package applications_test

// Two halves of one round trip that were missing on the CREATE side and on the
// READ side.
//
// TAGS AT CREATION. `versionFromBody` reads name, agent_type, instructions,
// welcome_message, llm_settings, conversation_starters, meta and variables —
// and not `tags`. `replaceVersionTags` was called from UpdateVersion alone. So
// the same payload persisted a tag through a SAVE and dropped it through a
// CREATE: the create-agent form's tag control was accepted with a 201, echoed
// an empty list, and the project's tag list stayed empty. A user who tagged an
// agent while creating it had to open it again and tag it a second time, and
// nothing said so.
//
// THE AUTHOR ON THE READ. The write echo answers an `author` object —
// `{id, email, name}` — and `GET /version/...`, the agent editor's own reload
// of the row it just wrote, answered `author_id` and nothing else. The name a
// user saw beside a version survived until the page was refreshed.
//
// Both are asserted on the READ and not on the write's echo: this endpoint
// family has a documented history of echoes that were true about the response
// and false about the database, which is the reason the tag defect above went
// unnoticed for as long as it did.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// createBodyWithTags is the create request the agent form sends when the user
// picks two tags before saving for the first time.
func createBodyWithTags(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "created by the tag contract test",
		"type":        "agent",
		"versions": []any{map[string]any{
			"name":         "base",
			"agent_type":   "openai",
			"instructions": "Follow the brief.",
			"tags": []any{
				map[string]any{"name": "autotest_tag_one", "data": map[string]any{"color": "blue"}},
				map[string]any{"name": "autotest_tag_two"},
			},
		}},
	}
}

func tagNamesOf(t *testing.T, detail map[string]any) []string {
	t.Helper()
	raw, _ := detail["tags"].([]any)
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		tag, _ := entry.(map[string]any)
		if tag == nil {
			continue
		}
		name, _ := tag["name"].(string)
		names = append(names, name)
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestHandlerPostgres_TagsSentAtCreationArePersisted(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1",
		createBodyWithTags("autotest_created_with_tags"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	details, _ := created["version_details"].(map[string]any)
	versionID, _ := details["id"].(string)
	if applicationID == "" || versionID == "" {
		t.Fatalf("create answered no ids: %v", created)
	}

	// The echo reports what the database now holds. It answered a hardcoded
	// empty list, which was true only because the create wrote nothing.
	echoed := tagNamesOf(t, details)
	for _, want := range []string{"autotest_tag_one", "autotest_tag_two"} {
		if !hasName(echoed, want) {
			t.Errorf("the create echo does not carry %q: %v", want, echoed)
		}
	}

	// THE ROW. The editor's own reload is where the defect was visible: a
	// create answered 201 with tags and the agent came back untagged forever.
	read, stored := do(t, router, http.MethodGet,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read: status = %d, body = %s", read.Code, read.Body.String())
	}
	names := tagNamesOf(t, stored)
	for _, want := range []string{"autotest_tag_one", "autotest_tag_two"} {
		if !hasName(names, want) {
			t.Errorf("the stored version does not carry %q: %v", want, names)
		}
	}

	// …and the association reached the project's own `tags` table, which is
	// what the tag rail and the tag filter read.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM p_1.application_version_tag_association a
		JOIN p_1.tags t ON t.id = a.tag_id
		WHERE a.version_id = $1 AND t.name IN ('autotest_tag_one', 'autotest_tag_two')`,
		versionID).Scan(&count); err != nil {
		t.Fatalf("count associations: %v", err)
	}
	if count != 2 {
		t.Errorf("the project holds %d of the 2 tag associations the create was given", count)
	}
}

func TestHandlerPostgres_TheVersionReadCarriesTheAuthorObject(t *testing.T) {
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 9, "nine@elitea.ai")
	router := newHandlerTestServer(t, pool, auth.User{ID: "9", UserID: "9", Email: "nine@elitea.ai"})

	recorder, created := do(t, router, http.MethodPost, "/applications/prompt_lib/1",
		j14CreateBody("autotest_author_on_read"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	applicationID, _ := created["id"].(string)
	details, _ := created["version_details"].(map[string]any)
	versionID, _ := details["id"].(string)

	read, stored := do(t, router, http.MethodGet,
		"/version/prompt_lib/1/"+applicationID+"/"+versionID, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read: status = %d, body = %s", read.Code, read.Body.String())
	}
	author, ok := stored["author"].(map[string]any)
	if !ok {
		t.Fatalf("the version read carries no author object: %v", stored["author"])
	}
	if author["id"] != "9" {
		t.Errorf("author.id = %v, want the principal that wrote the version", author["id"])
	}
	if author["email"] != "nine@elitea.ai" {
		t.Errorf("author.email = %v, want the account's address", author["email"])
	}
	if name, _ := author["name"].(string); name == "" {
		t.Error("author.name is empty; the join found no account for the version's author_id")
	}
	// The read and the write echo must agree — they are two projections of
	// one row, and the editor uses both.
	echoAuthor, _ := details["author"].(map[string]any)
	if echoAuthor["id"] != author["id"] || echoAuthor["email"] != author["email"] {
		t.Errorf("the write echo says %v and the read says %v", echoAuthor, author)
	}
}
