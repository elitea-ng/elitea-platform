package toolkits

// The toolkit repository against a real database, end to end.
//
// Every method here writes its own statement text with a tenant schema name
// interpolated into it, and most of them were reachable only through a handler
// that a stub could satisfy. A stubbed repository proves the handler; it proves
// nothing about the SQL, and the SQL is where a renamed column, a changed
// default or a missing table lands.
//
// One database, one fixture, one lifecycle: create, enumerate, read, update,
// fork, attach, validate, delete. Provisioning a database and running the
// baseline migrations is the expensive part, so the assertions share one.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL), which ci-go.yml
// provides.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func TestToolkitRepositoryLifecycleAgainstPostgres(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	repo := &pgRepo{pool: pool}

	created, err := repo.CreateToolkit(ctx, "1", map[string]any{
		"name":        "lifecycle-fixture",
		"type":        "custom",
		"description": "the repository lifecycle fixture",
		"settings":    map[string]any{"selected_tools": []any{}},
		"meta":        map[string]any{"origin": "test"},
		"_author_id":  "7",
	})
	if err != nil {
		t.Fatalf("CreateToolkit: %v", err)
	}
	toolkitID, _ := created["id"].(string)
	if toolkitID == "" {
		t.Fatalf("CreateToolkit returned no id: %#v", created)
	}

	t.Run("the type list is the distinct stored types", func(t *testing.T) {
		types, err := repo.ListTypes(ctx, "1")
		if err != nil {
			t.Fatalf("ListTypes: %v", err)
		}
		if len(types) != 1 || types[0] != "custom" {
			t.Errorf("types=%v, want [custom]", types)
		}
	})

	t.Run("a project id that is not a schema name is refused", func(t *testing.T) {
		// Every method quotes the schema first, so every method refuses. The
		// alternative is a raw PostgreSQL error carrying the caller's string.
		if _, err := repo.ListTypes(ctx, "not-a-project"); err == nil {
			t.Error("ListTypes accepted a non-project id")
		}
		if _, err := repo.AvailableTools(ctx, "not-a-project", "1"); err == nil {
			t.Error("AvailableTools accepted a non-project id")
		}
		if _, err := repo.DiscoverTools(ctx, "not-a-project", "custom"); err == nil {
			t.Error("DiscoverTools accepted a non-project id")
		}
		if _, err := repo.GetToolkit(ctx, "not-a-project", "1"); err == nil {
			t.Error("GetToolkit accepted a non-project id")
		}
		if _, err := repo.UpdateToolkit(ctx, "not-a-project", "1", nil); err == nil {
			t.Error("UpdateToolkit accepted a non-project id")
		}
		if err := repo.DeleteToolkit(ctx, "not-a-project", "1"); err == nil {
			t.Error("DeleteToolkit accepted a non-project id")
		}
		if _, err := repo.ForkToolkit(ctx, "not-a-project", nil); err == nil {
			t.Error("ForkToolkit accepted a non-project id")
		}
		if _, _, err := repo.ListToolkits(ctx, "not-a-project", 1, 10); err == nil {
			t.Error("ListToolkits accepted a non-project id")
		}
		if _, err := repo.ValidateToolkit(ctx, "not-a-project", "1"); err == nil {
			t.Error("ValidateToolkit accepted a non-project id")
		}
	})

	t.Run("the paged list counts every row and returns the page", func(t *testing.T) {
		rows, total, err := repo.ListToolkits(ctx, "1", 1, 10)
		if err != nil {
			t.Fatalf("ListToolkits: %v", err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("total=%d rows=%d, want 1 and 1", total, len(rows))
		}
		if rows[0]["name"] != "lifecycle-fixture" || rows[0]["type"] != "custom" {
			t.Errorf("row=%#v", rows[0])
		}
	})

	t.Run("a page past the end is empty and still counts the whole table", func(t *testing.T) {
		rows, total, err := repo.ListToolkits(ctx, "1", 9, 10)
		if err != nil {
			t.Fatalf("ListToolkits: %v", err)
		}
		if total != 1 || len(rows) != 0 {
			t.Errorf("total=%d rows=%d, want 1 and 0", total, len(rows))
		}
	})

	t.Run("the single read carries the sanitized toolkit name and the author", func(t *testing.T) {
		row, err := repo.GetToolkit(ctx, "1", toolkitID)
		if err != nil {
			t.Fatalf("GetToolkit: %v", err)
		}
		if row["name"] != "lifecycle-fixture" {
			t.Errorf("name=%#v", row["name"])
		}
		// toolkit_name is the identifier the RUNTIME addresses this toolkit's
		// tools by, so it is the runtime's rule that decides it: `_`, `.` and
		// `-` survive and the `.` folds into `_`
		// (internal/toolkitnaming.RuntimeName). This assertion used to read
		// "no `-` and no space", which passed for a route that kept
		// alphanumerics only and so reported a name nothing addresses.
		if name, _ := row["toolkit_name"].(string); name != "lifecycle-fixture" {
			t.Errorf("toolkit_name=%q, want the runtime identifier %q", name, "lifecycle-fixture")
		}
		author, _ := row["author"].(map[string]any)
		if author == nil {
			t.Fatalf("row carries no author: %#v", row)
		}
		// The join is a LEFT JOIN and this fixture has no user row, so the
		// author is present and empty rather than absent.
		if _, ok := author["email"]; !ok {
			t.Errorf("author=%#v", author)
		}
	})

	t.Run("an unknown toolkit is an error, not an empty row", func(t *testing.T) {
		if _, err := repo.GetToolkit(ctx, "1", "999999"); err == nil {
			t.Error("GetToolkit answered for an id that does not exist")
		}
	})

	t.Run("the update writes only the fields the body names", func(t *testing.T) {
		updated, err := repo.UpdateToolkit(ctx, "1", toolkitID, map[string]any{
			"description": "renamed by the lifecycle test",
		})
		if err != nil {
			t.Fatalf("UpdateToolkit: %v", err)
		}
		if updated["description"] != "renamed by the lifecycle test" {
			t.Errorf("updated=%#v", updated)
		}
		// The name was not in the body and must survive.
		row, err := repo.GetToolkit(ctx, "1", toolkitID)
		if err != nil {
			t.Fatalf("GetToolkit after update: %v", err)
		}
		if row["name"] != "lifecycle-fixture" {
			t.Errorf("the update overwrote a field the body did not name: %#v", row)
		}
	})

	t.Run("the settings update stores the new document", func(t *testing.T) {
		if _, err := repo.UpdateToolkit(ctx, "1", toolkitID, map[string]any{
			"settings": map[string]any{"selected_tools": []any{"one"}},
			"meta":     map[string]any{"origin": "updated"},
		}); err != nil {
			t.Fatalf("UpdateToolkit: %v", err)
		}
		row, err := repo.GetToolkit(ctx, "1", toolkitID)
		if err != nil {
			t.Fatalf("GetToolkit: %v", err)
		}
		settings, _ := row["settings"].(map[string]any)
		tools, _ := settings["selected_tools"].([]any)
		if len(tools) != 1 || tools[0] != "one" {
			t.Errorf("settings=%#v", settings)
		}
	})

	t.Run("the type discovery reads the stored type", func(t *testing.T) {
		tools, err := repo.DiscoverTools(ctx, "1", "custom")
		if err != nil {
			t.Fatalf("DiscoverTools: %v", err)
		}
		if len(tools) != 1 || tools[0].Type != "custom" {
			t.Fatalf("tools=%#v", tools)
		}
		// A type nothing stores is an empty list, and the list is non-nil so
		// the response encodes as [] rather than null.
		absent, err := repo.DiscoverTools(ctx, "1", "no_such_type")
		if err != nil || absent == nil || len(absent) != 0 {
			t.Errorf("absent=%#v err=%v", absent, err)
		}
	})

	t.Run("the attached-tool read joins the mapping table", func(t *testing.T) {
		// Nothing is attached yet, and that is an empty list rather than an
		// error: the join simply matches no row.
		attached, err := repo.AvailableTools(ctx, "1", toolkitID)
		if err != nil {
			t.Fatalf("AvailableTools: %v", err)
		}
		if attached == nil || len(attached) != 0 {
			t.Fatalf("attached=%#v", attached)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id)
			VALUES ($1, 'application', $2)`, 4242, toolkitID); err != nil {
			t.Fatalf("attach the fixture toolkit: %v", err)
		}
		attached, err = repo.AvailableTools(ctx, "1", "4242")
		if err != nil {
			t.Fatalf("AvailableTools after attaching: %v", err)
		}
		if len(attached) != 1 || attached[0].Name != "lifecycle-fixture" {
			t.Errorf("attached=%#v", attached)
		}
	})

	t.Run("validation passes a toolkit with no embedding model", func(t *testing.T) {
		valid, err := repo.ValidateToolkit(ctx, "1", toolkitID)
		if err != nil || !valid {
			t.Errorf("valid=%v err=%v", valid, err)
		}
	})

	t.Run("validation refuses a toolkit whose embedding model is gone", func(t *testing.T) {
		if _, err := repo.UpdateToolkit(ctx, "1", toolkitID, map[string]any{
			"settings": map[string]any{"embedding_model": "a-model-nobody-configured"},
		}); err != nil {
			t.Fatalf("UpdateToolkit: %v", err)
		}
		valid, err := repo.ValidateToolkit(ctx, "1", toolkitID)
		if valid || err == nil {
			t.Fatalf("valid=%v err=%v, want a refusal", valid, err)
		}
		// The message names the model the operator has to restore.
		if !strings.Contains(err.Error(), "a-model-nobody-configured") {
			t.Errorf("err=%v", err)
		}
		if _, err := repo.UpdateToolkit(ctx, "1", toolkitID, map[string]any{
			"settings": map[string]any{"selected_tools": []any{}},
		}); err != nil {
			t.Fatalf("restore the fixture settings: %v", err)
		}
	})

	t.Run("validation refuses an unknown toolkit", func(t *testing.T) {
		if valid, err := repo.ValidateToolkit(ctx, "1", "999999"); valid || err == nil {
			t.Errorf("valid=%v err=%v", valid, err)
		}
	})

	t.Run("the fork copies the row under a new name", func(t *testing.T) {
		forked, err := repo.ForkToolkit(ctx, "1", map[string]any{"source_id": toolkitID})
		if err != nil {
			t.Fatalf("ForkToolkit: %v", err)
		}
		if forked.ID == toolkitID {
			t.Error("the fork reused the source id")
		}
		if forked.Name != "lifecycle-fixture (copy)" || forked.Type != "custom" {
			t.Errorf("forked=%#v", forked)
		}
		if _, _, err := repo.ListToolkits(ctx, "1", 1, 10); err != nil {
			t.Fatalf("ListToolkits after the fork: %v", err)
		}
		if err := repo.DeleteToolkit(ctx, "1", forked.ID); err != nil {
			t.Fatalf("DeleteToolkit the fork: %v", err)
		}
	})

	t.Run("a fork of a source that does not exist is an error", func(t *testing.T) {
		if _, err := repo.ForkToolkit(ctx, "1", map[string]any{"source_id": "999999"}); err == nil {
			t.Error("ForkToolkit answered for a source that does not exist")
		}
	})

	t.Run("the delete removes the row", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM p_1.entity_tool_mapping WHERE tool_id = $1`, toolkitID); err != nil {
			t.Fatalf("detach the fixture toolkit: %v", err)
		}
		if err := repo.DeleteToolkit(ctx, "1", toolkitID); err != nil {
			t.Fatalf("DeleteToolkit: %v", err)
		}
		if _, err := repo.GetToolkit(ctx, "1", toolkitID); err == nil {
			t.Error("the toolkit is still readable after the delete")
		}
		_, total, err := repo.ListToolkits(ctx, "1", 1, 10)
		if err != nil {
			t.Fatalf("ListToolkits after the delete: %v", err)
		}
		if total != 0 {
			t.Errorf("total=%d after deleting every row", total)
		}
	})
}
