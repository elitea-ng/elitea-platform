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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
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
			INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, created_at)
			VALUES ($1, 'application', $2, now() AT TIME ZONE 'UTC')`, 4242, toolkitID); err != nil {
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

	// Issue 1968: a source already at the 128-character limit still forks, and the " (copy)"
	// suffix stays inside the limit.
	t.Run("the fork of a name at the limit stays inside it", func(t *testing.T) {
		longName := strings.Repeat("字", 128)
		if _, err := pool.Exec(ctx, `UPDATE p_1.elitea_tools SET name = $1 WHERE id = $2`, longName, toolkitID); err != nil {
			t.Fatalf("lengthen the fixture name: %v", err)
		}
		defer func() {
			if _, err := pool.Exec(ctx, `UPDATE p_1.elitea_tools SET name = 'lifecycle-fixture' WHERE id = $1`, toolkitID); err != nil {
				t.Fatalf("restore the fixture name: %v", err)
			}
		}()
		forked, err := repo.ForkToolkit(ctx, "1", map[string]any{"source_id": toolkitID})
		if err != nil {
			t.Fatalf("ForkToolkit: %v", err)
		}
		want := strings.Repeat("字", 128-len(" (copy)")) + " (copy)"
		if forked.Name != want {
			t.Errorf("forked name = %q (%d characters), want %d characters ending in (copy)", forked.Name, utf8.RuneCountInString(forked.Name), 128)
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

// instanceSeed is one elitea_tools row of the filter fixture. isMCP and isApp
// are the EXPECTED classification, written by hand from the web rule
// (selectors.ts isMcpToolkit), so the SQL predicate is checked against an
// independent statement of it rather than against itself.
type instanceSeed struct {
	name, typ, desc string
	meta            string // "" stores SQL NULL
	isMCP, isApp    bool
}

func instanceFilterFixture() []instanceSeed {
	var seeds []instanceSeed
	// 25 plain toolkits that are NEWER than every MCP (the seed loop stamps
	// earlier seeds later, see instanceSeedCreatedAt), so the first page of an
	// unfiltered newest-first list holds no MCP at all (the picker regression). Every third
	// has a NULL meta: NOT(mcp predicate) must keep a NULL meta row.
	for i := 0; i < 25; i++ {
		seed := instanceSeed{name: fmt.Sprintf("a-tool-%02d", i), typ: "github", meta: `{}`}
		if i%3 == 0 {
			seed.meta = ""
		}
		seeds = append(seeds, seed)
	}
	seeds = append(seeds,
		// The three MCP shapes.
		instanceSeed{name: "mcp-type", typ: "mcp", meta: `{}`, isMCP: true},
		instanceSeed{name: "mcp-prefix", typ: "mcp_context7", desc: "Library docs", meta: `{}`, isMCP: true},
		instanceSeed{name: "mcp-meta", typ: "github", meta: `{"mcp": true}`, isMCP: true},
		// Duplicate names, on both sides of the split.
		instanceSeed{name: "dup-mcp", typ: "mcp", meta: `{}`, isMCP: true},
		instanceSeed{name: "dup-mcp", typ: "mcp_x", meta: `{}`, isMCP: true},
		instanceSeed{name: "dup-mcp", typ: "github", meta: `{"mcp": true}`, isMCP: true},
		instanceSeed{name: "dup-tool", typ: "jira", meta: `{}`},
		instanceSeed{name: "dup-tool", typ: "jira", meta: `{}`},
		// Near misses the web rule treats as NOT MCP.
		instanceSeed{name: "near-no-underscore", typ: "mcpxctx", meta: `{}`},           // `_` is not a wildcard
		instanceSeed{name: "near-meta-string", typ: "github", meta: `{"mcp": "true"}`}, // string, not boolean
		instanceSeed{name: "near-meta-false", typ: "github", meta: `{"mcp": false}`},
		// Agent-as-tool links: never in a typed listing, in the raw one.
		instanceSeed{name: "a-tool-05-agent", typ: "application", meta: `{}`, isApp: true},
		instanceSeed{name: "mcp-agent-link", typ: "application", meta: `{"mcp": true}`, isApp: true},
		instanceSeed{name: "zz-agent-link", typ: "application", meta: ``, isApp: true},
		instanceSeed{name: "dup-tool", typ: "application", meta: `{}`, isApp: true},
		// Text-search targets.
		instanceSeed{name: "100% done", typ: "jira", desc: "wildcard percent", meta: `{}`},
		instanceSeed{name: "snake_case", typ: "jira", desc: "wildcard underscore", meta: `{}`},
		instanceSeed{name: "snakeXcase", typ: "jira", desc: "not a literal underscore", meta: `{}`},
		instanceSeed{name: "back\\slash", typ: "jira", desc: "wildcard backslash", meta: `{}`},
		instanceSeed{name: "plain-name", typ: "jira", desc: "Mentions QUARTZ Here", meta: `{}`},
		instanceSeed{name: "Quartz-Cap", typ: "mcp", meta: `{}`, isMCP: true},
	)
	return seeds
}

// instanceSeedCreatedAt stamps seed i: earlier seeds are newer, and seeds
// come in pairs that share one instant, so the newest-first order has
// created_at ties for id to break.
func instanceSeedCreatedAt(base time.Time, i int) time.Time {
	return base.Add(-time.Duration(i/2) * time.Minute)
}

func TestToolkitInstanceListFiltersAgainstPostgres(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	repo := &pgRepo{pool: pool}

	seeds := instanceFilterFixture()
	byID := map[string]instanceSeed{}
	createdAt := map[string]time.Time{}
	base := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for i, seed := range seeds {
		var meta any
		if seed.meta != "" {
			meta = seed.meta
		}
		var id int
		stamp := instanceSeedCreatedAt(base, i)
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id, meta, created_at)
			VALUES ($1, $2, NULLIF($3, ''), 1, 1, $4::jsonb, $5) RETURNING id`,
			seed.name, seed.typ, seed.desc, meta, stamp).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", seed.name, err)
		}
		byID[strconv.Itoa(id)] = seed
		createdAt[strconv.Itoa(id)] = stamp
	}

	boolRef := func(v bool) *bool { return &v }
	contains := func(haystack, needle string) bool {
		return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
	}
	// want returns the ids the filter must select, by the hand-written
	// classification above.
	want := func(mcp *bool, query string) map[string]bool {
		ids := map[string]bool{}
		for id, seed := range byID {
			if mcp != nil && (seed.isApp || seed.isMCP != *mcp) {
				continue
			}
			if query != "" && !contains(seed.name, query) && !contains(seed.desc, query) {
				continue
			}
			ids[id] = true
		}
		return ids
	}
	// readAll pages with the given size until a short page, the way the picker
	// does, and returns the ids in the order served plus the first total.
	readAll := func(t *testing.T, filter InstanceListFilter, size int) ([]string, int) {
		t.Helper()
		var served []string
		firstTotal := -1
		for page := 1; page < 100; page++ {
			rows, total, err := repo.ListToolkitInstances(ctx, "1", filter, page, size)
			if err != nil {
				t.Fatalf("ListToolkitInstances page %d: %v", page, err)
			}
			if firstTotal == -1 {
				firstTotal = total
			}
			if total != firstTotal {
				t.Fatalf("total changed from %d to %d between pages", firstTotal, total)
			}
			if len(rows) > size {
				t.Fatalf("page %d holds %d rows, more than the size %d", page, len(rows), size)
			}
			for _, row := range rows {
				served = append(served, row["id"].(string))
			}
			if len(rows) < size {
				break
			}
		}
		return served, firstTotal
	}

	cases := []struct {
		name  string
		mcp   *bool
		query string
	}{
		{"no filter", nil, ""},
		{"mcp=true", boolRef(true), ""},
		{"mcp=false", boolRef(false), ""},
		{"query matches the name, any case", nil, "QUARTZ"},
		{"query matches the description, any case", nil, "quartz here"},
		{"mcp=true with a query", boolRef(true), "quartz"},
		{"mcp=false with a query", boolRef(false), "quartz"},
		{"a percent is literal", nil, "%"},
		{"an underscore is literal", nil, "_"},
		{"a backslash is literal", nil, `\`},
		{"nothing matches", nil, "zzz-no-such-toolkit"},
	}
	for _, testCase := range cases {
		for _, size := range []int{7, 20} {
			t.Run(fmt.Sprintf("%s, size %d: every row once, filtered total", testCase.name, size), func(t *testing.T) {
				filter := InstanceListFilter{MCP: testCase.mcp, Query: testCase.query}
				served, total := readAll(t, filter, size)
				expected := want(testCase.mcp, testCase.query)

				if total != len(expected) {
					t.Errorf("total=%d, want the filtered count %d", total, len(expected))
				}
				seen := map[string]bool{}
				for _, id := range served {
					if seen[id] {
						t.Errorf("row %s (%s) served twice across pages", id, byID[id].name)
					}
					seen[id] = true
					if !expected[id] {
						t.Errorf("row %s (%q, type %q) must not match this filter", id, byID[id].name, byID[id].typ)
					}
				}
				for id := range expected {
					if !seen[id] {
						t.Errorf("row %s (%q, type %q) never served", id, byID[id].name, byID[id].typ)
					}
				}
			})
		}
	}

	t.Run("a repeated type filter keeps exactly those types and pages past the page size (#2189)", func(t *testing.T) {
		types := []string{seeds[0].typ, "mcp"}
		expected := map[string]bool{}
		for id, seed := range byID {
			if seed.typ == types[0] || seed.typ == types[1] {
				expected[id] = true
			}
		}
		if len(expected) < 2 {
			t.Fatalf("fixture error: types %v match %d rows", types, len(expected))
		}
		served, total := readAll(t, InstanceListFilter{Types: types}, 3)
		if total != len(expected) || len(served) != len(expected) {
			t.Fatalf("total=%d served=%d, want %d", total, len(served), len(expected))
		}
		for _, id := range served {
			if !expected[id] {
				t.Errorf("row %s (type %q) is not one of %v", id, byID[id].typ, types)
			}
		}
		if _, none, err := repo.ListToolkitInstances(ctx, "1", InstanceListFilter{Types: []string{"no-such-type"}}, 1, 20); err != nil || none != 0 {
			t.Fatalf("an unknown type must match nothing: total=%d err=%v", none, err)
		}
	})

	t.Run("newest first, id breaks a created_at tie, so a new toolkit is on page one", func(t *testing.T) {
		served, _ := readAll(t, InstanceListFilter{}, 5)
		// The order, stated independently of the SQL: created_at descending,
		// then id descending.
		expected := make([]string, 0, len(createdAt))
		for id := range createdAt {
			expected = append(expected, id)
		}
		sort.Slice(expected, func(a, b int) bool {
			ta, tb := createdAt[expected[a]], createdAt[expected[b]]
			if !ta.Equal(tb) {
				return ta.After(tb)
			}
			ia, _ := strconv.Atoi(expected[a])
			ib, _ := strconv.Atoi(expected[b])
			return ia > ib
		})
		if strings.Join(served, ",") != strings.Join(expected, ",") {
			t.Fatalf("served order %v, want newest first then id descending %v", served, expected)
		}
		again, _ := readAll(t, InstanceListFilter{}, 5)
		if strings.Join(served, ",") != strings.Join(again, ",") {
			t.Error("two reads served the rows in a different order")
		}

		// The e2e regression: a project holding more than a page of toolkits,
		// one of them created just now with a name that sorts last. It must be
		// the first row of page one, not on whichever page its name lands.
		var newID int
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.elitea_tools (name, type, owner_id, author_id, meta, created_at)
			VALUES ('zzzz-just-created', 'github', 1, 1, '{}'::jsonb, $1) RETURNING id`,
			base.Add(time.Hour)).Scan(&newID); err != nil {
			t.Fatalf("seed the new toolkit: %v", err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `DELETE FROM p_1.elitea_tools WHERE id = $1`, newID); err != nil {
				t.Errorf("remove the new toolkit: %v", err)
			}
		})
		for _, filter := range []InstanceListFilter{{}, {MCP: boolRef(false)}} {
			rows, total, err := repo.ListToolkitInstances(ctx, "1", filter, 1, 20)
			if err != nil {
				t.Fatal(err)
			}
			if total <= 20 {
				t.Fatalf("fixture error: total %d fits on one page of 20", total)
			}
			if len(rows) == 0 {
				t.Fatalf("filter %+v: page one is empty", filter)
			}
			if rows[0]["id"] != strconv.Itoa(newID) {
				t.Errorf("filter %+v: page one starts with %v, want the toolkit created just now (id %d)", filter, rows[0]["name"], newID)
			}
		}
	})

	t.Run("page one of the raw list holds no MCP, which is why the picker filters on the server", func(t *testing.T) {
		rows, _, err := repo.ListToolkitInstances(ctx, "1", InstanceListFilter{}, 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if byID[row["id"].(string)].isMCP {
				t.Fatalf("fixture error: %q is an MCP on the first raw page", row["name"])
			}
		}
		mcpRows, mcpTotal, err := repo.ListToolkitInstances(ctx, "1", InstanceListFilter{MCP: boolRef(true)}, 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if mcpTotal != len(want(boolRef(true), "")) || len(mcpRows) != mcpTotal {
			t.Errorf("mcp=true page one: rows=%d total=%d, want every MCP (%d)", len(mcpRows), mcpTotal, len(want(boolRef(true), "")))
		}
	})

	t.Run("the raw list still serves the application rows", func(t *testing.T) {
		served, _ := readAll(t, InstanceListFilter{}, 20)
		apps := 0
		for _, id := range served {
			if byID[id].isApp {
				apps++
			}
		}
		if apps != 4 {
			t.Errorf("raw list served %d application rows, want 4", apps)
		}
		// ListToolkits is the same call with the empty filter.
		_, total, err := repo.ListToolkits(ctx, "1", 1, 5)
		if err != nil || total != len(seeds) {
			t.Errorf("ListToolkits total=%d err=%v, want %d", total, err, len(seeds))
		}
	})
}
