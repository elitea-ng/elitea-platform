package budgets_test

// #246 acceptance for the budgets / usage surface.
//
// What these tests are guarding against is not a wrong status code. This repo
// has repeatedly shipped routes that answered 200 and changed nothing (#128,
// #130) or persisted a policy nothing read (#218), and a test that asserted
// "200" would have passed against every one of them.
//
// So each case here re-reads what it wrote, and the re-read is chosen to be a
// place the handler could not have faked:
//
//   - the project-budget write is checked against `is_unlimited` and
//     `hard_limit_usd` in gateway.project_budget — the two columns the LLM
//     GATEWAY reads (services/elitea-llm-gateway/internal/failmode/store.go).
//     A handler that stored the authored fields but not the derived
//     enforcement flag would answer correctly and enforce nothing.
//   - spend is checked against a row planted in gateway.llm_budget_accumulators
//     exactly as elitea-scheduler's budgetwriteback consumer would write it,
//     and against a row in a DIFFERENT period that must not be counted.
//   - the per-member limit is checked to report `enforced: true`, which is the
//     claim the package makes about the platform rather than about itself, and
//     which the gateway's own per-member admission test (llmproxy
//     budget_member_test.go) proves on the other side of the boundary.
//
// The schema is the REAL migration (db.GatewayMigrationSQL) rather than a
// hand-copied DDL, so a schema change cannot pass here and fail in production.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/budgets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

const (
	budgetProjectID  = 21
	budgetOtherID    = 22
	budgetAdminUser  = 501
	budgetMemberUser = 502
	budgetOutsider   = 503
)

// fixedNow pins the reporting period. Without it a run that straddles midnight
// on the last of the month writes into one period and reads from the next — the
// shape that made the audit-seed E2E fail at midnight.
var fixedNow = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

func periodStart() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) }
func periodEnd() time.Time   { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }

/* ── harness ───────────────────────────────────────────────────────────── */

// budgetsRouter mounts the routes exactly as internal/api/router.go does, minus
// the permission middleware: the gate layer is a router concern and needs the
// auth stack these tests do not build. The IN-HANDLER authorisation — the
// ownership check on the member read, the admin-only member listing, the
// amount redaction — is this package's own and IS exercised below.
func budgetsRouter(h *handler.Handler) chi.Router {
	router := chi.NewRouter()
	router.Mount("/", h.Routes())
	return router
}

// asUser attaches an authenticated identity, which the project-scoped reads
// need in order to answer "is this you, and are you an admin here?".
func asUser(request *http.Request, userID int) *http.Request {
	user := auth.User{UserID: fmt.Sprint(userID)}
	return request.WithContext(auth.ContextWithUser(request.Context(), user))
}

func budgetsDo(t *testing.T, router chi.Router, method, target string, userID int, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	if userID > 0 {
		request = asUser(request, userID)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// decodeMap reads a response as a map so a test can assert on a field being
// ABSENT, which a struct with omitempty could never distinguish from zero.
func decodeMap(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(recorder.Body.Bytes()))
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, want, recorder.Body.String())
	}
}

// wantNumber asserts an exact decimal, as a string, so a value that survived a
// float64 round trip (0.10000000000000001) fails rather than "nearly" passing.
func wantNumber(t *testing.T, payload map[string]any, field, want string) {
	t.Helper()
	value, present := payload[field]
	if !present {
		t.Fatalf("%s is absent from %v", field, payload)
	}
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON number", field, value)
	}
	if number.String() != want {
		t.Fatalf("%s = %s, want %s", field, number, want)
	}
}

func wantNull(t *testing.T, payload map[string]any, field string) {
	t.Helper()
	value, present := payload[field]
	if !present {
		t.Fatalf("%s is absent, want present-and-null", field)
	}
	if value != nil {
		t.Fatalf("%s = %#v, want null", field, value)
	}
}

func wantAbsent(t *testing.T, payload map[string]any, field string) {
	t.Helper()
	if _, present := payload[field]; present {
		t.Fatalf("%s is present (%#v), want absent", field, payload[field])
	}
}

func wantString(t *testing.T, payload map[string]any, field, want string) {
	t.Helper()
	if got, _ := payload[field].(string); got != want {
		t.Fatalf("%s = %q, want %q", field, payload[field], want)
	}
}

func wantBool(t *testing.T, payload map[string]any, field string, want bool) {
	t.Helper()
	got, ok := payload[field].(bool)
	if !ok || got != want {
		t.Fatalf("%s = %#v, want %v", field, payload[field], want)
	}
}

// plantAccumulator writes a budget accumulator row exactly as elitea-scheduler's
// budgetwriteback consumer does — same table, same key, same USD NUMERIC
// column — so the read path is exercised against the real write-back shape and
// not against a fixture invented here.
func plantAccumulator(t *testing.T, pool *pgxpool.Pool, scope, scopeID string, projectID int, start, end time.Time, usd string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_budget_accumulators
    (project_id, scope, scope_id, period_start, period_end, accumulated_cost)
VALUES ($1, $2, $3, $4, $5, $6::numeric)`, projectID, scope, scopeID, start, end, usd)
	if err != nil {
		t.Fatalf("plant accumulator: %v", err)
	}
}

// enforcementColumns reads the two columns the GATEWAY reads. This is the
// assertion an echo handler cannot pass.
func enforcementColumns(t *testing.T, pool *pgxpool.Pool, projectID int) (limit *string, isUnlimited, enabled bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT hard_limit_usd::text, is_unlimited, enabled FROM gateway.project_budget WHERE project_id = $1`,
		projectID).Scan(&limit, &isUnlimited, &enabled)
	if err != nil {
		t.Fatalf("read enforcement columns: %v", err)
	}
	return limit, isUnlimited, enabled
}

func newBudgetsEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newBudgetsPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (%d, 'alpha-team', %d, '{}', true),
			       (%d, 'project_user_%d', %d, '{}', true)`,
			budgetProjectID, budgetAdminUser, budgetOtherID, budgetMemberUser, budgetMemberUser),
		fmt.Sprintf(`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(%d, 'ada@example.com', 'Ada'),
			(%d, 'bo@example.com', 'Bo'),
			(%d, 'cy@example.com', 'Cy'),
			(9001, 'system_user_1@centry.user', 'service account')`,
			budgetAdminUser, budgetMemberUser, budgetOutsider),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
			(1, %d, 'admin'), (2, %d, 'editor'), (3, %d, 'admin')`,
			budgetProjectID, budgetProjectID, budgetOtherID),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
			(%d, %d, 1), (%d, %d, 2), (%d, 9001, 2), (%d, %d, 3)`,
			budgetProjectID, budgetAdminUser, budgetProjectID, budgetMemberUser,
			budgetProjectID, budgetOtherID, budgetMemberUser),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return pool, budgetsRouter(handler.NewHandler(pool, handler.WithClock(func() time.Time { return fixedNow })))
}

func newBudgetsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
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

	databaseName := fmt.Sprintf("elitea_budgets_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	for _, projection := range []string{
		dbschema.CentryProjectsBaselineSQLCProjection,
		dbschema.AuthCoreBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply schema projection: %v", err)
		}
	}
	// The REAL gateway migration, not a copy of it: a column added there and
	// not here would otherwise let a test pass against a schema production
	// does not have.
	gatewaySQL, err := infradb.GatewayMigrationSQL()
	if err != nil {
		t.Fatalf("load gateway migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, gatewaySQL); err != nil {
		t.Fatalf("apply gateway migrations: %v", err)
	}
	return pool
}

/* ── project budget: the write reaches enforcement ─────────────────────── */

func TestProjectBudgetWriteSetsTheColumnsTheGatewayReads(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": true})
	requireStatus(t, recorder, http.StatusOK)

	body := decodeMap(t, recorder)
	wantNumber(t, body, "monthly_limit", "100.00")
	wantNumber(t, body, "effective_limit", "100.00")
	wantString(t, body, "limit_source", "explicit")
	wantBool(t, body, "enabled", true)

	// THE assertion an echo could not pass: is_unlimited is what the gateway's
	// failmode snapshot reads, and it is DERIVED on write. A handler that
	// stored only the authored fields would answer exactly the same body and
	// enforce nothing.
	limit, isUnlimited, enabled := enforcementColumns(t, pool, budgetProjectID)
	if limit == nil || *limit != "100.00" {
		t.Fatalf("hard_limit_usd = %v, want 100.00", limit)
	}
	if isUnlimited {
		t.Fatal("is_unlimited = true after setting a limit: the gateway would not enforce it")
	}
	if !enabled {
		t.Fatal("enabled = false after an enabled write")
	}
}

func TestProjectBudgetDisablingLeavesTheCeilingButStopsEnforcing(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": true})

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": false})
	requireStatus(t, recorder, http.StatusOK)

	body := decodeMap(t, recorder)
	// The authored number survives; the ENFORCED one does not. Reporting the
	// two as one field is how an exemption reads as "the limit was deleted".
	wantNumber(t, body, "monthly_limit", "100.00")
	wantNull(t, body, "effective_limit")
	wantString(t, body, "limit_source", "unlimited")

	limit, isUnlimited, enabled := enforcementColumns(t, pool, budgetProjectID)
	if limit == nil || *limit != "100.00" {
		t.Fatalf("hard_limit_usd = %v, want the authored 100.00 to survive", limit)
	}
	if !isUnlimited {
		t.Fatal("is_unlimited = false for a disabled budget: the gateway would still enforce it")
	}
	if enabled {
		t.Fatal("enabled = true after disabling")
	}
}

// A budget that is enabled with no ceiling yet and a budget that is
// deliberately exempt are BOTH unlimited to the gateway. Reading `enabled` back
// off `is_unlimited` would collapse them, and the form would show a value
// nobody typed — which is why `enabled` is stored separately.
func TestProjectBudgetRoundTripsEnabledWithNoCeiling(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": nil, "enabled": true})
	requireStatus(t, recorder, http.StatusOK)

	read := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, read, http.StatusOK)
	body := decodeMap(t, read)
	wantNull(t, body, "monthly_limit")
	wantNull(t, body, "effective_limit")
	wantBool(t, body, "enabled", true)
	wantString(t, body, "limit_source", "unlimited")
}

func TestProjectBudgetSpendComesFromTheWriteBackAccumulator(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 200, "enabled": true})

	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "37.50")
	// A previous period's row must NOT be counted: the accumulator is keyed by
	// (scope, scope_id, period_start), and a read that ignored the period would
	// report a project's lifetime spend as this month's.
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart().AddDate(0, -1, 0), periodStart(), "999.99")

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/prompt_lib/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)

	body := decodeMap(t, recorder)
	wantNumber(t, body, "spend", "37.50000000")
	wantNumber(t, body, "remaining", "162.50000000")
	wantNumber(t, body, "percent_used", "18.75")
	wantBool(t, body, "spend_available", true)
	wantString(t, body, "period", "202608")
	wantString(t, body, "period_start", "2026-08-01")
	wantString(t, body, "period_end", "2026-08-31")
}

// "No spend recorded" and "no usage data" are different facts, and the second
// is about the write-back pipeline rather than about the project.
func TestProjectBudgetReportsWhenNoAccumulatorExists(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)

	body := decodeMap(t, recorder)
	wantNumber(t, body, "spend", "0")
	wantBool(t, body, "spend_available", false)
	// A project nobody has configured is unlimited, not a 404: the page has
	// nothing to show yet, which is not the same as not existing.
	wantNull(t, body, "monthly_limit")
	wantBool(t, body, "enabled", false)
	// The token counts and breakdowns the LiteLLM-backed reference served are
	// absent rather than zero-filled — see the package doc.
	wantAbsent(t, body, "total_tokens")
	wantAbsent(t, body, "models")
	wantAbsent(t, body, "daily")
}

func TestProjectBudgetWriteRejectionsPersistNothing(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	for name, body := range map[string]any{
		"negative limit":     map[string]any{"monthly_limit": -1},
		"foreign currency":   map[string]any{"monthly_limit": 10, "currency": "EUR"},
		"threshold too high": map[string]any{"monthly_limit": 10, "soft_alert_pct": 101},
	} {
		recorder := budgetsDo(t, router, http.MethodPut,
			fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", name, recorder.Code, recorder.Body.String())
		}
	}

	// A 400 that had already written the row would be the worst of both.
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM gateway.project_budget WHERE project_id = $1`, budgetProjectID,
	).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rejected writes persisted %d budget row(s)", rows)
	}
}

func TestProjectBudgetWriteStoresTheExactDecimal(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	// 0.1 is not representable in binary floating point. A limit that went
	// through float64 comes back as 0.10000000000000001 or is silently rounded;
	// the money path is built so it cannot.
	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": json.Number("0.10"), "enabled": true})
	requireStatus(t, recorder, http.StatusOK)
	wantNumber(t, decodeMap(t, recorder), "monthly_limit", "0.10")
}

func TestProjectBudgetSoftAlertThresholdRoundTripsAndDefaults(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	read := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	if got := decodeMap(t, read)["warning_pct"]; fmt.Sprint(got) != "80" {
		t.Fatalf("default warning_pct = %v, want 80", got)
	}

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 50, "soft_alert_pct": 90})
	read = budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	if got := decodeMap(t, read)["warning_pct"]; fmt.Sprint(got) != "90" {
		t.Fatalf("warning_pct = %v, want the written 90", got)
	}

	// Omitting it must not silently reset it to the default.
	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 60})
	read = budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	if got := decodeMap(t, read)["warning_pct"]; fmt.Sprint(got) != "90" {
		t.Fatalf("warning_pct = %v after an omitted threshold, want the stored 90", got)
	}
}

/* ── budget_period and nats_fail_mode (gap G4) ─────────────────────────── */

// policyColumns reads the two project-only policy columns straight from the
// table the GATEWAY reads. Both had no writer at all before this: the upsert
// hard-coded 'monthly' and never named nats_fail_mode, so the column the
// gateway's failmode resolver consults was reachable only by direct SQL.
func policyColumns(t *testing.T, pool *pgxpool.Pool, projectID int) (period string, failMode *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT budget_period, nats_fail_mode FROM gateway.project_budget WHERE project_id = $1`,
		projectID).Scan(&period, &failMode)
	if err != nil {
		t.Fatalf("read policy columns: %v", err)
	}
	return period, failMode
}

func TestProjectBudgetWriteStoresTheFailModeTheGatewayResolves(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "budget_period": "monthly", "nats_fail_mode": "fail_closed"})
	requireStatus(t, recorder, http.StatusOK)

	body := decodeMap(t, recorder)
	wantString(t, body, "budget_period", "monthly")
	wantString(t, body, "nats_fail_mode", "fail_closed")

	// The assertion an echo could not pass. The response is assembled from a
	// re-read, but only the COLUMN is what
	// services/elitea-llm-gateway/internal/failmode/store.go point-reads.
	period, failMode := policyColumns(t, pool, budgetProjectID)
	if period != "monthly" {
		t.Fatalf("budget_period = %q, want monthly", period)
	}
	if failMode == nil || *failMode != "fail_closed" {
		t.Fatalf("nats_fail_mode = %v, want fail_closed", failMode)
	}
}

// The tri-state that a *string could not carry. Absent must leave the stored
// mode alone; explicit null must clear it back to the platform baseline.
func TestProjectBudgetFailModeDistinguishesAbsentFromNull(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID)

	budgetsDo(t, router, http.MethodPut, url, budgetAdminUser,
		map[string]any{"monthly_limit": 100, "nats_fail_mode": "fail_open"})

	// An edit that moves only the limit must not reset the policy.
	requireStatus(t, budgetsDo(t, router, http.MethodPut, url, budgetAdminUser,
		map[string]any{"monthly_limit": 250}), http.StatusOK)
	if _, failMode := policyColumns(t, pool, budgetProjectID); failMode == nil || *failMode != "fail_open" {
		t.Fatalf("nats_fail_mode = %v after an omitted field, want the stored fail_open", failMode)
	}

	// An explicit null is a request, and it must land.
	recorder := budgetsDo(t, router, http.MethodPut, url, budgetAdminUser,
		map[string]any{"monthly_limit": 250, "nats_fail_mode": nil})
	requireStatus(t, recorder, http.StatusOK)
	wantNull(t, decodeMap(t, recorder), "nats_fail_mode")
	if _, failMode := policyColumns(t, pool, budgetProjectID); failMode != nil {
		t.Fatalf("nats_fail_mode = %v after an explicit null, want NULL (inherit the baseline)", *failMode)
	}
}

// A project nobody has configured reports the defaults rather than failing:
// there is no row at all, and that is the default state.
func TestProjectBudgetReportsThePolicyDefaultsWithNoRow(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)
	wantString(t, body, "budget_period", "monthly")
	wantNull(t, body, "nats_fail_mode")
}

func TestProjectBudgetPolicyRejectionsPersistNothing(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID)

	for _, body := range []map[string]any{
		{"monthly_limit": 10, "budget_period": "weekly"},
		{"monthly_limit": 10, "nats_fail_mode": "fail_sideways"},
	} {
		requireStatus(t, budgetsDo(t, router, http.MethodPut, url, budgetAdminUser, body),
			http.StatusBadRequest)
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM gateway.project_budget WHERE project_id = $1`,
		budgetProjectID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a rejected write left %d rows: the limit landed with the bad policy", rows)
	}
}

// The member PUT REFUSES the two project-scoped fields rather than answering
// 200 over a payload it discarded. gateway.user_budget has neither column.
func TestMemberBudgetWriteRefusesTheProjectPolicyFields(t *testing.T) {
	_, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser)

	for _, body := range []map[string]any{
		{"monthly_limit": 10, "budget_period": "monthly"},
		{"monthly_limit": 10, "nats_fail_mode": "fail_open"},
	} {
		requireStatus(t, budgetsDo(t, router, http.MethodPut, url, budgetAdminUser, body),
			http.StatusBadRequest)
	}
}

/* ── clearing back to the default (gap G4) ─────────────────────────────── */

func TestClearingAProjectBudgetRemovesTheAuthoredRow(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID)

	budgetsDo(t, router, http.MethodPut, url, budgetAdminUser,
		map[string]any{"monthly_limit": 100, "soft_alert_pct": 55, "nats_fail_mode": "fail_closed"})

	recorder := budgetsDo(t, router, http.MethodDelete, url, budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)

	// The response is the RESULT, not the request: after a clear the project is
	// unlimited and inherits the platform threshold again.
	body := decodeMap(t, recorder)
	wantNull(t, body, "monthly_limit")
	wantNull(t, body, "effective_limit")
	wantNull(t, body, "nats_fail_mode")
	wantString(t, body, "limit_source", "unlimited")
	wantBool(t, body, "enabled", false)
	if got := body["warning_pct"]; fmt.Sprint(got) != "80" {
		t.Fatalf("warning_pct = %v after a clear, want the inherited 80 rather than the authored 55", got)
	}

	// `enabled: false` was the nearest thing available before this route, and
	// it is a DIFFERENT state: it keeps a row. A clear must leave none.
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM gateway.project_budget WHERE project_id = $1`,
		budgetProjectID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("gateway.project_budget still holds %d rows after a clear", rows)
	}
}

// The trap this route could have shipped. gateway.llm_budget_accumulators
// declares `budget_rule_id UUID REFERENCES gateway.project_budget(id) ON DELETE
// CASCADE`, so if anything ever populates that column, clearing a limit would
// delete the period's billing history as a side effect. Nothing writes it
// today — this pins that the spend survives, and fails the day it stops being
// true.
func TestClearingAProjectBudgetKeepsTheSpend(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID)

	budgetsDo(t, router, http.MethodPut, url, budgetAdminUser, map[string]any{"monthly_limit": 100})
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "42.50")

	requireStatus(t, budgetsDo(t, router, http.MethodDelete, url, budgetAdminUser, nil), http.StatusOK)

	read := budgetsDo(t, router, http.MethodGet, url, budgetAdminUser, nil)
	requireStatus(t, read, http.StatusOK)
	// accumulated_cost is NUMERIC(20,8), and the exact scale PostgreSQL
	// produces is what reaches the wire — the money path never rounds through
	// float64.
	wantNumber(t, decodeMap(t, read), "spend", "42.50000000")
}

// Clearing a project that has no budget states an intent that is already true,
// so it succeeds. A 404 would make a retry after a lost response look like a
// failure.
func TestClearingAnUnconfiguredProjectBudgetSucceeds(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodDelete,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	wantNull(t, decodeMap(t, recorder), "monthly_limit")
}

// A member cap IS enforced (#321), so lifting one set by mistake has to be
// possible. `enabled: false` stores the different fact "deliberately exempt".
func TestClearingAMemberBudgetRemovesTheCap(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	url := fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser)

	budgetsDo(t, router, http.MethodPut, url, budgetAdminUser, map[string]any{"monthly_limit": 25})

	recorder := budgetsDo(t, router, http.MethodDelete, url, budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)
	wantNull(t, body, "monthly_limit")
	wantString(t, body, "limit_source", "unlimited")

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM gateway.user_budget WHERE project_id = $1 AND user_id = $2`,
		budgetProjectID, budgetMemberUser).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("gateway.user_budget still holds %d rows after a clear", rows)
	}
}

/* ── the admin listing ─────────────────────────────────────────────────── */

func TestListProjectBudgetsCarriesLimitsSpendAndOwnerIdentity(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 400, "enabled": true})
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "100.00")

	recorder := budgetsDo(t, router, http.MethodGet, "/project_budgets/administration", budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	listing := decodeMap(t, recorder)

	if fmt.Sprint(listing["total"]) != "2" {
		t.Fatalf("total = %v, want 2", listing["total"])
	}
	counts, _ := listing["counts"].(map[string]any)
	if fmt.Sprint(counts["team"]) != "1" || fmt.Sprint(counts["personal"]) != "1" {
		t.Fatalf("counts = %v, want one team and one personal", counts)
	}

	rows, _ := listing["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byID := map[string]map[string]any{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		byID[fmt.Sprint(row["project_id"])] = row
	}

	team := byID[fmt.Sprint(budgetProjectID)]
	wantNumber(t, team, "monthly_limit", "400.00")
	wantNumber(t, team, "spend", "100.00000000")
	wantNumber(t, team, "percent_used", "25.00")
	wantString(t, team, "display_name", "alpha-team")

	// A personal project is labelled by its owner's identity: nobody searches
	// for project_user_502.
	personal := byID[fmt.Sprint(budgetOtherID)]
	wantBool(t, personal, "is_personal", true)
	wantString(t, personal, "display_name", "bo@example.com")
	wantBool(t, personal, "spend_available", false)
}

func TestListProjectBudgetsFiltersAndPaginatesTheFilteredSet(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodGet,
		"/project_budgets/administration?project_type=team", budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	listing := decodeMap(t, recorder)
	if fmt.Sprint(listing["total"]) != "1" {
		// A total that counted the whole table would advertise pages that do
		// not exist as soon as a filter is applied.
		t.Fatalf("filtered total = %v, want 1", listing["total"])
	}
	counts, _ := listing["counts"].(map[string]any)
	if fmt.Sprint(counts["personal"]) != "1" {
		t.Fatalf("counts narrowed with the filter (%v); they label the tabs and must not", counts)
	}

	// The owner's email finds their personal project, whose name contains no
	// part of it.
	recorder = budgetsDo(t, router, http.MethodGet,
		"/project_budgets/administration?search=bo@example.com", budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	rows, _ := decodeMap(t, recorder)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("search by owner email matched %d rows, want 1", len(rows))
	}
}

/* ── per-member budgets ────────────────────────────────────────────────── */

// The claim this package makes about the platform, pinned. It was `false` until
// issue #321 gave the gateway a per-member admission check; if that check is
// ever removed or bypassed this test fails and the field has to be changed
// deliberately — which is the point of it being a field rather than a comment
// (#218, #135).
func TestUserBudgetReportsThatItIsEnforced(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, map[string]any{"monthly_limit": 25, "enabled": true})
	requireStatus(t, recorder, http.StatusOK)
	wantBool(t, decodeMap(t, recorder), "enforced", true)
}

func TestUserBudgetRoundTripsThroughStorage(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, map[string]any{"monthly_limit": 25, "enabled": true})
	requireStatus(t, recorder, http.StatusOK)

	// Through the product's own read...
	read := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, nil)
	requireStatus(t, read, http.StatusOK)
	body := decodeMap(t, read)
	wantNumber(t, body, "monthly_limit", "25.00")
	wantNumber(t, body, "effective_limit", "25.00")

	// ...and through the table, which the response alone cannot prove.
	var stored *string
	if err := pool.QueryRow(context.Background(),
		`SELECT hard_limit_usd::text FROM gateway.user_budget WHERE project_id = $1 AND user_id = $2`,
		budgetProjectID, budgetMemberUser).Scan(&stored); err != nil {
		t.Fatalf("read stored member budget: %v", err)
	}
	if stored == nil || *stored != "25.00" {
		t.Fatalf("stored hard_limit_usd = %v, want 25.00", stored)
	}

	// A second write REPLACES rather than duplicating: the unique constraint is
	// what the upsert depends on.
	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, map[string]any{"monthly_limit": 30, "enabled": true})
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM gateway.user_budget WHERE project_id = $1 AND user_id = $2`,
		budgetProjectID, budgetMemberUser).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("member budget rows = %d after two writes, want 1", rows)
	}
}

func TestMemberMayReadOnlyTheirOwnBudgetInProjectScope(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	// Their own: allowed.
	own := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budget/prompt_lib/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetMemberUser, nil)
	requireStatus(t, own, http.StatusOK)

	// A colleague's: refused. Without this an editor reads the whole team's
	// spend by editing the URL, since the route gate only proves membership.
	other := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budget/prompt_lib/%d/user_budget/%d", budgetProjectID, budgetAdminUser),
		budgetMemberUser, nil)
	requireStatus(t, other, http.StatusForbidden)

	// A project admin may read anyone's.
	asAdmin := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budget/prompt_lib/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, nil)
	requireStatus(t, asAdmin, http.StatusOK)

	// The administration route carries no membership check by design — and it
	// is a SEPARATE handler, so a caller cannot reach it by asking for a
	// different mode on the project-scoped one.
	admin := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetAdminUser),
		budgetMemberUser, nil)
	requireStatus(t, admin, http.StatusOK)
}

func TestListUserBudgetsIsProjectAdminOnlyAndListsRealMembers(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	refused := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budgets/prompt_lib/%d", budgetProjectID), budgetMemberUser, nil)
	requireStatus(t, refused, http.StatusForbidden)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, map[string]any{"monthly_limit": 15, "enabled": true})
	plantAccumulator(t, pool, "user", fmt.Sprintf("%d:%d", budgetProjectID, budgetMemberUser),
		budgetProjectID, periodStart(), periodEnd(), "3.00")

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budgets/prompt_lib/%d", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	listing := decodeMap(t, recorder)

	rows, _ := listing["rows"].([]any)
	// Two human members; the project's own service account is excluded, as
	// `filter_system_user=True` does in the reference.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the two human members (got %v)", len(rows), listing["rows"])
	}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if fmt.Sprint(row["email"]) == "system_user_1@centry.user" {
			t.Fatal("the project service account is listed as a member")
		}
		if fmt.Sprint(row["user_id"]) == fmt.Sprint(budgetMemberUser) {
			wantNumber(t, row, "monthly_limit", "15.00")
			wantNumber(t, row, "spend", "3.00000000")
			wantNumber(t, row, "percent_used", "20.00")
			wantBool(t, row, "enforced", true)
			roles, _ := row["roles"].([]any)
			if len(roles) != 1 || fmt.Sprint(roles[0]) != "editor" {
				t.Fatalf("roles = %v, want [editor] exactly once", roles)
			}
		}
	}
}

/* ── usage ─────────────────────────────────────────────────────────────── */

func TestUsageRejectsAnUnknownScope(t *testing.T) {
	_, router := newBudgetsEnvironment(t)
	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage?scope=team", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusBadRequest)
}

// The prompt_lib project-budget read redacts on the same rule /usage does.
// Until it did, a member refused the amounts on /usage could read the identical
// figures off this endpoint — same gate, same numbers — and the redaction was
// decorative.
func TestProjectBudgetReadRedactsForAMemberWhoIsNotAnAdmin(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": true})
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "40.00")

	redacted := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/prompt_lib/%d/budget", budgetProjectID), budgetMemberUser, nil)
	requireStatus(t, redacted, http.StatusOK)
	body := decodeMap(t, redacted)
	wantBool(t, body, "can_see_amounts", false)
	for _, field := range []string{"spend", "monthly_limit", "effective_limit", "remaining", "currency"} {
		wantAbsent(t, body, field)
	}
	wantNumber(t, body, "percent_used", "40.00")

	// The administration route is a different handler and does not redact: a
	// platform administrator is entitled to the figures.
	admin := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetMemberUser, nil)
	requireStatus(t, admin, http.StatusOK)
	wantNumber(t, decodeMap(t, admin), "spend", "40.00000000")
}

// A ceiling the gateway is not applying must not be reported as enforced. The
// two columns can disagree only on a row this API did not write, so the row is
// planted directly — with `is_unlimited = true` and a real hard_limit_usd, the
// shape 003's backfill exists to avoid creating.
func TestProjectBudgetReportsNoEffectiveLimitWhenTheGatewayTreatsItAsUnlimited(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	if _, err := pool.Exec(context.Background(), `
INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled, soft_alert_pct)
VALUES ($1, 100.00, true, true, 80)`, budgetProjectID); err != nil {
		t.Fatalf("plant divergent budget row: %v", err)
	}

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	// The authored number is still shown...
	wantNumber(t, body, "monthly_limit", "100.00")
	wantBool(t, body, "enabled", true)
	// ...but nothing claims it is being enforced, because is_unlimited says it
	// is not, and is_unlimited is what the gateway reads.
	wantNull(t, body, "effective_limit")
	wantNull(t, body, "remaining")
	wantNull(t, body, "percent_used")
	wantString(t, body, "limit_source", "unlimited")

	// Same answer from the listing, which derives it independently.
	listing := budgetsDo(t, router, http.MethodGet, "/project_budgets/administration", budgetAdminUser, nil)
	requireStatus(t, listing, http.StatusOK)
	rows, _ := decodeMap(t, listing)["rows"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if fmt.Sprint(row["project_id"]) == fmt.Sprint(budgetProjectID) {
			wantNull(t, row, "effective_limit")
			wantString(t, row, "limit_source", "unlimited")
		}
	}
}

// `?limit=` is also the capacity the result slice is preallocated with, so it
// has to be bounded before it reaches make().
func TestListProjectBudgetsClampsThePageSize(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodGet,
		"/project_budgets/administration?limit=100000000", budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	rows, _ := decodeMap(t, recorder)["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the 2 seeded projects", len(rows))
	}
}

func TestUsageRedactsAmountsForAMemberWhoIsNotAnAdmin(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": true})
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "40.00")

	// A project admin sees the money.
	visible := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, visible, http.StatusOK)
	body := decodeMap(t, visible)
	wantBool(t, body, "can_see_amounts", true)
	wantNumber(t, body, "spend", "40.00000000")
	wantNumber(t, body, "percent_used", "40.00")
	wantString(t, body, "scope", "project")
	wantNull(t, body, "user_id")

	// An editor does not — but still gets the percentage the usage bar draws.
	redacted := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetMemberUser, nil)
	requireStatus(t, redacted, http.StatusOK)
	body = decodeMap(t, redacted)
	wantBool(t, body, "can_see_amounts", false)
	for _, field := range []string{"spend", "monthly_limit", "effective_limit", "remaining", "currency"} {
		wantAbsent(t, body, field)
	}
	wantNumber(t, body, "percent_used", "40.00")
	if fmt.Sprint(body["warning_pct"]) != "80" {
		t.Fatalf("warning_pct = %v, want it to survive redaction", body["warning_pct"])
	}
}

// A personal project's spend is its owner's own, so the amounts are always
// theirs to see even though they hold no admin role in it.
func TestUsageShowsAmountsToAPersonalProjectOwner(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetOtherID), budgetMemberUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	wantBool(t, decodeMap(t, recorder), "can_see_amounts", true)

	// ...and not to anyone else's.
	other := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetOtherID), budgetOutsider, nil)
	requireStatus(t, other, http.StatusOK)
	wantBool(t, decodeMap(t, other), "can_see_amounts", false)
}

func TestUsageUserScopeReportsTheMembersOwnBudget(t *testing.T) {
	_, router := newBudgetsEnvironment(t)

	budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetAdminUser),
		budgetAdminUser, map[string]any{"monthly_limit": 12, "enabled": true})

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage?scope=user", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)
	wantString(t, body, "scope", "user")
	wantNumber(t, body, "monthly_limit", "12.00")
	if fmt.Sprint(body["user_id"]) != fmt.Sprint(budgetAdminUser) {
		t.Fatalf("user_id = %v, want the caller's own %d", body["user_id"], budgetAdminUser)
	}
	// This member has no accumulator row of their own in this period, so the
	// per-member scope reports spend_available=false. Since #321 the gateway
	// DOES write user-scoped rows, so this is "this member has not spent yet",
	// not "nothing ever writes these" — the distinction the field exists for.
	wantBool(t, body, "spend_available", false)
	wantBool(t, body, "enforced", true)
}

/* ── warning_active: the threshold crossing (issue 312) ────────────────── */

// setProjectBudgetFor writes a ceiling and a threshold through the product's
// own PUT, so these cases exercise the same row the gateway reads.
func setProjectBudgetFor(t *testing.T, router chi.Router, limitUSD any, softAlertPct int) {
	t.Helper()
	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": limitUSD, "soft_alert_pct": softAlertPct})
	requireStatus(t, recorder, http.StatusOK)
}

func readProjectBudget(t *testing.T, router chi.Router) map[string]any {
	t.Helper()
	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	return decodeMap(t, recorder)
}

// The banner this field drives is the whole feature (issue 312): the endpoints
// have existed since #322 with nothing calling them, so a project heading for
// its ceiling gets no warning today, only a hard stop at 100%.
//
// The three boundary cases are one test because the boundary is the point. A
// client deriving the flag itself would have to pick `>` or `>=` and would pick
// differently from the gateway's own soft-alert path.
func TestProjectBudgetReportsTheThresholdCrossing(t *testing.T) {
	for name, testCase := range map[string]struct {
		spendUSD string
		want     bool
	}{
		// 40 of 100 with a threshold of 80.
		"below the threshold": {spendUSD: "40.00", want: false},
		// EXACTLY at it. "reached 80% of its budget" is what the reference's
		// own copy says, so 80 must warn.
		"at the threshold": {spendUSD: "80.00", want: true},
		// A hair under, after rounding to the two decimals the client renders.
		"just under after rounding": {spendUSD: "79.994", want: false},
		"over the threshold":        {spendUSD: "93.50", want: true},
		// Past the ceiling entirely. The hard block is the gateway's job; the
		// warning stays true rather than switching off.
		"over the limit": {spendUSD: "140.00", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			pool, router := newBudgetsEnvironment(t)
			setProjectBudgetFor(t, router, 100, 80)
			plantAccumulator(t, pool, "project", strconv.Itoa(budgetProjectID), budgetProjectID,
				periodStart(), periodEnd(), testCase.spendUSD)

			wantBool(t, readProjectBudget(t, router), "warning_active", testCase.want)
		})
	}
}

// No budget set is NOT a warning. An unconfigured project reports a null
// percent_used, and a client that compared null to 80 would decide whatever its
// language's coercion rules decide — which is the reason the server answers
// this field at all.
func TestProjectBudgetWithNoLimitNeverWarns(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	plantAccumulator(t, pool, "project", strconv.Itoa(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "500.00")

	payload := readProjectBudget(t, router)
	wantNull(t, payload, "percent_used")
	wantBool(t, payload, "warning_active", false)
	wantString(t, payload, "limit_source", "unlimited")
}

// A DISABLED budget is exempt, whatever the stored ceiling says, so it must not
// warn either — the gateway is not holding the project to that number.
func TestProjectBudgetThatIsDisabledNeverWarns(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	setProjectBudgetFor(t, router, 100, 80)
	plantAccumulator(t, pool, "project", strconv.Itoa(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "95.00")
	wantBool(t, readProjectBudget(t, router), "warning_active", true)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/project_budget/administration/%d/budget", budgetProjectID), budgetAdminUser,
		map[string]any{"monthly_limit": 100, "enabled": false})
	requireStatus(t, recorder, http.StatusOK)

	wantBool(t, readProjectBudget(t, router), "warning_active", false)
}

// The threshold the flag compares against is the one the scope RESOLVES, not
// the 80 fallback: an operator who moves the project's threshold moves the
// warning with it. Without this the flag would be a second source of truth
// beside warning_pct.
func TestProjectBudgetWarningFollowsTheAuthoredThreshold(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	setProjectBudgetFor(t, router, 100, 95)
	plantAccumulator(t, pool, "project", strconv.Itoa(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "90.00")

	payload := readProjectBudget(t, router)
	if got := fmt.Sprint(payload["warning_pct"]); got != "95" {
		t.Fatalf("warning_pct = %s, want the authored 95", got)
	}
	wantBool(t, payload, "warning_active", false)

	setProjectBudgetFor(t, router, 100, 85)
	wantBool(t, readProjectBudget(t, router), "warning_active", true)
}

// The member half of the same field. userBudgetPageSQL computes it in its own
// SELECT, so a listing that forgot the column would report false for every
// member who is over their threshold — and nothing else in the response would
// look wrong.
func TestMemberBudgetListingReportsTheThresholdCrossing(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)

	recorder := budgetsDo(t, router, http.MethodPut,
		fmt.Sprintf("/user_budget/administration/%d/user_budget/%d", budgetProjectID, budgetMemberUser),
		budgetAdminUser, map[string]any{"monthly_limit": 10, "soft_alert_pct": 50})
	requireStatus(t, recorder, http.StatusOK)
	plantAccumulator(t, pool, "user",
		fmt.Sprintf("%d:%d", budgetProjectID, budgetMemberUser), budgetProjectID,
		periodStart(), periodEnd(), "7.00")

	recorder = budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/user_budgets/administration/%d", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)

	var listing struct {
		Rows []struct {
			UserID        int64 `json:"user_id"`
			WarningActive bool  `json:"warning_active"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	found := false
	for _, row := range listing.Rows {
		if row.UserID != int64(budgetMemberUser) {
			continue
		}
		found = true
		if !row.WarningActive {
			t.Fatal("the member is at 70% of a 50% threshold and the row does not warn")
		}
	}
	if !found {
		t.Fatalf("the listing carries no row for user %d", budgetMemberUser)
	}
}
