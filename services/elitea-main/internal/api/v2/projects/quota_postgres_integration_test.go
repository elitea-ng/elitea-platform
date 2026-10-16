package projects_test

// #246 acceptance for the project QUOTA and STATISTICS routes.
//
// Both had no route at all before this change, so "it answers 200" proves only
// that a route now exists. Each case below therefore asserts the STATE, not the
// status:
//
//   - the quota PUT is checked against the columns it must NOT have touched.
//     Writing all six limits from a `usage_type=vcu` request would answer
//     identically and quietly clear the storage ceiling.
//   - the `?quota=` read is checked for its WRITE side effect — the 30-day
//     window rollover and counter reset — which is the only thing that ever
//     resets those counters, and which a read-only reimplementation would drop
//     while still answering the right boolean this month.
//   - statistics is checked against real bytes in the artifact store, so a
//     hardcoded zero fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
)

const quotaProjectID = 31

// quotaRouter mounts the three routes as internal/api/v2/projects/handler.go
// does, minus the permission middleware (a router concern, covered by
// TestGroupWritesAreGated's layer).
func quotaRouter(h *handler.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/quota/{projectID}", h.GetQuota)
	router.Put("/quota/{projectID}", h.PutQuota)
	router.Get("/statistics/{projectID}", h.GetStatistics)
	return router
}

func quotaDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
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
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func quotaDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(recorder.Body.Bytes()))
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func newQuotaEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newQuotaPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (%d, 'quota-team', 1, '{}', true)`, quotaProjectID),
		fmt.Sprintf(`INSERT INTO centry.project_quota
			(project_id, data_retention_limit, dast_scans, sast_scans,
			 vcu_hard_limit, vcu_soft_limit, vcu_limit_total_block,
			 storage_hard_limit, storage_soft_limit, storage_limit_total_block,
			 last_update_time)
			VALUES (%d, 30, 5, 4, 100, 80, false, 10, 8, false, (now() AT TIME ZONE 'utc'))`,
			quotaProjectID),
		fmt.Sprintf(`INSERT INTO centry.statistic
			(project_id, vuh_used, performance_test_runs, sast_scans, dast_scans,
			 ui_performance_test_runs, tasks_executions)
			VALUES (%d, 7, 3, 2, 1, 6, 9)`, quotaProjectID),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return pool, quotaRouter(handler.NewHandler(pool))
}

func newQuotaPool(t *testing.T) *pgxpool.Pool {
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

	databaseName := fmt.Sprintf("elitea_project_quota_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		dbschema.ArtifactStorageBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply schema projection: %v", err)
		}
	}
	return pool
}

/* ── reading the quota ─────────────────────────────────────────────────── */

func TestQuotaReadReturnsTheStoredRow(t *testing.T) {
	_, router := newQuotaEnvironment(t)

	recorder := quotaDo(t, router, http.MethodGet, fmt.Sprintf("/quota/%d", quotaProjectID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	row := quotaDecode(t, recorder)
	for field, want := range map[string]string{
		"project_id":           fmt.Sprint(quotaProjectID),
		"vcu_hard_limit":       "100",
		"vcu_soft_limit":       "80",
		"storage_hard_limit":   "10",
		"data_retention_limit": "30",
	} {
		if fmt.Sprint(row[field]) != want {
			t.Fatalf("%s = %v, want %s", field, row[field], want)
		}
	}
}

func TestQuotaReadIsA404ForAnUnknownProject(t *testing.T) {
	_, router := newQuotaEnvironment(t)
	recorder := quotaDo(t, router, http.MethodGet, "/quota/9999", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestQuotaCheckReportsUnderAndOverTheCeiling(t *testing.T) {
	pool, router := newQuotaEnvironment(t)

	// Seeded: dast_scans used 1, ceiling 5.
	recorder := quotaDo(t, router, http.MethodGet,
		fmt.Sprintf("/quota/%d?quota=dast_scans", quotaProjectID), nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "true\n" {
		t.Fatalf("under-ceiling check = %d %q, want 200 true", recorder.Code, recorder.Body.String())
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE centry.statistic SET dast_scans = 5 WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}
	recorder = quotaDo(t, router, http.MethodGet,
		fmt.Sprintf("/quota/%d?quota=dast_scans", quotaProjectID), nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "false\n" {
		t.Fatalf("at-ceiling check = %d %q, want 200 false", recorder.Code, recorder.Body.String())
	}

	// -1 is the unlimited sentinel: the counter is not consulted at all.
	if _, err := pool.Exec(context.Background(),
		`UPDATE centry.project_quota SET dast_scans = -1 WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}
	recorder = quotaDo(t, router, http.MethodGet,
		fmt.Sprintf("/quota/%d?quota=dast_scans", quotaProjectID), nil)
	if recorder.Body.String() != "true\n" {
		t.Fatalf("unlimited check = %q, want true", recorder.Body.String())
	}
}

func TestQuotaCheckRejectsAMetricWithNoCounter(t *testing.T) {
	_, router := newQuotaEnvironment(t)
	// `vcu_hard_limit` is a quota column with no counterpart in the counter
	// table. The reference raises KeyError on it; a 400 naming the two
	// checkable metrics is the replacement.
	recorder := quotaDo(t, router, http.MethodGet,
		fmt.Sprintf("/quota/%d?quota=vcu_hard_limit", quotaProjectID), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// The write side of a read. This is the behaviour a "just port the GET"
// reimplementation drops silently: without it the counters never reset, and a
// project that once hit its ceiling is denied forever.
func TestQuotaCheckRollsTheWindowOverAndZeroesTheCounters(t *testing.T) {
	pool, router := newQuotaEnvironment(t)
	ctx := context.Background()

	// Push the window 31 days into the past so it is due.
	if _, err := pool.Exec(ctx, `
UPDATE centry.project_quota
SET last_update_time = (now() AT TIME ZONE 'utc') - INTERVAL '31 days'
WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE centry.statistic SET dast_scans = 5, vuh_used = 42 WHERE project_id = $1`,
		quotaProjectID); err != nil {
		t.Fatal(err)
	}

	// At the ceiling before the check; the rollover happens FIRST, so the
	// answer is "under" and the counters are gone.
	recorder := quotaDo(t, router, http.MethodGet,
		fmt.Sprintf("/quota/%d?quota=dast_scans", quotaProjectID), nil)
	if recorder.Body.String() != "true\n" {
		t.Fatalf("post-rollover check = %q, want true", recorder.Body.String())
	}

	var dast, vuh, tasks int
	if err := pool.QueryRow(ctx,
		`SELECT dast_scans, vuh_used, tasks_executions FROM centry.statistic WHERE project_id = $1`,
		quotaProjectID).Scan(&dast, &vuh, &tasks); err != nil {
		t.Fatal(err)
	}
	if dast != 0 || vuh != 0 {
		t.Fatalf("rolling counters = dast %d, vuh %d; want both reset to 0", dast, vuh)
	}
	// tasks_executions is a LIFETIME counter and is not in the reference's
	// reset list, so a reset that zeroed the whole row would be wrong.
	if tasks != 9 {
		t.Fatalf("tasks_executions = %d, want the lifetime 9 to survive the rollover", tasks)
	}

	// The window advanced by exactly one period rather than jumping to now, so
	// a project two windows behind catches up one per check.
	var age float64
	if err := pool.QueryRow(ctx, `
SELECT EXTRACT(EPOCH FROM ((now() AT TIME ZONE 'utc') - last_update_time))
FROM centry.project_quota WHERE project_id = $1`, quotaProjectID).Scan(&age); err != nil {
		t.Fatal(err)
	}
	if age < 0 || age > 2*24*3600 {
		t.Fatalf("window age after rollover = %.0fs, want roughly one day (31 days minus one 30-day window)", age)
	}
}

// The unrolled case: a window that is not due must not reset anything.
func TestQuotaCheckLeavesAFreshWindowAlone(t *testing.T) {
	pool, router := newQuotaEnvironment(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE centry.statistic SET dast_scans = 2 WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}
	quotaDo(t, router, http.MethodGet, fmt.Sprintf("/quota/%d?quota=dast_scans", quotaProjectID), nil)

	var dast int
	if err := pool.QueryRow(ctx,
		`SELECT dast_scans FROM centry.statistic WHERE project_id = $1`, quotaProjectID).Scan(&dast); err != nil {
		t.Fatal(err)
	}
	if dast != 2 {
		t.Fatalf("dast_scans = %d after a check inside the window, want the untouched 2", dast)
	}
}

/* ── writing the quota ─────────────────────────────────────────────────── */

func TestQuotaUpdateWritesOnlyItsOwnTriple(t *testing.T) {
	pool, router := newQuotaEnvironment(t)

	recorder := quotaDo(t, router, http.MethodPut,
		fmt.Sprintf("/quota/%d?usage_type=vcu", quotaProjectID),
		map[string]any{"vcu_hard_limit": 250, "vcu_soft_limit": 200, "vcu_limit_total_block": true})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The response carries the row, so the form re-renders without a refetch.
	row := quotaDecode(t, recorder)
	if fmt.Sprint(row["vcu_hard_limit"]) != "250" || row["vcu_limit_total_block"] != true {
		t.Fatalf("response row = %v, want the written vcu limits", row)
	}

	// THE assertion: the storage triple is untouched. A handler that wrote all
	// six columns would answer identically and silently clear the storage
	// ceiling an operator never edited.
	var hard, soft int
	var block bool
	if err := pool.QueryRow(context.Background(), `
SELECT storage_hard_limit, storage_soft_limit, storage_limit_total_block
FROM centry.project_quota WHERE project_id = $1`, quotaProjectID).Scan(&hard, &soft, &block); err != nil {
		t.Fatal(err)
	}
	if hard != 10 || soft != 8 || block {
		t.Fatalf("storage triple = (%d, %d, %v) after a vcu write, want the seeded (10, 8, false)", hard, soft, block)
	}

	// And the vcu triple really landed in the table, not just in the response.
	var vcuHard int
	if err := pool.QueryRow(context.Background(),
		`SELECT vcu_hard_limit FROM centry.project_quota WHERE project_id = $1`,
		quotaProjectID).Scan(&vcuHard); err != nil {
		t.Fatal(err)
	}
	if vcuHard != 250 {
		t.Fatalf("stored vcu_hard_limit = %d, want 250", vcuHard)
	}
}

func TestQuotaUpdateStorageClearsAnOmittedLimit(t *testing.T) {
	pool, router := newQuotaEnvironment(t)

	// The reference assigns all three unconditionally, so an omitted limit
	// CLEARS it. The form always sends all three; treating omission as "leave
	// alone" would make a ceiling impossible to remove.
	recorder := quotaDo(t, router, http.MethodPut,
		fmt.Sprintf("/quota/%d?usage_type=storage", quotaProjectID),
		map[string]any{"storage_hard_limit": 20})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	var hard, soft *int
	var block bool
	if err := pool.QueryRow(context.Background(), `
SELECT storage_hard_limit, storage_soft_limit, storage_limit_total_block
FROM centry.project_quota WHERE project_id = $1`, quotaProjectID).Scan(&hard, &soft, &block); err != nil {
		t.Fatal(err)
	}
	if hard == nil || *hard != 20 {
		t.Fatalf("storage_hard_limit = %v, want 20", hard)
	}
	if soft != nil {
		t.Fatalf("storage_soft_limit = %v, want it cleared", *soft)
	}
	if block {
		t.Fatal("storage_limit_total_block = true, want the omitted flag to default to false")
	}
}

func TestQuotaUpdateRefusesAMissingOrUnknownUsageType(t *testing.T) {
	_, router := newQuotaEnvironment(t)
	for _, target := range []string{
		fmt.Sprintf("/quota/%d", quotaProjectID),
		fmt.Sprintf("/quota/%d?usage_type=cpu", quotaProjectID),
	} {
		recorder := quotaDo(t, router, http.MethodPut, target, map[string]any{"vcu_hard_limit": 1})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", target, recorder.Code, recorder.Body.String())
		}
	}
}

/* ── statistics ────────────────────────────────────────────────────────── */

func TestStatisticsPairsCountersWithTheirCeilings(t *testing.T) {
	pool, router := newQuotaEnvironment(t)
	ctx := context.Background()

	// Two live buckets and one soft-deleted one. The deleted bucket's bytes
	// must not be counted — a join that ignored deleted_at would report storage
	// nobody is using.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, deleted_at) VALUES
    ($1, 'live-one', NULL), ($1, 'live-two', NULL), ($1, 'gone', now())`, quotaProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.objects (bucket_id, key, byte_length)
SELECT id, 'k', CASE WHEN name = 'gone' THEN 5000 ELSE 1500 END
FROM elitea_storage.buckets WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}

	recorder := quotaDo(t, router, http.MethodGet, fmt.Sprintf("/statistics/%d", quotaProjectID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	stats := quotaDecode(t, recorder)

	entry := func(name string) map[string]any {
		t.Helper()
		value, ok := stats[name].(map[string]any)
		if !ok {
			t.Fatalf("%s is absent from %v", name, stats)
		}
		return value
	}

	if got := fmt.Sprint(entry("dast_scans")["current"]); got != "1" {
		t.Fatalf("dast_scans current = %s, want 1", got)
	}
	if got := fmt.Sprint(entry("dast_scans")["quota"]); got != "5" {
		t.Fatalf("dast_scans quota = %s, want 5", got)
	}
	// A metric the quota table has no column for reports null rather than
	// raising, which is what the reference does on its first key.
	if entry("performance_test_runs")["quota"] != nil {
		t.Fatalf("performance_test_runs quota = %v, want null", entry("performance_test_runs")["quota"])
	}
	if got := fmt.Sprint(entry("performance_test_runs")["current"]); got != "3" {
		t.Fatalf("performance_test_runs current = %s, want 3", got)
	}

	// Live buckets only: 2 × 1500.
	if got := fmt.Sprint(entry("storage_space")["current"]); got != "3000" {
		t.Fatalf("storage_space current = %s bytes, want 3000 (the deleted bucket must not count)", got)
	}
	// 10 GB, in bytes on both sides.
	if got := fmt.Sprint(entry("storage_space")["quota"]); got != "10000000000" {
		t.Fatalf("storage_space quota = %s, want 10000000000", got)
	}

	if got := fmt.Sprint(entry("data_retention_limit")["quota"]); got != "30" {
		t.Fatalf("data_retention_limit quota = %s, want 30", got)
	}
	// tasks_count is deliberately absent: its RPC has no provider in pylon
	// either, and nothing in this platform counts tasks.
	if _, present := stats["tasks_count"]; present {
		t.Fatal("tasks_count is reported; nothing in this platform counts tasks")
	}
}

// A project with counters but no quota row still reports its usage. The
// reference dereferences the missing row.
func TestStatisticsReportsUsageWithNoQuotaRow(t *testing.T) {
	pool, router := newQuotaEnvironment(t)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM centry.project_quota WHERE project_id = $1`, quotaProjectID); err != nil {
		t.Fatal(err)
	}

	recorder := quotaDo(t, router, http.MethodGet, fmt.Sprintf("/statistics/%d", quotaProjectID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	stats := quotaDecode(t, recorder)
	dast, _ := stats["dast_scans"].(map[string]any)
	if fmt.Sprint(dast["current"]) != "1" || dast["quota"] != nil {
		t.Fatalf("dast_scans = %v, want current 1 with a null quota", dast)
	}
}

func TestStatisticsIsA404ForAnUnknownProject(t *testing.T) {
	_, router := newQuotaEnvironment(t)
	recorder := quotaDo(t, router, http.MethodGet, "/statistics/9999", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}
