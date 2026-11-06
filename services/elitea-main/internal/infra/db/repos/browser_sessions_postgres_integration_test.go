package repos

// The cloud browser session table (shared/0228, ADR-0049 decision 6, #1597)
// against a REAL migrated database. It SKIPS without
// ELITEA_TEST_DATABASE_URL; each case asserts values a skip could not have
// produced.
//
// Replica independence: two BrowserSessions built on two separate pools
// stand for two elitea-main replicas. One records the session, the other
// reads it; nothing passes between them but the row.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

func seedBrowserSessionRun(t *testing.T, pool *pgxpool.Pool, executionID, actor string, conversation string) {
	t.Helper()
	seedEgressExecution(t, pool, executionID, 9, 0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET actor_id = $2::text WHERE execution_id = $1::text`, executionID, actor); err != nil {
		t.Fatalf("seed the owner: %v", err)
	}
	if conversation == "" {
		return
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event)
VALUES ($1::text, 1, 'agent.execute.application.v1', 'bundle-'||$1::text, 'request', $2::text,
        'message-'||$1::text, '1', 'chat_predict')`, executionID, conversation); err != nil {
		t.Fatalf("seed the agent job: %v", err)
	}
}

func browserReport(executionID string, state browsersession.State) BrowserSessionReport {
	return BrowserSessionReport{
		ExecutionID:        executionID,
		Generation:         1,
		SupervisorAudience: "dns:elitea-sandbox-browser",
		Spec: browsersession.Spec{
			Revision: 1, Name: browsersession.NameFor(executionID), Engine: 1,
			CPUMillis: 1000, MemoryBytes: 1 << 30, WallSeconds: 600, MaxPages: 3,
		},
		State: state,
	}
}

func TestBrowserSessionsRecordOnOneReplicaReadOnAnother(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const execution = "exec-browser-view-1597"
	const conversation = "6f1d1c2e-1597-4b5e-9a1d-0123456789ab"
	seedBrowserSessionRun(t, pool, execution, "42", conversation)

	// A second pool on the same database: another elitea-main replica.
	other, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	writer, err := NewBrowserSessions(pool)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewBrowserSessions(other)
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Record(ctx, browserReport(execution, browsersession.StateLive)); err != nil {
		t.Fatalf("record live: %v", err)
	}
	name := browsersession.NameFor(execution)
	got, ok, err := reader.Lookup(ctx, name)
	if err != nil || !ok {
		t.Fatalf("lookup on the other replica: ok=%v err=%v", ok, err)
	}
	if got.ExecutionID != execution || got.ProjectID != 9 || got.OwnerActorID != "42" ||
		got.ConversationUUID != conversation || got.SupervisorAudience != "dns:elitea-sandbox-browser" ||
		got.State != browsersession.StateLive || got.Spec.MemoryBytes != 1<<30 || got.Spec.Name != name {
		t.Fatalf("row %+v", got)
	}
	// The digest the relay binds a view grant to is the worker's.
	if browsersession.SpecDigest(got.Spec) != browsersession.SpecDigest(browserReport(execution, browsersession.StateLive).Spec) {
		t.Fatal("the stored spec does not reproduce the session digest")
	}

	// A repeated report is idempotent; the run end moves it forward.
	if err := writer.Record(ctx, browserReport(execution, browsersession.StateLive)); err != nil {
		t.Fatalf("repeat live: %v", err)
	}
	lingering := browserReport(execution, browsersession.StateLingering)
	lingering.LingerUntil = time.Date(2030, 1, 1, 0, 10, 0, 0, time.UTC)
	if err := writer.Record(ctx, lingering); err != nil {
		t.Fatalf("record lingering: %v", err)
	}
	// Never backwards.
	if err := writer.Record(ctx, browserReport(execution, browsersession.StateLive)); !errors.Is(err, ErrBrowserSessionRefused) {
		t.Fatalf("lingering -> live: %v", err)
	}
	got, _, err = reader.Lookup(ctx, name)
	if err != nil || got.State != browsersession.StateLingering || !got.LingerUntil.Equal(lingering.LingerUntil) {
		t.Fatalf("lingering row %+v err=%v", got, err)
	}
	if err := writer.Record(ctx, browserReport(execution, browsersession.StateEnded)); err != nil {
		t.Fatalf("record ended: %v", err)
	}
	got, _, _ = reader.Lookup(ctx, name)
	if got.State != browsersession.StateEnded || got.Watchable(time.Now()) {
		t.Fatalf("ended row %+v", got)
	}
}

func TestBrowserSessionsRefuseAReportThatIsNotTheRuns(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const execution = "exec-browser-refuse-1597"
	seedBrowserSessionRun(t, pool, execution, "42", "")
	sessions, err := NewBrowserSessions(pool)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*BrowserSessionReport){
		"another run's name": func(r *BrowserSessionReport) {
			r.Spec.Name = browsersession.NameFor("exec-someone-else")
		},
		"an unknown generation": func(r *BrowserSessionReport) { r.Generation = 7 },
		"an unknown execution": func(r *BrowserSessionReport) {
			r.ExecutionID = "exec-browser-absent-1597"
			r.Spec.Name = browsersession.NameFor(r.ExecutionID)
		},
		"a malformed spec": func(r *BrowserSessionReport) { r.Spec.Revision = 2 },
	}
	for label, mutate := range cases {
		report := browserReport(execution, browsersession.StateLive)
		mutate(&report)
		if err := sessions.Record(ctx, report); !errors.Is(err, ErrBrowserSessionRefused) {
			t.Errorf("%s: %v", label, err)
		}
	}
	// A run without a conversation still records its session.
	if err := sessions.Record(ctx, browserReport(execution, browsersession.StateLive)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := sessions.Lookup(ctx, browsersession.NameFor(execution))
	if err != nil || !ok || got.ConversationUUID != "" {
		t.Fatalf("row %+v ok=%v err=%v", got, ok, err)
	}
	// Another supervisor or spec cannot take the row over.
	moved := browserReport(execution, browsersession.StateEnded)
	moved.SupervisorAudience = "dns:elsewhere"
	if err := sessions.Record(ctx, moved); !errors.Is(err, ErrBrowserSessionRefused) {
		t.Fatalf("another supervisor: %v", err)
	}
	if _, ok, err := sessions.Lookup(ctx, "elitea-br-000000000000000000000000"); ok || err != nil {
		t.Fatalf("an unknown session ok=%v err=%v", ok, err)
	}
	if _, ok, err := sessions.Lookup(ctx, "'; DROP TABLE x; --"); ok || err != nil {
		t.Fatalf("a malformed id ok=%v err=%v", ok, err)
	}
}

// A supervisor's "not started" or "ended" ends the row once; the retention
// pass deletes a row a day past its end or past its bound, and nothing else.
func TestBrowserSessionsMarkEndedAndPrune(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	sessions, err := NewBrowserSessions(pool)
	if err != nil {
		t.Fatal(err)
	}
	const live, stale, old, fresh = "exec-browser-live-1597", "exec-browser-stale-1597", "exec-browser-old-1597", "exec-browser-fresh-1597"
	for _, execution := range []string{live, stale, old, fresh} {
		seedBrowserSessionRun(t, pool, execution, "42", "")
		if err := sessions.Record(ctx, browserReport(execution, browsersession.StateLive)); err != nil {
			t.Fatalf("record %s: %v", execution, err)
		}
	}
	got, _, err := sessions.Lookup(ctx, browsersession.NameFor(live))
	if err != nil || got.StartedAt.IsZero() || !got.Watchable(time.Now()) {
		t.Fatalf("live row %+v err=%v", got, err)
	}
	if err := sessions.MarkEnded(ctx, browsersession.NameFor(fresh)); err != nil {
		t.Fatal(err)
	}
	if err := sessions.MarkEnded(ctx, browsersession.NameFor(fresh)); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	if got, _, _ := sessions.Lookup(ctx, browsersession.NameFor(fresh)); got.State != browsersession.StateEnded {
		t.Fatalf("marked row %+v", got)
	}
	// stale: started two days ago, never ended (its worker lost the lease).
	// old: ended two days ago.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.browser_sessions SET started_at = clock_timestamp() - interval '2 days'
WHERE session_id = $1::text`, browsersession.NameFor(stale)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := sessions.Lookup(ctx, browsersession.NameFor(stale)); got.Watchable(time.Now()) {
		t.Fatalf("a row past its bound is still watchable: %+v", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.browser_sessions SET state = 'ended',
    started_at = clock_timestamp() - interval '3 days', ended_at = clock_timestamp() - interval '2 days'
WHERE session_id = $1::text`, browsersession.NameFor(old)); err != nil {
		t.Fatal(err)
	}
	pruned, err := sessions.Prune(ctx, 100)
	if err != nil || pruned != 2 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
	for execution, kept := range map[string]bool{live: true, fresh: true, stale: false, old: false} {
		if _, ok, err := sessions.Lookup(ctx, browsersession.NameFor(execution)); ok != kept || err != nil {
			t.Errorf("%s: kept=%v err=%v", execution, ok, err)
		}
	}
	if _, err := sessions.Prune(ctx, 0); err == nil {
		t.Fatal("an unbounded prune")
	}
}
