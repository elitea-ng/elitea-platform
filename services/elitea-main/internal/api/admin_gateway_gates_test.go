package api

// The /admin/gateway routes of ADR-0059 R0.3: one group-wide
// configuration.governance gate became one gate per sub-surface, each
// accepting a narrow permission OR configuration.governance.
//
// Every request goes through the production router (NewRouter), so the gate
// under test is the one router.go composes, not a hand-mounted copy. The
// resolver grants exactly the permissions a test names
// (platformGateResolver, admin_platform_gates_test.go).
//
// Two kinds of proof live here:
//
//   - The gate tables (no database). The legacy holder's answers are pinned
//     in testdata/admin_gateway_legacy.golden, captured on main before the
//     split: a holder of configuration.governance sees no change. A narrow
//     holder gets the legacy holder's answer on the routes of its name and the
//     gate's 403 everywhere else.
//   - The database tables (ELITEA_TEST_DATABASE_URL). A read holder gets 200
//     on every read and 403 on every write; an alerts manager saves the budget
//     alerts.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

const goldenAdminGatewayLegacy = "testdata/admin_gateway_legacy.golden"

// gatewayGateRoute is one route under /admin/gateway. class is the class of
// the permission its gate takes (read or write); narrow are the narrow names
// its gate accepts beside configuration.governance.
type gatewayGateRoute struct {
	method, path, body string
	class              string
	narrow             []string
}

const (
	gatewayRead  = "read"
	gatewayWrite = "write"
)

const gatewayPrefix = "/api/v2/admin/gateway"

var (
	catalogueView   = []string{adminaccess.LLMCatalogueView}
	catalogueManage = []string{adminaccess.LLMCatalogueManage}
	policyView      = []string{adminaccess.LLMPolicyView}
	// The governance writes admit either manager at the route; the handler
	// then decides by the kind of the row.
	policyManage = []string{adminaccess.LLMPolicyManage, adminaccess.LLMGuardrailsManage}
	usageView    = []string{adminaccess.LLMUsageView}
	alertsManage = []string{adminaccess.LLMAlertsManage}
)

// gatewayGateRoutes are every route under /admin/gateway, in the order of the
// golden file. The bodies are the smallest a handler decodes; a gate never
// reads them. The ids name rows that do not exist.
var gatewayGateRoutes = []gatewayGateRoute{
	// Usage and alerts.
	{http.MethodGet, gatewayPrefix + "/budget-alerts", "", gatewayRead, usageView},
	{http.MethodPut, gatewayPrefix + "/budget-alerts", `{"enabled":true}`, gatewayWrite, alertsManage},
	{http.MethodGet, gatewayPrefix + "/status", "", gatewayRead, usageView},
	{http.MethodGet, gatewayPrefix + "/usage", "", gatewayRead, usageView},
	{http.MethodGet, gatewayPrefix + "/logs", "", gatewayRead, usageView},
	// Policy. validate-cel is a POST that changes nothing: a read.
	{http.MethodGet, gatewayPrefix + "/governance", "", gatewayRead, policyView},
	{http.MethodPost, gatewayPrefix + "/governance/validate-cel", `{"cel":"true"}`, gatewayRead, policyView},
	{http.MethodPost, gatewayPrefix + "/governance", `{}`, gatewayWrite, policyManage},
	{http.MethodPut, gatewayPrefix + "/governance/00000000-0000-0000-0000-000000000001", `{}`, gatewayWrite, policyManage},
	{http.MethodDelete, gatewayPrefix + "/governance/00000000-0000-0000-0000-000000000001", "", gatewayWrite, policyManage},
	// Catalogue: prices.
	{http.MethodGet, gatewayPrefix + "/models", "", gatewayRead, catalogueView},
	{http.MethodPut, gatewayPrefix + "/models", `{}`, gatewayWrite, catalogueManage},
	{http.MethodDelete, gatewayPrefix + "/models/fixture", "", gatewayWrite, catalogueManage},
	{http.MethodGet, gatewayPrefix + "/metered-prices", "", gatewayRead, catalogueView},
	{http.MethodPut, gatewayPrefix + "/metered-prices", `{}`, gatewayWrite, catalogueManage},
	// Catalogue: global providers. check, revalidate and the model listing
	// use the stored credential: writes.
	{http.MethodGet, gatewayPrefix + "/providers", "", gatewayRead, catalogueView},
	{http.MethodPost, gatewayPrefix + "/providers", `{}`, gatewayWrite, catalogueManage},
	{http.MethodPut, gatewayPrefix + "/providers/999999", `{}`, gatewayWrite, catalogueManage},
	{http.MethodDelete, gatewayPrefix + "/providers/999999", "", gatewayWrite, catalogueManage},
	{http.MethodPost, gatewayPrefix + "/providers/999999/check", "", gatewayWrite, catalogueManage},
	{http.MethodPost, gatewayPrefix + "/providers/999999/revalidate", "", gatewayWrite, catalogueManage},
	{http.MethodPost, gatewayPrefix + "/providers/999999/models", "", gatewayWrite, catalogueManage},
	// Catalogue: platform models and the default model.
	{http.MethodGet, gatewayPrefix + "/platform_models", "", gatewayRead, catalogueView},
	{http.MethodPost, gatewayPrefix + "/platform_models", `{}`, gatewayWrite, catalogueManage},
	{http.MethodPut, gatewayPrefix + "/platform_models/999999", `{}`, gatewayWrite, catalogueManage},
	{http.MethodDelete, gatewayPrefix + "/platform_models/999999", "", gatewayWrite, catalogueManage},
	{http.MethodGet, gatewayPrefix + "/platform_models/999999/default_usage", "", gatewayRead, catalogueView},
	{http.MethodGet, gatewayPrefix + "/default_model?section=llm", "", gatewayRead, catalogueView},
	{http.MethodPut, gatewayPrefix + "/default_model?section=llm", `{"model_name":"fixture"}`, gatewayWrite, catalogueManage},
	{http.MethodDelete, gatewayPrefix + "/default_model?section=llm", "", gatewayWrite, catalogueManage},
}

// gatewayReadHolder holds the three read names of the gateway.
var gatewayReadHolder = []string{adminaccess.LLMCatalogueView, adminaccess.LLMPolicyView, adminaccess.LLMUsageView}

// gatewayNarrowNames are the seven names this story wires.
var gatewayNarrowNames = []string{
	adminaccess.LLMCatalogueView, adminaccess.LLMCatalogueManage,
	adminaccess.LLMPolicyView, adminaccess.LLMPolicyManage, adminaccess.LLMGuardrailsManage,
	adminaccess.LLMUsageView, adminaccess.LLMAlertsManage,
}

// gatewayAnswers drives every route and answers one line per route.
func gatewayAnswers(t *testing.T, router http.Handler) string {
	t.Helper()
	var b strings.Builder
	for _, route := range gatewayGateRoutes {
		rec := servePlatformGate(t, router, route.method, route.path, route.body)
		fmt.Fprintf(&b, "%s\t%s\t%d\n", route.method, route.path, rec.Code)
	}
	return b.String()
}

// TestGatewayRoutesAnswerTheLegacyHolderAsBefore pins every /admin/gateway
// route's answer to a holder of configuration.governance alone. The file was
// captured on main before the group gate was split; regenerate it with
// UPDATE_GOLDEN=1 only for a change that means to alter what a legacy holder
// sees.
func TestGatewayRoutesAnswerTheLegacyHolderAsBefore(t *testing.T) {
	got := gatewayAnswers(t, newPlatformGateRouter(t, adminaccess.ConfigurationGovernance))

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenAdminGatewayLegacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenAdminGatewayLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenAdminGatewayLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenAdminGatewayLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a holder of configuration.governance gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasSuffix(line, "\t403") {
			t.Errorf("%s: the legacy holder is refused", line)
		}
	}
}

// TestGatewayRoutesAdmitTheirNarrowNameOnly: a holder of one narrow name gets
// the legacy holder's answer on the routes of that name and the gate's 403 on
// every other route. A holder of nothing is refused everywhere.
func TestGatewayRoutesAdmitTheirNarrowNameOnly(t *testing.T) {
	legacy := newPlatformGateRouter(t, adminaccess.ConfigurationGovernance)
	for _, holder := range append([]string{""}, gatewayNarrowNames...) {
		router := newPlatformGateRouter(t, holder)
		for _, route := range gatewayGateRoutes {
			got := servePlatformGate(t, router, route.method, route.path, route.body).Code
			if !slices.Contains(route.narrow, holder) {
				if got != http.StatusForbidden {
					t.Errorf("holder %q: %s %s = %d, want 403", holder, route.method, route.path, got)
				}
				continue
			}
			want := servePlatformGate(t, legacy, route.method, route.path, route.body).Code
			if got != want || got == http.StatusForbidden {
				t.Errorf("holder %q: %s %s = %d, want the legacy holder's %d", holder, route.method, route.path, got, want)
			}
		}
	}
}

// TestGatewayReadHolderIsRefusedEveryWrite: a holder of the three read names
// passes every gate of a route classed read and is refused every route classed
// write, provider check and revalidate included.
func TestGatewayReadHolderIsRefusedEveryWrite(t *testing.T) {
	router := newPlatformGateRouter(t, gatewayReadHolder...)
	for _, route := range gatewayGateRoutes {
		got := servePlatformGate(t, router, route.method, route.path, route.body).Code
		if route.class == gatewayWrite && got != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 for a read holder", route.method, route.path, got)
		}
		if route.class == gatewayRead && got == http.StatusForbidden {
			t.Errorf("%s %s = 403, want a read holder admitted", route.method, route.path)
		}
	}
}

// TestGuardrailWriteTakesTheGuardrailsName drives the kind check through the
// production composition: the route gate admits a policy manager, and the
// handler refuses the guardrail by reading the gate's resolution. The check
// runs before the database, so no pool is needed: a guardrails manager passes
// it and reaches the store (which this router does not have).
func TestGuardrailWriteTakesTheGuardrailsName(t *testing.T) {
	const guardrail = `{"type":"guardrail","name":"g","data":{"guardrail":{"stage":"input",` +
		`"detectors":[{"type":"secrets","action":"block"}]}}}`
	for _, c := range []struct {
		holder  string
		refused bool
	}{
		{adminaccess.LLMPolicyManage, true},
		{adminaccess.LLMGuardrailsManage, false},
		{adminaccess.ConfigurationGovernance, false},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			path := gatewayPrefix + "/governance"
			if method == http.MethodPut {
				path += "/00000000-0000-0000-0000-000000000001"
			}
			got := servePlatformGate(t, newPlatformGateRouter(t, c.holder), method, path, guardrail).Code
			if c.refused != (got == http.StatusForbidden) {
				t.Errorf("holder %s: %s %s with a guardrail = %d, refused %v", c.holder, method, path, got, c.refused)
			}
		}
	}
}

/* ── with a database ───────────────────────────────────────────────────── */

// stubModelDefaults answers the platform default model routes with an empty
// value, so a read reaches 200 without the catalogue service.
type stubModelDefaults struct{}

func (stubModelDefaults) Get(
	_ context.Context, section configurationapp.CurrentModelSection,
) (configurationapp.PlatformModelDefaultView, error) {
	return configurationapp.PlatformModelDefaultView{Section: string(section)}, nil
}

func (stubModelDefaults) Set(context.Context, configurationapp.CurrentModelSection, string) error {
	return nil
}

func (stubModelDefaults) Clear(context.Context, configurationapp.CurrentModelSection) error {
	return nil
}

func (stubModelDefaults) Usage(
	_ context.Context, row configurationapp.PlatformModelDefaultModelRow,
) (configurationapp.PlatformModelDefaultUsage, error) {
	return configurationapp.PlatformModelDefaultUsage{ModelName: row.Name}, nil
}

func (stubModelDefaults) ReleaseDeletedPlatformModelDefault(
	context.Context, configurationapp.PlatformModelDefaultModelRow,
) (bool, error) {
	return false, nil
}

func (stubModelDefaults) ReleaseDeletedProjectModelDefaults(
	context.Context, configurationapp.PlatformModelDefaultModelRow,
) error {
	return nil
}

func (stubModelDefaults) SeedProjectModelDefault(context.Context, int64) error { return nil }

// newGatewayDatabase is a migrated database (ELITEA_TEST_DATABASE_URL) with
// one platform model in the public project, and the id of that model.
func newGatewayDatabase(t *testing.T) (*pgxpool.Pool, int) {
	t.Helper()
	if testpg.DSN(os.Getenv(credentialJourneyDatabaseURL)) == "" {
		t.Skipf("set %s to run the gateway gate tables on a database", credentialJourneyDatabaseURL)
	}
	pool := newCredentialJourneyPool(t)
	var modelID int
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO p_1.configuration
			(uuid, project_id, elitea_title, label, type, section, data, meta, shared,
			 status_ok, source, created_at)
		VALUES (gen_random_uuid(), 1, 'r03_model', 'r03_model', 'llm_model', 'llm',
			'{"name":"r03-model"}'::jsonb, '{}'::jsonb, true, true, 'user', now() AT TIME ZONE 'UTC')
		RETURNING id`).Scan(&modelID); err != nil {
		t.Fatalf("seed the platform model: %v", err)
	}
	return pool, modelID
}

// gatewayDatabaseRouter is the production router on pool with the
// one-permission-set resolver.
func gatewayDatabaseRouter(pool *pgxpool.Pool, granted ...string) chi.Router {
	return NewRouter(RouterConfig{
		Pool:                  pool,
		SkillsRepo:            struct{ v2skills.Repository }{},
		AuthValidator:         testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:    testPrincipalValidator{},
		PermissionResolver:    platformGateResolver{granted: granted},
		PlatformModelDefaults: stubModelDefaults{},
	})
}

// TestGatewayReadHolderReadsEveryRead is the database half of the read table:
// with the stores composed, every route classed read answers 200 to a holder
// of the three read names, and every route classed write answers 403,
// provider check and revalidate included.
func TestGatewayReadHolderReadsEveryRead(t *testing.T) {
	pool, modelID := newGatewayDatabase(t)
	router := gatewayDatabaseRouter(pool, gatewayReadHolder...)
	for _, route := range gatewayGateRoutes {
		path := strings.Replace(route.path, "/platform_models/999999/default_usage",
			fmt.Sprintf("/platform_models/%d/default_usage", modelID), 1)
		rec := servePlatformGate(t, router, route.method, path, route.body)
		want := http.StatusOK
		if route.class == gatewayWrite {
			want = http.StatusForbidden
		}
		if rec.Code != want {
			t.Errorf("%s %s = %d %s, want %d", route.method, path, rec.Code,
				strings.TrimSpace(rec.Body.String()), want)
		}
	}
}

// TestAlertsManagerSavesTheBudgetAlerts: a holder of only llm.alerts.manage
// saves the budget alerts, and is refused a governance rule write.
func TestAlertsManagerSavesTheBudgetAlerts(t *testing.T) {
	pool, _ := newGatewayDatabase(t)
	router := gatewayDatabaseRouter(pool, adminaccess.LLMAlertsManage)
	saved := servePlatformGate(t, router, http.MethodPut, gatewayPrefix+"/budget-alerts",
		`{"enabled":true,"threshold_pct":75}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), "75") {
		t.Fatalf("budget alerts save = %d %s, want 200 with the stored threshold", saved.Code, saved.Body)
	}
	rule := servePlatformGate(t, router, http.MethodPost, gatewayPrefix+"/governance",
		`{"type":"rate_limit","name":"r","data":{}}`)
	if rule.Code != http.StatusForbidden {
		t.Errorf("governance write = %d, want 403", rule.Code)
	}
}

// TestPolicyManagerCannotTouchAStoredGuardrail: against the real table, a
// policy manager is refused a guardrail create, an edit that turns a stored
// guardrail into a throttle rule and a delete of it, and still edits its own
// throttle rule; a guardrails manager creates and deletes the guardrail.
func TestPolicyManagerCannotTouchAStoredGuardrail(t *testing.T) {
	const guardrail = `{"type":"guardrail","name":"r03_guard","data":{"guardrail":{"stage":"input",` +
		`"detectors":[{"type":"secrets","action":"block"}]}},"enabled":true}`
	const throttle = `{"type":"rate_limit","name":"r03_throttle","data":{},"enabled":true}`
	pool, _ := newGatewayDatabase(t)
	guardrails := gatewayDatabaseRouter(pool, adminaccess.LLMGuardrailsManage)
	policy := gatewayDatabaseRouter(pool, adminaccess.LLMPolicyManage)
	rowID := func(rec interface{ String() string }) string {
		_, after, _ := strings.Cut(rec.String(), `"id":"`)
		id, _, _ := strings.Cut(after, `"`)
		return id
	}

	if got := servePlatformGate(t, policy, http.MethodPost, gatewayPrefix+"/governance", guardrail).Code; got != http.StatusForbidden {
		t.Errorf("guardrail create by a policy manager = %d, want 403", got)
	}
	created := servePlatformGate(t, guardrails, http.MethodPost, gatewayPrefix+"/governance", guardrail)
	if created.Code != http.StatusOK {
		t.Fatalf("guardrail create by a guardrails manager = %d %s, want 200", created.Code, created.Body)
	}
	guardID := rowID(created.Body)

	retype := servePlatformGate(t, policy, http.MethodPut, gatewayPrefix+"/governance/"+guardID, throttle)
	if retype.Code != http.StatusForbidden {
		t.Errorf("turning a guardrail into a throttle rule as a policy manager = %d %s, want 403", retype.Code, retype.Body)
	}
	if got := servePlatformGate(t, policy, http.MethodDelete, gatewayPrefix+"/governance/"+guardID, "").Code; got != http.StatusForbidden {
		t.Errorf("guardrail delete by a policy manager = %d, want 403", got)
	}
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT type FROM gateway.governance_config WHERE id::text = $1`, guardID).Scan(&stored); err != nil || stored != "guardrail" {
		t.Errorf("the guardrail after the refused writes: type %q, err %v; want it untouched", stored, err)
	}

	own := servePlatformGate(t, policy, http.MethodPost, gatewayPrefix+"/governance", throttle)
	if own.Code != http.StatusOK {
		t.Fatalf("throttle create by a policy manager = %d %s, want 200", own.Code, own.Body)
	}
	if got := servePlatformGate(t, policy, http.MethodDelete, gatewayPrefix+"/governance/"+rowID(own.Body), "").Code; got != http.StatusNoContent {
		t.Errorf("throttle delete by a policy manager = %d, want 204", got)
	}
	if got := servePlatformGate(t, guardrails, http.MethodDelete, gatewayPrefix+"/governance/"+guardID, "").Code; got != http.StatusNoContent {
		t.Errorf("guardrail delete by a guardrails manager = %d, want 204", got)
	}
}
