package repos

// HTTP action rules (shared/0170) against the migrated corpus: the write
// paths, the per-project limit under a concurrent burst, the revision fence,
// the audit row of every write, and project scoping on every read and write.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
)

func newHTTPActionRulesTestRepo(t *testing.T) (*HTTPActionRulesRepo, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewHTTPActionRulesRepo(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo, pool
}

func testHTTPActionRule(name string) httpaction.Rule {
	return httpaction.Rule{
		Name: name, Origin: "https://api.example.com", PathTemplate: "/v1/items/{id}", Method: "GET",
		QueryNames: []string{"page"}, ConfigurationID: 1, Toolkit: "openapi", Tool: "get_item", Enabled: true,
	}
}

func httpActionRuleContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func auditRows(t *testing.T, pool *pgxpool.Pool, ruleID int64) []struct {
	revision int64
	action   string
	actor    int64
	rule     httpaction.Rule
} {
	t.Helper()
	rows, err := pool.Query(httpActionRuleContext(t), `
SELECT revision, action, actor_id, rule_json FROM elitea_runtime.http_action_rule_audit
WHERE rule_id = $1 ORDER BY revision`, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		revision int64
		action   string
		actor    int64
		rule     httpaction.Rule
	}
	for rows.Next() {
		var row struct {
			revision int64
			action   string
			actor    int64
			rule     httpaction.Rule
		}
		var body []byte
		if err := rows.Scan(&row.revision, &row.action, &row.actor, &body); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &row.rule); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHTTPActionRulesRepoPostgres_CreateListGetUpdateAndAudit(t *testing.T) {
	repo, pool := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)

	second, err := repo.Create(ctx, 1, 7, testHTTPActionRule("zeta.lookup"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first, err := repo.Create(ctx, 1, 7, testHTTPActionRule("alpha.lookup"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.ID <= 0 || first.Revision != 1 || first.ProjectID != 1 || first.SchemaVersion != httpaction.RuleSchema ||
		first.Replay != httpaction.ReplayNever || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatalf("created rule = %+v", first)
	}

	listed, err := repo.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Name != "alpha.lookup" || listed[1].Name != "zeta.lookup" {
		t.Fatalf("list is not ordered by name: %+v", listed)
	}
	names, err := repo.ListNames(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != (HTTPActionRuleName{Name: "alpha.lookup", Enabled: true}) {
		t.Fatalf("names = %+v", names)
	}

	got, err := repo.Get(ctx, 1, second.ID)
	if err != nil || got.Name != "zeta.lookup" || got.Revision != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}

	edit := testHTTPActionRule("zeta.lookup")
	edit.Enabled = false
	edit.Method = "POST"
	edit.Replay = httpaction.ReplayIdempotencyKey
	updated, err := repo.Update(ctx, 1, second.ID, 9, edit, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 || updated.Enabled || updated.Method != "POST" || !updated.UpdatedAt.After(second.UpdatedAt) {
		t.Fatalf("updated rule = %+v", updated)
	}
	var updatedBy, createdBy int64
	if err := pool.QueryRow(ctx, `SELECT created_by, updated_by FROM elitea_runtime.http_action_rules WHERE id = $1`,
		second.ID).Scan(&createdBy, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != 7 || updatedBy != 9 {
		t.Fatalf("created_by=%d updated_by=%d", createdBy, updatedBy)
	}

	audit := auditRows(t, pool, second.ID)
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want one per write", len(audit))
	}
	if audit[0].revision != 1 || audit[0].action != "create" || audit[0].actor != 7 || audit[0].rule.Method != "GET" {
		t.Fatalf("create audit = %+v", audit[0])
	}
	if audit[1].revision != 2 || audit[1].action != "update" || audit[1].actor != 9 ||
		audit[1].rule.Method != "POST" || audit[1].rule.Enabled || audit[1].rule.Revision != 2 {
		t.Fatalf("update audit = %+v", audit[1])
	}

	rule, err := readHTTPActionRule(ctx, pgxExecutor{queryer: pool}, 1, "zeta.lookup", false)
	if err != nil || rule.ID != second.ID || rule.Revision != 2 || rule.ProjectID != 1 || rule.SchemaVersion != httpaction.RuleSchema {
		t.Fatalf("readHTTPActionRule = %+v, %v", rule, err)
	}
	if _, err := readHTTPActionRule(ctx, pgxExecutor{queryer: pool}, 2, "zeta.lookup", false); !errors.Is(err, ErrHTTPActionRuleNotFound) {
		t.Fatalf("readHTTPActionRule in another project = %v, want not found", err)
	}
}

func TestHTTPActionRulesRepoPostgres_ReadHTTPActionRuleLocksForShare(t *testing.T) {
	repo, pool := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)
	created, err := repo.Create(ctx, 1, 7, testHTTPActionRule("locked.rule"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := readHTTPActionRule(ctx, pgxExecutor{queryer: tx}, 1, "locked.rule", true); err != nil {
		t.Fatal(err)
	}
	// An update must wait behind the share lock.
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := repo.Update(short, 1, created.ID, 7, testHTTPActionRule("locked.rule"), 1); err == nil {
		t.Fatal("an update passed a rule held FOR SHARE")
	}
}

func TestHTTPActionRulesRepoPostgres_ConflictsAndScoping(t *testing.T) {
	repo, _ := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)
	created, err := repo.Create(ctx, 1, 7, testHTTPActionRule("same.name"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, 1, 7, testHTTPActionRule("same.name")); !errors.Is(err, ErrHTTPActionRuleNameTaken) {
		t.Fatalf("duplicate create = %v, want name taken", err)
	}
	// The same name in another project is a different rule.
	if _, err := repo.Create(ctx, 2, 7, testHTTPActionRule("same.name")); err != nil {
		t.Fatalf("same name in project 2: %v", err)
	}
	other, err := repo.Create(ctx, 1, 7, testHTTPActionRule("other.name"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Update(ctx, 1, other.ID, 7, testHTTPActionRule("same.name"), 1); !errors.Is(err, ErrHTTPActionRuleNameTaken) {
		t.Fatalf("rename onto an existing name = %v, want name taken", err)
	}

	if _, err := repo.Update(ctx, 1, created.ID, 7, testHTTPActionRule("same.name"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Update(ctx, 1, created.ID, 7, testHTTPActionRule("same.name"), 1); !errors.Is(err, ErrHTTPActionRuleStale) {
		t.Fatalf("stale revision = %v, want stale", err)
	}

	// Another project's id answers exactly what a missing id answers.
	if _, err := repo.Get(ctx, 2, created.ID); !errors.Is(err, ErrHTTPActionRuleNotFound) {
		t.Fatalf("cross-project get = %v, want not found", err)
	}
	if _, err := repo.Update(ctx, 2, created.ID, 7, testHTTPActionRule("same.name"), 2); !errors.Is(err, ErrHTTPActionRuleNotFound) {
		t.Fatalf("cross-project update = %v, want not found", err)
	}
	if _, err := repo.Get(ctx, 1, created.ID+1000); !errors.Is(err, ErrHTTPActionRuleNotFound) {
		t.Fatalf("missing get = %v, want not found", err)
	}

	invalid := testHTTPActionRule("bad.origin")
	invalid.Origin = "http://api.example.com"
	if _, err := repo.Create(ctx, 1, 7, invalid); err == nil {
		t.Fatal("the repository stored a rule Validate refuses")
	}
}

// A row a newer validator refuses (here: a dot segment the column CHECK does
// not see) is never handed out as a rule.
func TestHTTPActionRulesRepoPostgres_ReadRevalidatesStoredRows(t *testing.T) {
	repo, pool := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)
	created, err := repo.Create(ctx, 1, 7, testHTTPActionRule("widened.rule"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.http_action_rules SET path_template = '/v1/../admin' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, 1, created.ID); !errors.Is(err, ErrHTTPActionRuleStoredInvalid) {
		t.Fatalf("get of a widened row = %v, want stored-invalid", err)
	}
	if _, err := readHTTPActionRule(ctx, pgxExecutor{queryer: pool}, 1, "widened.rule", false); !errors.Is(err, ErrHTTPActionRuleStoredInvalid) {
		t.Fatalf("readHTTPActionRule of a widened row = %v, want stored-invalid", err)
	}
}

func TestHTTPActionRulesRepoPostgres_LimitHoldsAtTheBoundary(t *testing.T) {
	repo, _ := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)
	for i := 0; i < httpaction.MaxRulesPerProject; i++ {
		if _, err := repo.Create(ctx, 1, 7, testHTTPActionRule(fmt.Sprintf("rule_%03d", i))); err != nil {
			t.Fatalf("create rule %d of %d: %v", i+1, httpaction.MaxRulesPerProject, err)
		}
	}
	if _, err := repo.Create(ctx, 1, 7, testHTTPActionRule("rule_overflow")); !errors.Is(err, ErrHTTPActionRuleLimit) {
		t.Fatalf("rule %d = %v, want the limit", httpaction.MaxRulesPerProject+1, err)
	}
	// The limit is per project.
	if _, err := repo.Create(ctx, 2, 7, testHTTPActionRule("rule_overflow")); err != nil {
		t.Fatalf("project 2 is not bounded by project 1's rules: %v", err)
	}
	listed, err := repo.List(ctx, 1)
	if err != nil || len(listed) != httpaction.MaxRulesPerProject {
		t.Fatalf("list = %d rules, %v", len(listed), err)
	}
}

func TestHTTPActionRulesRepoPostgres_ConcurrentBurstNeverExceedsTheLimit(t *testing.T) {
	repo, pool := newHTTPActionRulesTestRepo(t)
	ctx := httpActionRuleContext(t)
	const burst = 300
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		limited int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := repo.Create(ctx, 1, 7, testHTTPActionRule(fmt.Sprintf("burst_%03d", i)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrHTTPActionRuleLimit):
				limited++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other[0])
	}
	var stored, audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.http_action_rules WHERE project_id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.http_action_rule_audit WHERE project_id = 1`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if created != httpaction.MaxRulesPerProject || limited != burst-httpaction.MaxRulesPerProject ||
		stored != httpaction.MaxRulesPerProject || audited != httpaction.MaxRulesPerProject {
		t.Fatalf("created=%d limited=%d stored=%d audited=%d, want %d/%d/%d/%d", created, limited, stored, audited,
			httpaction.MaxRulesPerProject, burst-httpaction.MaxRulesPerProject, httpaction.MaxRulesPerProject, httpaction.MaxRulesPerProject)
	}
}
