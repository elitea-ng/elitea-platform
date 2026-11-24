package repos

// DB action rules (shared/0271) against the migrated corpus: CRUD paths,
// per-project 256-rule cap under a concurrent burst, optimistic revision fence,
// audit row per write, SQL configuration ownership/type/dialect validation,
// FOR SHARE target-drift binding, start-time snapshot freezing, and claim-time
// credential materialization.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
)

const testDBActionRuleConfigID = int32(721)

var testDBActionRuleTarget = DBActionConfigurationTarget{Host: "warehouse.example.com", Port: 5432}

func newDBActionRulesTestRepo(t *testing.T) (*DBActionRulesRepo, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	if _, err := pool.Exec(dbActionRuleContext(t), `
INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id,created_at)
VALUES ($1,'60000000-0000-4000-8000-000000000721',1,'shared_sql','sql','credentials',
 '{"host":"warehouse.example.com","port":5432,"username":"etl_ro","password":"{{secret.WAREHOUSE_PASS}}"}'::jsonb,
 '{}'::jsonb,true,true,'user',7,now() AT TIME ZONE 'UTC')`,
		testDBActionRuleConfigID); err != nil {
		t.Fatal(err)
	}
	repo, err := NewDBActionRulesRepo(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo, pool
}

func testDBActionRule(name string) dbaction.Rule {
	return dbaction.Rule{
		Name:            name,
		ConfigurationID: testDBActionRuleConfigID,
		Dialect:         dbaction.DialectPostgres,
		DatabaseName:    "analytics",
		Mode:            dbaction.ModeRead,
		Enabled:         true,
	}
}

func dbActionRuleContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func dbAuditRows(t *testing.T, pool *pgxpool.Pool, ruleID int64) []struct {
	revision int64
	action   string
	actor    int64
	rule     dbaction.Rule
} {
	t.Helper()
	rows, err := pool.Query(dbActionRuleContext(t), `
SELECT revision, action, actor_id, rule_json FROM elitea_runtime.db_action_rule_audit
WHERE rule_id = $1 ORDER BY revision`, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		revision int64
		action   string
		actor    int64
		rule     dbaction.Rule
	}
	for rows.Next() {
		var row struct {
			revision int64
			action   string
			actor    int64
			rule     dbaction.Rule
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

func TestDBActionRulesRepoPostgres_CreateListGetUpdateAndAudit(t *testing.T) {
	repo, pool := newDBActionRulesTestRepo(t)
	ctx := dbActionRuleContext(t)

	second, err := repo.Create(ctx, 1, 7, testDBActionRule("zeta.query"), testDBActionRuleTarget)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	firstRule := testDBActionRule("alpha.load")
	firstRule.Mode = dbaction.ModeWrite
	first, err := repo.Create(ctx, 1, 7, firstRule, testDBActionRuleTarget)
	if err != nil {
		t.Fatalf("create write rule: %v", err)
	}
	if first.ID <= 0 || first.Revision != 1 || first.ProjectID != 1 || first.SchemaVersion != dbaction.RuleSchema ||
		first.MarkerTable != dbaction.DefaultMarkerTable || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatalf("created write rule = %+v", first)
	}

	listed, err := repo.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Name != "alpha.load" || listed[1].Name != "zeta.query" {
		t.Fatalf("list is not ordered by name: %+v", listed)
	}
	names, err := repo.ListNames(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != (DBActionRuleName{Name: "alpha.load", Dialect: "postgres", Mode: "write", Enabled: true}) {
		t.Fatalf("names = %+v", names)
	}

	got, err := repo.Get(ctx, 1, second.ID)
	if err != nil || got.Name != "zeta.query" || got.Revision != 1 || got.MarkerTable != "" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	edit := testDBActionRule("zeta.query")
	edit.Enabled = false
	edit.Mode = dbaction.ModeWrite
	edit.MarkerTable = "public.etl_markers"
	updated, err := repo.Update(ctx, 1, second.ID, 9, edit, 1, testDBActionRuleTarget)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 || updated.Enabled || updated.Mode != dbaction.ModeWrite || updated.MarkerTable != "public.etl_markers" {
		t.Fatalf("updated rule = %+v", updated)
	}

	audit := dbAuditRows(t, pool, second.ID)
	if len(audit) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(audit))
	}
	if audit[0].revision != 1 || audit[0].action != "create" || audit[0].actor != 7 || audit[0].rule.Mode != dbaction.ModeRead {
		t.Fatalf("create audit = %+v", audit[0])
	}
	if audit[1].revision != 2 || audit[1].action != "update" || audit[1].actor != 9 || audit[1].rule.MarkerTable != "public.etl_markers" || audit[1].rule.Enabled {
		t.Fatalf("update audit = %+v", audit[1])
	}
}

func TestDBActionRulesRepoPostgres_ConflictsScopingTargetDriftAndConfigurationAccess(t *testing.T) {
	repo, pool := newDBActionRulesTestRepo(t)
	ctx := dbActionRuleContext(t)
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id,created_at) VALUES
 (730,'60000000-0000-4000-8000-000000000730',1,'own_private_sql','sql','credentials','{"host":"priv.example.com","username":"u","password":"{{secret.P}}"}'::jsonb,'{}'::jsonb,false,true,'user',7,now() AT TIME ZONE 'UTC'),
 (731,'60000000-0000-4000-8000-000000000731',1,'other_private_sql','sql','credentials','{"host":"priv2.example.com","username":"u","password":"{{secret.P}}"}'::jsonb,'{}'::jsonb,false,true,'user',99,now() AT TIME ZONE 'UTC'),
 (732,'60000000-0000-4000-8000-000000000732',1,'wrong_type','openapi','credentials','{"host":"api.example.com"}'::jsonb,'{}'::jsonb,true,true,'user',7,now() AT TIME ZONE 'UTC'),
 (733,'60000000-0000-4000-8000-000000000733',1,'mysql_dialect_cfg','sql','credentials','{"host":"mysql.example.com","dialect":"mysql","username":"u","password":"{{secret.P}}"}'::jsonb,'{}'::jsonb,true,true,'user',7,now() AT TIME ZONE 'UTC'),
 (734,'60000000-0000-4000-8000-000000000734',1,'secret_username_cfg','sql','credentials','{"host":"warehouse.example.com","username":"{{secret.ADMIN}}","password":"{{secret.P}}"}'::jsonb,'{}'::jsonb,true,true,'user',7,now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	created, err := repo.Create(ctx, 1, 7, testDBActionRule("same.name"), testDBActionRuleTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, 1, 7, testDBActionRule("same.name"), testDBActionRuleTarget); !errors.Is(err, ErrDBActionRuleNameTaken) {
		t.Fatalf("duplicate create = %v, want name taken", err)
	}
	if _, err := repo.Update(ctx, 1, created.ID, 7, testDBActionRule("same.name"), 99, testDBActionRuleTarget); !errors.Is(err, ErrDBActionRuleStale) {
		t.Fatalf("stale update = %v, want stale", err)
	}
	if _, err := repo.Get(ctx, 2, created.ID); !errors.Is(err, ErrDBActionRuleNotFound) {
		t.Fatalf("cross-project get = %v, want not found", err)
	}

	// SEC-5E2-03: target drift between LookupConfigurationTarget and FOR SHARE write fails closed.
	driftedTarget := DBActionConfigurationTarget{Host: "other.example.com", Port: 5432}
	if _, err := repo.Create(ctx, 1, 7, testDBActionRule("drifted.create"), driftedTarget); !errors.Is(err, ErrDBActionRuleConfigurationInvalid) {
		t.Fatalf("drifted create = %v, want ErrDBActionRuleConfigurationInvalid", err)
	}
	if _, err := repo.Update(ctx, 1, created.ID, 7, testDBActionRule("same.name"), created.Revision, driftedTarget); !errors.Is(err, ErrDBActionRuleConfigurationInvalid) {
		t.Fatalf("drifted update = %v, want ErrDBActionRuleConfigurationInvalid", err)
	}

	// Own private SQL config accepted; default port 5432 resolved.
	own := testDBActionRule("own.private")
	own.ConfigurationID = 730
	target, err := repo.LookupConfigurationTarget(ctx, 1, 7, 730, dbaction.DialectPostgres)
	if err != nil || target.Host != "priv.example.com" || target.Port != 5432 {
		t.Fatalf("LookupConfigurationTarget = %+v, %v", target, err)
	}
	if _, err := repo.Create(ctx, 1, 7, own, target); err != nil {
		t.Fatalf("own private SQL config refused: %v", err)
	}

	// Other user's private, non-sql type, mismatched dialect, secret placeholder in username,
	// missing, and foreign project all refused.
	for _, tc := range []struct {
		label    string
		ruleName string
		cfgID    int32
	}{
		{"other user private", "bad.other_user_private", 731},
		{"non-sql type", "bad.non_sql_type", 732},
		{"dialect mismatch", "bad.dialect_mismatch", 733},
		{"secret placeholder in username", "bad.secret_username", 734},
		{"missing config", "bad.missing_config", 999},
	} {
		rule := testDBActionRule(tc.ruleName)
		rule.ConfigurationID = tc.cfgID
		if _, err := repo.Create(ctx, 1, 7, rule, testDBActionRuleTarget); !errors.Is(err, ErrDBActionRuleConfigurationInvalid) {
			t.Errorf("%s: err = %v, want ErrDBActionRuleConfigurationInvalid", tc.label, err)
		}
	}
	if _, err := repo.Create(ctx, 2, 7, testDBActionRule("foreign.cfg"), testDBActionRuleTarget); !errors.Is(err, ErrDBActionRuleConfigurationInvalid) {
		t.Fatalf("foreign project config err = %v, want ErrDBActionRuleConfigurationInvalid", err)
	}
}

func TestDBActionRulesRepoPostgres_ConcurrentBurstNeverExceedsTheLimit(t *testing.T) {
	repo, pool := newDBActionRulesTestRepo(t)
	ctx := dbActionRuleContext(t)
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
			_, err := repo.Create(ctx, 1, 7, testDBActionRule(fmt.Sprintf("burst_%03d", i)), testDBActionRuleTarget)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrDBActionRuleLimit):
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.db_action_rules WHERE project_id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.db_action_rule_audit WHERE project_id = 1`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if created != dbaction.MaxRulesPerProject || limited != burst-dbaction.MaxRulesPerProject ||
		stored != dbaction.MaxRulesPerProject || audited != dbaction.MaxRulesPerProject {
		t.Fatalf("created=%d limited=%d stored=%d audited=%d, want %d/%d/%d/%d",
			created, limited, stored, audited,
			dbaction.MaxRulesPerProject, burst-dbaction.MaxRulesPerProject, dbaction.MaxRulesPerProject, dbaction.MaxRulesPerProject)
	}
}

type stubDBUnsecreter struct {
	secrets map[string]string
}

func (s stubDBUnsecreter) Unsecret(_ context.Context, _ int32, data map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if str, ok := v.(string); ok {
			for placeholder, plain := range s.secrets {
				str = strings.ReplaceAll(str, placeholder, plain)
			}
			out[k] = str
			continue
		}
		out[k] = v
	}
	return out, nil
}

func TestDBActionCredentialsPostgres_FreezeAndMaterialize(t *testing.T) {
	repo, pool := newDBActionRulesTestRepo(t)
	ctx := dbActionRuleContext(t)

	seedAuthCoreTables(t, pool)
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS public.auth_core__role (
    id serial PRIMARY KEY, name varchar(64) NOT NULL, mode varchar(64) NOT NULL, UNIQUE (name, mode));
CREATE TABLE IF NOT EXISTS public.auth_core__role_permission (
    id serial PRIMARY KEY, role_id integer NOT NULL REFERENCES public.auth_core__role (id) ON DELETE CASCADE,
    permission varchar(64), UNIQUE (role_id, permission));
CREATE TABLE IF NOT EXISTS public.auth_core__project_role_permission (
    id serial PRIMARY KEY, project_id integer NOT NULL,
    role_id integer REFERENCES public.auth_core__project_role (id) ON UPDATE CASCADE ON DELETE CASCADE,
    permission text NOT NULL, UNIQUE (project_id, role_id, permission));
INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES
 (7, 'db-admin@test.local', 'db-admin', false),
 (8, 'db-outsider@test.local', 'db-outsider', false);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (9301, 1, 'admin');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES (1, 9301, 'configurations.configuration.details');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 7, 9301);
INSERT INTO p_1.configuration (id,uuid,project_id,elitea_title,type,section,data,meta,shared,status_ok,source,author_id,created_at)
VALUES (739,'60000000-0000-4000-8000-000000000739',1,'unsealed_sql','sql','credentials',
 '{"host":"warehouse.example.com","port":5432,"username":"etl_ro","password":"legacy-plaintext-password"}'::jsonb,
 '{}'::jsonb,true,true,'user',7,now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	created, err := repo.Create(ctx, 1, 7, testDBActionRule("warehouse.analytics"), testDBActionRuleTarget)
	if err != nil {
		t.Fatal(err)
	}
	unsealedRule := testDBActionRule("warehouse.unsealed")
	unsealedRule.ConfigurationID = 739
	if _, err := repo.Create(ctx, 1, 7, unsealedRule, testDBActionRuleTarget); err != nil {
		t.Fatal(err)
	}

	creds, err := NewDBActionCredentials(pool, stubDBUnsecreter{secrets: map[string]string{
		"{{secret.WAREHOUSE_PASS}}": "plaintext-db-secret-value",
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Authorized actor freezes sealed snapshot cleanly.
	snap, err := dbaction.FreezeSnapshot(ctx, 1, 7, 31, 41, []string{created.Name}, creds)
	if err != nil || snap == nil || len(snap.Rules) != 1 {
		t.Fatalf("FreezeSnapshot = %+v, %v", snap, err)
	}
	frozen := snap.Rules[0]
	if frozen.SQLConfiguration[configurationapp.CurrentFrozenConfigurationMarker] != true ||
		frozen.SQLConfiguration["password"] != "{{secret.WAREHOUSE_PASS}}" {
		t.Fatalf("frozen SQL config did not stay sealed with marker: %+v", frozen.SQLConfiguration)
	}

	// SEC-5E2-02: unsealed legacy plaintext password is refused at freeze time.
	if _, err := dbaction.FreezeSnapshot(ctx, 1, 7, 31, 41, []string{unsealedRule.Name}, creds); !errors.Is(err, dbaction.ErrPolicyDenied) {
		t.Fatalf("unsealed password FreezeSnapshot err = %v, want ErrPolicyDenied", err)
	}

	// Outsider without configurations.configuration.details is refused.
	if _, err := dbaction.FreezeSnapshot(ctx, 1, 8, 31, 41, []string{created.Name}, creds); !errors.Is(err, dbaction.ErrPolicyDenied) {
		t.Fatalf("outsider FreezeSnapshot err = %v, want ErrPolicyDenied", err)
	}

	// BUG-5E2-01: canceled context is preserved rather than masked as ErrUnavailable.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := creds.LoadFrozenRule(canceledCtx, 1, 7, created.Name); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled LoadFrozenRule err = %v, want context.Canceled", err)
	}

	// Claim-time materialization unsecrets password for the claimed Worker.
	materialized, err := creds.MaterializeSQLCredential(ctx, 1, frozen.Dialect, frozen.SQLConfiguration)
	if err != nil {
		t.Fatalf("MaterializeSQLCredential: %v", err)
	}
	defer materialized.Zero()
	if materialized.Host != "warehouse.example.com" || materialized.Port != 5432 || materialized.Username != "etl_ro" || materialized.Password() != "plaintext-db-secret-value" {
		t.Fatalf("unexpected materialized credential: host=%s port=%d user=%s", materialized.Host, materialized.Port, materialized.Username)
	}

	// Disabled rule is refused at freeze time.
	disabled := testDBActionRule("warehouse.analytics")
	disabled.Enabled = false
	if _, err := repo.Update(ctx, 1, created.ID, 7, disabled, created.Revision, testDBActionRuleTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := dbaction.FreezeSnapshot(ctx, 1, 7, 31, 41, []string{created.Name}, creds); !errors.Is(err, dbaction.ErrPolicyDenied) {
		t.Fatalf("disabled rule FreezeSnapshot err = %v, want ErrPolicyDenied", err)
	}
}
