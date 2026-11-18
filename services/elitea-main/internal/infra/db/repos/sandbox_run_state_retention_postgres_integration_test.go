package repos

// #2241 against a REAL migrated AgentState database (agentstate 0007, 0016):
// the retention sweep's sandbox side deletes the dispatch and failure-detail
// rows of runs idle past the cutoff and nothing else.

import (
	"testing"
	"time"
)

func TestSandboxRunStatePurgesOnlyRunsIdlePastTheCutoff(t *testing.T) {
	f := newCompiledPGFixture(t)
	f.reset(t)
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM elitea_runtime.sandbox_run_failure_details`); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	dispatch := func(project int, execution string, age time.Duration) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO elitea_runtime.sandbox_dispatches(tenant_id,project_id,execution_id,generation,activation_id,request_digest,audience,created_at)
VALUES('t1',$1,$2,1,decode(repeat('ab',32),'hex'),decode(repeat('cd',32),'hex'),'aud',clock_timestamp()-make_interval(secs => $3::float8))`,
			project, execution, age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	failure := func(project int, execution string, age time.Duration) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO elitea_runtime.sandbox_run_failure_details(tenant_id,project_id,execution_id,generation,failure_detail,recorded_at)
VALUES('t1',$1,$2,1,'boom',clock_timestamp()-make_interval(secs => $3::float8))`,
			project, execution, age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	dispatch(7, "old-both", 40*day)
	failure(7, "old-both", 40*day)
	dispatch(7, "old-dispatch-only", 41*day)
	// A run with a recent failure detail keeps its old dispatch too.
	dispatch(7, "resumed", 45*day)
	failure(7, "resumed", day)
	dispatch(7, "recent", day)
	// Another project's old run is not this project's to clear.
	dispatch(8, "other-project", 60*day)

	store := NewSandboxRunState(f.pool)
	cutoff := time.Now().Add(-30 * day)
	// The limit is honoured: one run first, then the rest.
	first, err := store.PurgeSettledRuns(f.ctx, 7, cutoff, 1)
	if err != nil || first != 1 {
		t.Fatalf("first batch = %d, %v; want 1", first, err)
	}
	second, err := store.PurgeSettledRuns(f.ctx, 7, cutoff, 10)
	if err != nil || second != 1 {
		t.Fatalf("second batch = %d, %v; want 1", second, err)
	}
	left := func(table string) map[string]bool {
		t.Helper()
		rows, err := f.pool.Query(f.ctx, `SELECT execution_id FROM elitea_runtime.`+table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got[id] = true
		}
		return got
	}
	dispatches := left("sandbox_dispatches")
	if len(dispatches) != 3 || !dispatches["resumed"] || !dispatches["recent"] || !dispatches["other-project"] {
		t.Fatalf("dispatches left = %v", dispatches)
	}
	failures := left("sandbox_run_failure_details")
	if len(failures) != 1 || !failures["resumed"] {
		t.Fatalf("failure details left = %v", failures)
	}
}
