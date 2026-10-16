package gateway_test

// #322 acceptance: PUT /admin/gateway/budget-alerts persists, and what it
// persists is what the gateway reads.
//
// The old store passed every test it had — defaults, partial update, bounds —
// while writing to a process-local struct. So none of the assertions here are
// about the response body alone. Each one either re-reads through a SECOND
// store built over the same database (the restart / second-replica case) or
// reads the row through the gateway's own snapshot expression.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/gateway"
	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func newAlertPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_alerts_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		adminPool.Close()
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	// The REAL migration corpus, applied to an EMPTY database. A seeded or
	// dump-loaded database would already hold gateway.governance_config and
	// would hide a migration that never creates or seeds anything.
	gatewaySQL, err := infradb.GatewayMigrationSQL()
	if err != nil {
		t.Fatalf("load gateway migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, gatewaySQL); err != nil {
		t.Fatalf("apply gateway migrations: %v", err)
	}
	return pool
}

func alertDo(t *testing.T, pool *pgxpool.Pool, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := gateway.NewBudgetAlertHandler(gateway.NewBudgetAlertStore(pool))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/budget-alerts", strings.NewReader(body))
	h.Routes().ServeHTTP(rr, req)
	return rr
}

func decodeAlertConfig(t *testing.T, rr *httptest.ResponseRecorder) gateway.BudgetAlertConfig {
	t.Helper()
	var cfg gateway.BudgetAlertConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return cfg
}

// TestMigrationSeedsTheGlobalRow: a GET before any PUT answers with the same
// values the old in-process store returned, so upgrading changes nothing an
// operator can see except that the value now survives.
func TestMigrationSeedsTheGlobalRow(t *testing.T) {
	pool := newAlertPool(t)

	rr := alertDo(t, pool, http.MethodGet, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	cfg := decodeAlertConfig(t, rr)
	if !cfg.Enabled || cfg.ThresholdPct != gateway.DefaultSoftAlertThresholdPct {
		t.Fatalf("seeded config = %+v, want enabled at %d", cfg, gateway.DefaultSoftAlertThresholdPct)
	}
}

// TestUpdateSurvivesARestart is the defect. The second store is a fresh
// instance over the same database — the restart, and equally the second
// elitea-main replica that used to answer with its own divergent value.
func TestUpdateSurvivesARestart(t *testing.T) {
	pool := newAlertPool(t)

	rr := alertDo(t, pool, http.MethodPut, `{"enabled":false,"threshold_pct":55}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if cfg := decodeAlertConfig(t, rr); cfg.Enabled || cfg.ThresholdPct != 55 {
		t.Fatalf("PUT returned %+v, want {false 55}", cfg)
	}

	// A brand-new store, as a restarted or second replica would build.
	fresh := gateway.NewBudgetAlertStore(pool)
	cfg, err := fresh.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("a restarted replica reports alerts enabled; the operator's change was lost")
	}
	if cfg.ThresholdPct != 55 {
		t.Fatalf("threshold = %d, want the saved 55", cfg.ThresholdPct)
	}
}

// TestUpdateIsAPartialPatch: two operators changing different fields must not
// lose each other's write. The old read-modify-write in memory did not have to
// get this right because it never persisted.
func TestUpdateIsAPartialPatch(t *testing.T) {
	pool := newAlertPool(t)

	if rr := alertDo(t, pool, http.MethodPut, `{"threshold_pct":90}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	rr := alertDo(t, pool, http.MethodPut, `{"enabled":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	cfg := decodeAlertConfig(t, rr)
	if cfg.ThresholdPct != 90 {
		t.Fatalf("threshold = %d, want the 90 the first write saved", cfg.ThresholdPct)
	}
	if cfg.Enabled {
		t.Fatal("the second write did not take effect")
	}
}

// TestTheGatewaySeesTheSameValue closes the loop #322's second half is about:
// the config had no reader at all. This runs the gateway's OWN snapshot
// expression — the subquery from failmode/store.go globalAlertConfigSQL — over
// the row this API wrote, so a divergence in the three key columns
// (section/type/name) fails here rather than in production.
func TestTheGatewaySeesTheSameValue(t *testing.T) {
	pool := newAlertPool(t)

	if rr := alertDo(t, pool, http.MethodPut, `{"enabled":false,"threshold_pct":42}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var alertsDisabled bool
	var thresholdPct int
	if err := pool.QueryRow(context.Background(), gatewayAlertRead).Scan(&alertsDisabled, &thresholdPct); err != nil {
		t.Fatalf("the gateway's snapshot subquery found no row for the config this API wrote: %v", err)
	}
	if !alertsDisabled {
		t.Fatal("the gateway would still emit soft alerts after an operator turned them off")
	}
	if thresholdPct != 42 {
		t.Fatalf("the gateway would use threshold %d, not the saved 42", thresholdPct)
	}
}

// gatewayAlertRead is transcribed from
// services/elitea-llm-gateway/internal/failmode/store.go globalAlertConfigSQL.
// The gateway is outside go.work, so this cannot import the constant; the
// column names, the three key values and the cast guards are the contract
// between them.
const gatewayAlertRead = `SELECT
	NOT COALESCE(ga.alerts_enabled, true),
	COALESCE(ga.threshold_pct, 80)
FROM (
	SELECT CASE WHEN data->>'enabled' IN ('true','false')
	            THEN (data->>'enabled')::boolean END       AS alerts_enabled,
	       CASE WHEN data->>'threshold_pct' ~ '^[0-9]{1,3}$'
	             AND (data->>'threshold_pct')::int BETWEEN 1 AND 100
	            THEN (data->>'threshold_pct')::smallint END AS threshold_pct
	FROM gateway.governance_config
	WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'
	  AND enabled
) ga`

// TestAMalformedConfigRowDoesNotBreakTheLLMPath is the blast-radius test. The
// subquery above runs inside the budget snapshot on EVERY /llm call, and an
// unguarded cast over a value that is not a number raises 22P02 there — which
// fails the snapshot, which fails closed, which 503s every request for every
// project until somebody finds the row.
//
// The authoring API validates 1..100, but this JSONB column is reachable by
// direct SQL, so the hot path must not depend on that validation. Each case
// below is written straight into the row, as a bad migration or an operator at
// a psql prompt would.
func TestAMalformedConfigRowDoesNotBreakTheLLMPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "threshold is a word", data: `{"enabled": true, "threshold_pct": "abc"}`},
		{name: "threshold overflows smallint", data: `{"enabled": true, "threshold_pct": "99999"}`},
		{name: "threshold is out of range", data: `{"enabled": true, "threshold_pct": 0}`},
		{name: "threshold is a float", data: `{"enabled": true, "threshold_pct": 12.5}`},
		{name: "enabled is a word", data: `{"enabled": "sometimes", "threshold_pct": 42}`},
		{name: "both are junk", data: `{"enabled": [], "threshold_pct": {}}`},
		{name: "keys are absent", data: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newAlertPool(t)
			if _, err := pool.Exec(context.Background(), `
UPDATE gateway.governance_config SET data = $1::jsonb
WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'`, tc.data); err != nil {
				t.Fatal(err)
			}

			var alertsDisabled bool
			var thresholdPct int
			if err := pool.QueryRow(context.Background(), gatewayAlertRead).
				Scan(&alertsDisabled, &thresholdPct); err != nil {
				t.Fatalf("a malformed config row broke the gateway's budget snapshot: %v", err)
			}
			// A value that fails the guard falls back to the shipped default,
			// exactly as a missing key does — never to a zero that would read
			// as "alerts off, threshold 0".
			if thresholdPct < 1 || thresholdPct > 100 {
				t.Fatalf("threshold resolved to %d, want a usable percentage", thresholdPct)
			}
		})
	}
}

// TestTheGlobalThresholdReachesAProjectWithoutItsOwn is the half that migration
// 0084's DROP NOT NULL exists for. Under 0067 soft_alert_pct was NOT NULL
// DEFAULT 80, so "a project without its own threshold" was unrepresentable and
// the documented global default could never apply to anything.
func TestTheGlobalThresholdReachesAProjectWithoutItsOwn(t *testing.T) {
	pool := newAlertPool(t)
	ctx := context.Background()

	if rr := alertDo(t, pool, http.MethodPut, `{"threshold_pct":65}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	// A project that authored a limit but no threshold.
	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled, soft_alert_pct)
VALUES (901, 100, false, true, NULL)`); err != nil {
		t.Fatalf("a project budget with no authored threshold could not be stored: %v", err)
	}
	// ...and one that did author one.
	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled, soft_alert_pct)
VALUES (902, 100, false, true, 95)`); err != nil {
		t.Fatal(err)
	}

	const resolve = `SELECT COALESCE(pb.soft_alert_pct, ga.threshold_pct, 80)
		FROM gateway.project_budget pb
		LEFT JOIN (
			SELECT (data->>'threshold_pct')::smallint AS threshold_pct
			FROM gateway.governance_config
			WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'
			  AND enabled
		) ga ON true
		WHERE pb.project_id = $1`

	var inherited int
	if err := pool.QueryRow(ctx, resolve, 901).Scan(&inherited); err != nil {
		t.Fatal(err)
	}
	if inherited != 65 {
		t.Fatalf("a project with no authored threshold resolved to %d, want the global 65", inherited)
	}

	// The negative control: an authored threshold is NOT overridden by the
	// global default. Without this, a resolver that ignored the column entirely
	// would pass the test above.
	var authored int
	if err := pool.QueryRow(ctx, resolve, 902).Scan(&authored); err != nil {
		t.Fatal(err)
	}
	if authored != 95 {
		t.Fatalf("an authored threshold resolved to %d, want the project's own 95", authored)
	}
}

// gatewayStorePath is the gateway source the constant above is transcribed
// from. The gateway is a separate Go module (outside go.work, built with
// GOWORK=off), so this package cannot import it.
const gatewayStorePath = "../../../../elitea-llm-gateway/internal/failmode/store.go"

// TestTheTranscribedGatewayReadStillMatchesTheGateway is the test that keeps
// the test above honest.
//
// Everything else in this file proves that the SQL WRITTEN HERE is safe, which
// is worth nothing if the gateway's own SQL has since changed. That is the
// standing weakness of a transcribed contract across a module boundary, and
// this repo has shipped it before: a gate that reads a hardcoded path stops
// gating the moment the real thing moves, and reports success either way.
//
// So this reads the gateway source and asserts the two guards are still there.
// It deliberately checks for the GUARD, not for string equality with the whole
// constant: formatting churn should not fail a build, but deleting the guard
// must.
func TestTheTranscribedGatewayReadStillMatchesTheGateway(t *testing.T) {
	source, err := os.ReadFile(gatewayStorePath)
	if err != nil {
		// Not a skip. A missing file means the path is stale, and a stale path
		// is exactly how this class of gate stops gating without saying so.
		t.Fatalf("cannot read the gateway source this file transcribes (%s): %v", gatewayStorePath, err)
	}
	text := string(source)

	// The constant must still exist and still be joined into the snapshot.
	if !strings.Contains(text, "globalAlertConfigSQL") {
		t.Fatal("the gateway no longer declares globalAlertConfigSQL; the transcription above is stale")
	}
	for _, guard := range []string{
		`data->>'enabled' IN ('true','false')`,
		`data->>'threshold_pct' ~ '^[0-9]{1,3}$'`,
		`(data->>'threshold_pct')::int BETWEEN 1 AND 100`,
	} {
		if !strings.Contains(text, guard) {
			t.Fatalf("the gateway's budget snapshot lost the guard %q — a malformed config row would 503 every /llm call", guard)
		}
	}
}

// TestAWriteReEnablesTheRowItNeeds closes #322 through its other door.
//
// Both readers filter on `AND enabled`. The generic governance CRUD writes the
// same table and can set that column false on this row. The surface then
// behaves exactly as it did before #322: Get finds no row and answers the
// shipped defaults, Update writes into the invisible row and returns the
// operator's value from RETURNING, and the gateway reads nothing. The operator
// sees 200 OK and a changed GET for a setting with no effect.
//
// A write to this surface therefore has to assert that the row it needs is
// live. This test drives that state directly.
func TestAWriteReEnablesTheRowItNeeds(t *testing.T) {
	pool := newAlertPool(t)
	ctx := context.Background()

	// Somebody disabled the row through the generic governance surface.
	if _, err := pool.Exec(ctx, `
UPDATE gateway.governance_config SET enabled = false
WHERE section = 'governance' AND type = 'budget_alert' AND name = 'global'`); err != nil {
		t.Fatal(err)
	}

	// The operator turns soft alerts off and gets a 200.
	rr := alertDo(t, pool, http.MethodPut, `{"enabled":false,"threshold_pct":33}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	// The gateway must now see exactly what the operator saved. Before the fix
	// this read found no row, so it fell back to "alerts on, threshold 80" —
	// the 200 above was for a value nothing read.
	var alertsDisabled bool
	var thresholdPct int
	if err := pool.QueryRow(ctx, gatewayAlertRead).Scan(&alertsDisabled, &thresholdPct); err != nil {
		t.Fatalf("the gateway cannot see the row the operator just wrote: %v", err)
	}
	if !alertsDisabled {
		t.Fatal("the gateway would still emit soft alerts; the write reported success and changed nothing")
	}
	if thresholdPct != 33 {
		t.Fatalf("the gateway would use threshold %d, not the saved 33", thresholdPct)
	}

	// And the surface's own read agrees with the gateway.
	cfg, err := gateway.NewBudgetAlertStore(pool).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.ThresholdPct != 33 {
		t.Fatalf("GET reports %+v, want {false 33}", cfg)
	}
}
