package repos

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// newEntityFoldersFixture is the full legacy-shaped project schema (001_initial
// carries applications, skills and elitea_tools) plus the REAL tenant/0245 file
// applied to p_1, so the entity tables the folders point at exist.
func newEntityFoldersFixture(t *testing.T) (*ApplicationsRepo, *EntityFoldersRepo, *pgxpool.Pool) {
	t.Helper()
	apps, pool := newApplicationsTestRepo(t)
	raw, err := platformmigrations.Files.ReadFile("tenant/0245_project_entity_folders.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SET LOCAL search_path TO p_1`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("apply 0245: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return apps, NewEntityFoldersRepo(pool), pool
}

func folderNames(items []entityfolders.Folder) []string {
	names := []string{}
	for _, f := range items {
		names = append(names, f.Name)
	}
	return names
}

func TestEntityFoldersRepoPostgres_CreateRenameReorderAndKindScoping(t *testing.T) {
	_, repo, _ := newEntityFoldersFixture(t)
	ctx := testContext(t)

	first, err := repo.Create(ctx, "1", entityfolders.KindSkill, "Drafts", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(ctx, "1", entityfolders.KindSkill, "Shipped", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A new folder lands at the TOP, one gap above the maximum.
	if second.Position != first.Position+folders.PositionGap {
		t.Fatalf("positions %d then %d", first.Position, second.Position)
	}
	if first.CreatedBy != "1" {
		t.Fatalf("created_by = %q", first.CreatedBy)
	}
	items, err := repo.List(ctx, "1", entityfolders.KindSkill)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Shipped", "Drafts"}; !reflect.DeepEqual(folderNames(items), want) {
		t.Fatalf("order %v want %v", folderNames(items), want)
	}

	// Names are unique per kind, ignoring case; another kind may reuse one.
	_, err = repo.Create(ctx, "1", entityfolders.KindSkill, "drafts", nil)
	if apiStatus(err) != 409 {
		t.Fatalf("duplicate name: status %d err %v", apiStatus(err), err)
	}
	if _, err = repo.Create(ctx, "1", entityfolders.KindMCP, "Drafts", nil); err != nil {
		t.Fatalf("same name in another kind: %v", err)
	}
	other, _ := repo.List(ctx, "1", entityfolders.KindAgent)
	if len(other) != 0 {
		t.Fatalf("agent list sees skill folders: %v", other)
	}

	// A rename (no position) keeps the order; a position moves the folder.
	newName := "Archive"
	renamed, err := repo.Update(ctx, "1", entityfolders.KindSkill, first.ID, &newName, nil)
	if err != nil || renamed.Name != "Archive" || renamed.Position != first.Position {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	top := second.Position + folders.PositionGap
	if _, err = repo.Update(ctx, "1", entityfolders.KindSkill, first.ID, nil, &top); err != nil {
		t.Fatal(err)
	}
	items, _ = repo.List(ctx, "1", entityfolders.KindSkill)
	if want := []string{"Archive", "Shipped"}; !reflect.DeepEqual(folderNames(items), want) {
		t.Fatalf("after reorder %v want %v", folderNames(items), want)
	}
	// Renaming onto an existing name is a conflict, and a folder of another kind is not found.
	clash := "shipped"
	if _, err = repo.Update(ctx, "1", entityfolders.KindSkill, first.ID, &clash, nil); apiStatus(err) != 409 {
		t.Fatalf("rename clash: %v", err)
	}
	if _, err = repo.Update(ctx, "1", entityfolders.KindAgent, first.ID, &newName, nil); apiStatus(err) != 404 {
		t.Fatalf("wrong kind: %v", err)
	}

	rebalanced, err := repo.Rebalance(ctx, "1", entityfolders.KindSkill)
	if err != nil || len(rebalanced) != 2 || rebalanced[0].Name != "Archive" ||
		rebalanced[0].Position != 2*folders.PositionGap || rebalanced[1].Position != folders.PositionGap {
		t.Fatalf("rebalance: %+v %v", rebalanced, err)
	}
}

func TestEntityFoldersRepoPostgres_MoveValidatesKindAndDeleteReturnsItemsToRoot(t *testing.T) {
	apps, repo, pool := newEntityFoldersFixture(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "first@example.com")

	agent := createTestApplication(t, apps, "Helper", 1, &applications.Version{Name: "base", AgentType: "openai"})
	pipe := createTestApplication(t, apps, "Flow", 1, &applications.Version{Name: "base", AgentType: "pipeline"})
	exec := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, owner_id, meta, created_at) VALUES
		(501, 'github', 'plain', '', '{}'::jsonb, 1, 1, '{}'::jsonb, now()),
		(502, 'mcp', 'remote', '', '{}'::jsonb, 1, 1, '{}'::jsonb, now()),
		(503, 'custom', 'flagged', '', '{}'::jsonb, 1, 1, '{"mcp": true}'::jsonb, now())`)
	exec(`INSERT INTO p_1.skills (id, name, description, owner_id, author_id) VALUES (601, 'skill-a', '', 1, 1)`)

	mk := func(kind entityfolders.Kind, name string) entityfolders.Folder {
		f, err := repo.Create(ctx, "1", kind, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	agentFolder, pipeFolder := mk(entityfolders.KindAgent, "A"), mk(entityfolders.KindPipeline, "P")
	skillFolder, tkFolder, mcpFolder := mk(entityfolders.KindSkill, "S"), mk(entityfolders.KindToolkit, "T"), mk(entityfolders.KindMCP, "M")
	id := func(s string) int {
		n := 0
		for _, c := range s {
			n = n*10 + int(c-'0')
		}
		return n
	}
	move := func(kind entityfolders.Kind, entity int, folder *string) error {
		return repo.Move(ctx, "1", kind, entity, folder)
	}

	for _, ok := range []struct {
		kind   entityfolders.Kind
		entity int
		folder string
	}{
		{entityfolders.KindAgent, id(agent.ID), agentFolder.ID},
		{entityfolders.KindPipeline, id(pipe.ID), pipeFolder.ID},
		{entityfolders.KindSkill, 601, skillFolder.ID},
		{entityfolders.KindToolkit, 501, tkFolder.ID},
		{entityfolders.KindMCP, 502, mcpFolder.ID},
		{entityfolders.KindMCP, 503, mcpFolder.ID},
	} {
		folder := ok.folder
		if err := move(ok.kind, ok.entity, &folder); err != nil {
			t.Fatalf("move %s %d: %v", ok.kind, ok.entity, err)
		}
	}
	// An entity of another kind, a folder of another kind and a missing entity are all 404.
	for name, err := range map[string]error{
		"pipeline as agent":  move(entityfolders.KindAgent, id(pipe.ID), &agentFolder.ID),
		"agent as pipeline":  move(entityfolders.KindPipeline, id(agent.ID), &pipeFolder.ID),
		"mcp as toolkit":     move(entityfolders.KindToolkit, 502, &tkFolder.ID),
		"toolkit as mcp":     move(entityfolders.KindMCP, 501, &mcpFolder.ID),
		"agent into skill":   move(entityfolders.KindAgent, id(agent.ID), &skillFolder.ID),
		"missing entity":     move(entityfolders.KindSkill, 9999, &skillFolder.ID),
		"missing folder":     move(entityfolders.KindSkill, 601, strPtr("9999")),
		"non-numeric folder": move(entityfolders.KindSkill, 601, strPtr("abc")),
	} {
		if apiStatus(err) != 404 {
			t.Errorf("%s: status %d, err %v", name, apiStatus(err), err)
		}
	}

	items, _ := repo.List(ctx, "1", entityfolders.KindMCP)
	if len(items) != 1 || !reflect.DeepEqual(items[0].ItemIDs, []int{502, 503}) {
		t.Fatalf("mcp folder items: %+v", items)
	}

	// Moving again replaces the placement (one folder per entity).
	second := mk(entityfolders.KindSkill, "S2")
	if err := move(entityfolders.KindSkill, 601, &second.ID); err != nil {
		t.Fatal(err)
	}
	items, _ = repo.List(ctx, "1", entityfolders.KindSkill)
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Name] = len(f.ItemIDs)
	}
	if counts["S"] != 0 || counts["S2"] != 1 {
		t.Fatalf("after move: %v", counts)
	}
	// null takes it back to the root, and doing it twice is fine.
	for range 2 {
		if err := move(entityfolders.KindSkill, 601, nil); err != nil {
			t.Fatal(err)
		}
	}
	items, _ = repo.List(ctx, "1", entityfolders.KindSkill)
	for _, f := range items {
		if len(f.ItemIDs) != 0 {
			t.Fatalf("root move left %v in %s", f.ItemIDs, f.Name)
		}
	}

	// Deleting a folder returns its entities to the root and deletes none of them.
	if err := move(entityfolders.KindSkill, 601, &second.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "1", entityfolders.KindSkill, second.ID); err != nil {
		t.Fatal(err)
	}
	var skills, placements int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.skills WHERE id = 601`).Scan(&skills); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.project_folder_items WHERE entity_kind = 'skill'`).Scan(&placements); err != nil {
		t.Fatal(err)
	}
	if skills != 1 || placements != 0 {
		t.Fatalf("skills=%d placements=%d after folder delete", skills, placements)
	}
	if err := repo.Delete(ctx, "1", entityfolders.KindSkill, second.ID); apiStatus(err) != 404 {
		t.Fatalf("second delete: %v", err)
	}
	if err := repo.Delete(ctx, "1", entityfolders.KindAgent, skillFolder.ID); apiStatus(err) != 404 {
		t.Fatalf("delete through the wrong kind: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestApplicationsRepoPostgres_ListFiltersByProjectFolder(t *testing.T) {
	apps, repo, pool := newEntityFoldersFixture(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "first@example.com")

	a := createTestApplication(t, apps, "Alpha", 1, &applications.Version{Name: "base", AgentType: "openai"})
	b := createTestApplication(t, apps, "Beta", 1, &applications.Version{Name: "base", AgentType: "openai"})
	createTestApplication(t, apps, "Gamma", 1, &applications.Version{Name: "base", AgentType: "openai"})
	p := createTestApplication(t, apps, "Flow", 1, &applications.Version{Name: "base", AgentType: "pipeline"})
	agents, err := repo.Create(ctx, "1", entityfolders.KindAgent, "Mine", nil)
	if err != nil {
		t.Fatal(err)
	}
	pipes, err := repo.Create(ctx, "1", entityfolders.KindPipeline, "Flows", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		n := 0
		for _, c := range id {
			n = n*10 + int(c-'0')
		}
		if err := repo.Move(ctx, "1", entityfolders.KindAgent, n, &agents.ID); err != nil {
			t.Fatal(err)
		}
	}
	pn := 0
	for _, c := range p.ID {
		pn = pn*10 + int(c-'0')
	}
	if err := repo.Move(ctx, "1", entityfolders.KindPipeline, pn, &pipes.ID); err != nil {
		t.Fatal(err)
	}

	list := func(req applications.ListRequest) (int, []string) {
		t.Helper()
		req.ProjectID, req.SortBy, req.SortOrder = "1", "name", "asc"
		res, err := apps.List(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, row := range res.Rows {
			names = append(names, row.Name)
		}
		return res.Total, names
	}
	if total, names := list(applications.ListRequest{FolderID: agents.ID, AgentsType: "classic"}); total != 2 || !reflect.DeepEqual(names, []string{"Alpha", "Beta"}) {
		t.Fatalf("agent folder: %d %v", total, names)
	}
	if total, names := list(applications.ListRequest{FolderID: agents.ID, AgentsType: "classic", PageSize: 1}); total != 2 || !reflect.DeepEqual(names, []string{"Alpha"}) {
		t.Fatalf("agent folder, page 1 of 2: %d %v", total, names)
	}
	if total, names := list(applications.ListRequest{FolderID: pipes.ID, AgentsType: "pipeline"}); total != 1 || !reflect.DeepEqual(names, []string{"Flow"}) {
		t.Fatalf("pipeline folder: %d %v", total, names)
	}
	// The agents list never shows a pipeline folder's content, and a folder with no items is empty.
	if total, _ := list(applications.ListRequest{FolderID: pipes.ID, AgentsType: "classic"}); total != 0 {
		t.Fatalf("pipeline folder through the agents list: %d", total)
	}
	if total, names := list(applications.ListRequest{AgentsType: "classic"}); total != 3 {
		t.Fatalf("no folder filter: %d %v", total, names)
	}
	_, err = apps.List(ctx, applications.ListRequest{ProjectID: "1", FolderID: "1; DROP TABLE x"})
	if apiStatus(err) != 400 {
		t.Fatalf("bad folder id: status %d err %v", apiStatus(err), err)
	}
}

func TestEntityFoldersRepoPostgres_ItemIDsDropDeletedAndRekindedEntities(t *testing.T) {
	apps, repo, pool := newEntityFoldersFixture(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "first@example.com")
	gone := createTestApplication(t, apps, "Goner", 1, &applications.Version{Name: "base", AgentType: "openai"})
	stays := createTestApplication(t, apps, "Stays", 1, &applications.Version{Name: "base", AgentType: "openai"})
	becomes := createTestApplication(t, apps, "Becomes", 1, &applications.Version{Name: "base", AgentType: "openai"})
	folder, err := repo.Create(ctx, "1", entityfolders.KindAgent, "Mine", nil)
	if err != nil {
		t.Fatal(err)
	}
	num := func(id string) int {
		n := 0
		for _, c := range id {
			n = n*10 + int(c-'0')
		}
		return n
	}
	for _, a := range []applications.Application{gone, stays, becomes} {
		if err := repo.Move(ctx, "1", entityfolders.KindAgent, num(a.ID), &folder.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.applications WHERE id = $1`, num(gone.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.CreateVersion(ctx, "1", becomes.ID, applications.Version{Name: "flow", AgentType: "pipeline", AuthorID: 1}); err != nil {
		t.Fatal(err)
	}
	listed, err := repo.List(ctx, "1", entityfolders.KindAgent)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v %v", listed, err)
	}
	if want := []int{num(stays.ID)}; !reflect.DeepEqual(listed[0].ItemIDs, want) {
		t.Fatalf("item_ids %v, want only the live agent %v", listed[0].ItemIDs, want)
	}
}

func TestEntityFoldersRepoPostgres_OutOfRangeIdsAreNotFoundNotServerErrors(t *testing.T) {
	_, repo, _ := newEntityFoldersFixture(t)
	ctx := testContext(t)
	huge := "2147483648"
	name := "x"
	if _, err := repo.Update(ctx, "1", entityfolders.KindSkill, huge, &name, nil); apiStatus(err) != 404 {
		t.Fatalf("update: %v", err)
	}
	if err := repo.Delete(ctx, "1", entityfolders.KindSkill, huge); apiStatus(err) != 404 {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Move(ctx, "1", entityfolders.KindSkill, 1, &huge); apiStatus(err) == 500 || apiStatus(err) == 0 {
		t.Fatalf("move: %v", err)
	}
}

// The agents list, the pipelines list and the folder lists classify by one SQL
// fragment; this pins that an agent and a pipeline never overlap in any of them.
func TestApplicationKindClassificationAgreesBetweenListsAndFolders(t *testing.T) {
	apps, repo, pool := newEntityFoldersFixture(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "first@example.com")
	a := createTestApplication(t, apps, "Plain", 1, &applications.Version{Name: "base", AgentType: "openai"})
	p := createTestApplication(t, apps, "Flow", 1, &applications.Version{Name: "base", AgentType: "pipeline"})
	n := func(id string) int {
		v := 0
		for _, c := range id {
			v = v*10 + int(c-'0')
		}
		return v
	}
	fa, _ := repo.Create(ctx, "1", entityfolders.KindAgent, "A", nil)
	fp, _ := repo.Create(ctx, "1", entityfolders.KindPipeline, "P", nil)
	if err := repo.Move(ctx, "1", entityfolders.KindAgent, n(p.ID), &fa.ID); apiStatus(err) != 404 {
		t.Fatalf("pipeline as agent: %v", err)
	}
	if err := repo.Move(ctx, "1", entityfolders.KindPipeline, n(a.ID), &fp.ID); apiStatus(err) != 404 {
		t.Fatalf("agent as pipeline: %v", err)
	}
	for kind, want := range map[string]string{"classic": "Plain", "pipeline": "Flow"} {
		res, err := apps.List(ctx, applications.ListRequest{ProjectID: "1", AgentsType: kind, SortBy: "name", SortOrder: "asc"})
		if err != nil || res.Total != 1 || res.Rows[0].Name != want {
			t.Fatalf("%s list: %+v %v", kind, res, err)
		}
	}
}

// The ledgered corpus alone (what every real tenant migrates through) carries both tables.
func TestEntityFoldersMigrationIsInTheLedgeredCorpus(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	var folders, items *string
	if err := pool.QueryRow(testContext(t), `SELECT to_regclass('p_1.project_folders')::text, to_regclass('p_1.project_folder_items')::text`).Scan(&folders, &items); err != nil {
		t.Fatal(err)
	}
	if folders == nil || items == nil {
		t.Fatalf("corpus tables: %v %v", folders, items)
	}
}
