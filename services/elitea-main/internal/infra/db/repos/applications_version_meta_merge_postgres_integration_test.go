package repos

// A version save must not destroy the `meta` keys it does not carry.
//
// `application_versions.meta` is a bag of independent keys — `step_limit`,
// `icon_meta`, `internal_tools`, `variables`, and the `parent_entity_id` /
// `parent_project_id` / `parent_author_id` fork provenance — each written by a
// different feature and read by a different one. No client sends them all: the
// agent editor's draft models two of them, and the HTTP layer synthesises a
// one-key `{"variables": …}` for a body that carries `variables` and no `meta`.
//
// UpdateVersion wrote `meta = $n::jsonb`, a whole-column overwrite, so an
// ordinary save wrote that two-key or one-key object over the whole bag and
// every other key was gone — behind a 201. `step_limit` is one of the four
// gates a stored agent must pass to be admitted by the Rust runtime, so an
// agent could be edited into being unrunnable by a request that reported
// success.
//
// This is the repository half of the pin: it asserts the SQL, one call away
// from the HTTP decode, so a later change to the handler cannot make the
// column-level rule pass by accident.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// The keys a stored agent carries besides the ones an editor knows about.
func seedFullVersionMeta() map[string]any {
	return map[string]any{
		"step_limit":        float64(7),
		"icon_meta":         map[string]any{"icon": "bolt"},
		"internal_tools":    []any{"canvas"},
		"parent_entity_id":  float64(41),
		"parent_project_id": float64(2),
		"parent_author_id":  float64(3),
		"variables":         []any{map[string]any{"name": "region", "value": "emea"}},
	}
}

func TestApplicationsRepoPostgres_UpdateVersionMergesMetaInsteadOfReplacingIt(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "meta-merge", 1, &applications.Version{
		Name:     "base",
		AuthorID: 1,
		Meta:     seedFullVersionMeta(),
	})
	versionID := app.Versions[0].ID

	// Exactly what the HTTP layer builds for a body that carries `variables`
	// and no `meta` at all: a single key.
	saved, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		Meta: map[string]any{
			"variables": []any{map[string]any{"name": "region", "value": "apac"}},
		},
	})
	if err != nil {
		t.Fatalf("update version: %v", err)
	}

	// The gate the runtime reads. Before the merge this was absent.
	if got := saved.Meta["step_limit"]; got != float64(7) {
		t.Errorf("meta.step_limit = %v, want 7 — a one-key save destroyed the runtime's admission gate", got)
	}
	if _, present := saved.Meta["icon_meta"]; !present {
		t.Errorf("meta.icon_meta is gone: %v", saved.Meta)
	}
	for _, key := range []string{"parent_entity_id", "parent_project_id", "parent_author_id"} {
		if _, present := saved.Meta[key]; !present {
			t.Errorf("meta.%s is gone — the fork provenance did not survive an ordinary save: %v", key, saved.Meta)
		}
	}
	// …and the key the save WAS about still wins. A merge that kept
	// everything by refusing the write would satisfy every assertion above.
	edited, ok := saved.Meta["variables"].([]any)
	if !ok || len(edited) != 1 {
		t.Fatalf("meta.variables = %v, want the one edited variable", saved.Meta["variables"])
	}
	if first, _ := edited[0].(map[string]any); first["value"] != "apac" {
		t.Errorf("meta.variables[0] = %v, want the edited value — the merge kept the stored array", edited[0])
	}

	// The RETURNING clause and the stored row must agree: the echo is what
	// the HTTP layer answers, and a merge computed only in Go would leave
	// the two disagreeing.
	readBack, err := repo.GetVersion(ctx, testProjectID, app.ID, versionID)
	if err != nil {
		t.Fatalf("read the version back: %v", err)
	}
	if readBack.Meta["step_limit"] != float64(7) || readBack.Meta["icon_meta"] == nil {
		t.Errorf("the stored row disagrees with the update's echo: %v", readBack.Meta)
	}
}

// A key the caller DOES send still replaces its own value, and an empty
// array still clears the array it names. Without this, "merge" could mean
// "the stored value always wins", which would lose the edit instead of the
// neighbour.
func TestApplicationsRepoPostgres_UpdateVersionMetaStillOverwritesTheKeysItCarries(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "meta-overwrite", 1, &applications.Version{
		Name:     "base",
		AuthorID: 1,
		Meta:     seedFullVersionMeta(),
	})
	versionID := app.Versions[0].ID

	saved, err := repo.UpdateVersion(ctx, testProjectID, app.ID, versionID, applications.Version{
		Meta: map[string]any{
			"step_limit": float64(40),
			"variables":  []any{},
		},
	})
	if err != nil {
		t.Fatalf("update version: %v", err)
	}
	if got := saved.Meta["step_limit"]; got != float64(40) {
		t.Errorf("meta.step_limit = %v, want 40 — the caller's own key must win", got)
	}
	cleared, ok := saved.Meta["variables"].([]any)
	if !ok || len(cleared) != 0 {
		t.Errorf("meta.variables = %v, want an empty list — deleting the last variable is a real action",
			saved.Meta["variables"])
	}
	if _, present := saved.Meta["icon_meta"]; !present {
		t.Errorf("meta.icon_meta is gone: %v", saved.Meta)
	}
}
