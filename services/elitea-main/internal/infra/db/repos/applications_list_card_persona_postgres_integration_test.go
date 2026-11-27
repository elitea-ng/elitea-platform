package repos

import (
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// The Agents list card (#3133) draws the agent's persona role and plays its
// own live avatar, so the list row carries both on its passthrough `meta`. An
// agent with neither carries neither key: a card then falls back to the
// monogram and shows no role line.
func TestApplicationsRepoPostgres_ListCarriesThePersonaRoleAndAvatarPack(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 41, "persona@elitea.ai")

	withPersona := createTestApplication(t, repo, "card-with-a-persona", 41, &applications.Version{
		Name:       "base",
		Soul:       json.RawMessage(`{"schema":"elitea.soul/v1","role":"  Code reviewer  "}`),
		AvatarPack: json.RawMessage(`{"schema":"elitea.avatar_pack/v1","still":{"url":"/media/still.webp","content_type":"image/webp"}}`),
	})
	plain := createTestApplication(t, repo, "card-without-a-persona", 41, &applications.Version{Name: "base"})

	list, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	meta := map[string]map[string]any{}
	for _, row := range list.Rows {
		meta[row.ID] = row.Meta
	}

	got := meta[withPersona.ID]
	if got["persona_role"] != "Code reviewer" {
		t.Errorf("persona_role = %v, want the trimmed role", got["persona_role"])
	}
	pack, ok := got["avatar_pack"].(map[string]any)
	if !ok || pack["schema"] != "elitea.avatar_pack/v1" {
		t.Errorf("avatar_pack = %v, want the version's pack as an object", got["avatar_pack"])
	}
	if _, ok := meta[plain.ID]["persona_role"]; ok {
		t.Errorf("an agent with no soul carries persona_role %v", meta[plain.ID]["persona_role"])
	}
	if _, ok := meta[plain.ID]["avatar_pack"]; ok {
		t.Errorf("an agent with no pack carries avatar_pack %v", meta[plain.ID]["avatar_pack"])
	}
}

// The card describes the version a chat from it runs: the default version,
// not the first one, when an author gave only a later version a persona.
func TestApplicationsRepoPostgres_ListTakesThePersonaFromTheDefaultVersion(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 42, "default@elitea.ai")

	app := createTestApplication(t, repo, "persona-on-a-later-version", 42, &applications.Version{Name: "base"})
	later, err := repo.CreateVersion(ctx, testProjectID, app.ID, applications.Version{
		Name:     "v2",
		AuthorID: 42,
		Soul:     json.RawMessage(`{"schema":"elitea.soul/v1","role":"Release manager"}`),
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	listRole := func() any {
		t.Helper()
		list, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 1, PageSize: 50})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, row := range list.Rows {
			if row.ID == app.ID {
				return row.Meta["persona_role"]
			}
		}
		t.Fatalf("agent %s not listed", app.ID)
		return nil
	}

	if got := listRole(); got != nil {
		t.Errorf("before the default moves, persona_role = %v, want none (base has no soul)", got)
	}
	if err := repo.SetDefaultVersion(ctx, testProjectID, app.ID, later.ID); err != nil {
		t.Fatalf("set default version: %v", err)
	}
	if got := listRole(); got != "Release manager" {
		t.Errorf("persona_role = %v, want the default version's role", got)
	}
}

func TestSurfaceCardPersonaIgnoresEmptyAndMalformedInput(t *testing.T) {
	meta := map[string]any{}
	surfaceCardPersona(meta, "   ", "null")
	surfaceCardPersona(meta, "", "not json")
	surfaceCardPersona(meta, "", "{}")
	if len(meta) != 0 {
		t.Errorf("meta = %v, want nothing surfaced", meta)
	}
}
