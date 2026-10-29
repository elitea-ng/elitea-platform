package repos

// The worker egress pieces (ADR-0044 decision 2, issue 1490) against a REAL
// migrated database: the worker's own row beside the proxy's in
// egress_run_summaries (replaced per report, final only on settlement), the
// execution's project read from the kernel, the admin hosts view's agent
// mapping, source filter and truncation, and the policy sources the command
// assembly reads. It SKIPS without ELITEA_TEST_DATABASE_URL; each case
// asserts rows a skip could not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// seedEgressExecution writes the kernel rows of one execution: an input
// bundle and its execution job, plus an agent job naming applicationID when
// it is positive.
func seedEgressExecution(t *testing.T, pool *pgxpool.Pool, executionID string, projectID, applicationID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id) VALUES ($1) ON CONFLICT DO NOTHING`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	bundle := "bundle-" + executionID
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, 'admission:'||$1, $2, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, bundle, projectID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $1, 1, 'cmd-'||$1, '1', $2, $2, 'actor-1', 'actor-1',
    'agent.execute.application.v1', 'v1', $3,
    decode(repeat('61', 32), 'hex'), 'scope-'||$1, 'key-'||$1, 'RUNNING', 'RUNNING'
)`, executionID, projectID, bundle); err != nil {
		t.Fatalf("seed execution job: %v", err)
	}
	if applicationID <= 0 {
		return
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ($1, 'request', '1', 'request', 'application/json',
    decode(repeat('cd', 32), 'hex'), 2, 'inline://request', 'internal', 'worker', '{}'::bytea)`, bundle); err != nil {
		t.Fatalf("seed the request entry: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event, application_id
) VALUES ($1, 1, 'agent.execute.application.v1', $2, 'request', 'stream-'||$1, 'message-'||$1, '1', 'chat_predict', $3)`,
		executionID, bundle, applicationID); err != nil {
		t.Fatalf("seed agent execution job: %v", err)
	}
}

func workerReport(executionID string, projectID int64, hosts ...app.HostReached) app.Report {
	return app.Report{ReplicaID: app.WorkerReplica, ExecutionID: executionID, ProjectID: projectID, HostsReached: hosts}
}

// The worker's row is replaced by each snapshot, stays open until the
// settlement marks it final, and the mark never touches the proxy's row.
func TestWorkerEgressRowIsReplacedAndMarkedFinal(t *testing.T) {
	store := newEgressSummaries(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	first := workerReport("exec-w", 3, app.HostReached{Host: "api.github.com", Port: 443, Connections: 1})
	first.TokenExpiresAt = now.Add(6 * time.Hour)
	second := workerReport("exec-w", 3, app.HostReached{Host: "api.github.com", Port: 443, Connections: 4},
		app.HostReached{Host: "pypi.org", Port: 443, Connections: 2})
	second.TokenExpiresAt = now.Add(6 * time.Hour)
	proxy := app.Report{ReplicaID: "proxy-a", ExecutionID: "exec-w", ProjectID: 3, TokenExpiresAt: now.Add(time.Hour)}
	for _, report := range []app.Report{first, second, proxy} {
		if err := store.Upsert(ctx, report, now); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := store.ClaimDue(ctx, now.Add(time.Minute), 30*time.Second, 10*time.Minute); err != nil || len(rows) != 0 {
		t.Fatalf("an open turn was due: %v %v", rows, err)
	}
	if err := store.MarkFinal(ctx, "exec-w", app.WorkerReplica, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var workerFinal, proxyFinal bool
	var reached int
	if err := store.pool.QueryRow(ctx, `
SELECT bool_or(final) FILTER (WHERE replica = 'worker'), bool_or(final) FILTER (WHERE replica <> 'worker'),
       max(jsonb_array_length(hosts_reached)) FILTER (WHERE replica = 'worker')
FROM elitea_runtime.egress_run_summaries WHERE execution_id = 'exec-w'`).Scan(&workerFinal, &proxyFinal, &reached); err != nil {
		t.Fatal(err)
	}
	if !workerFinal || proxyFinal || reached != 2 {
		t.Fatalf("worker final=%v proxy final=%v worker hosts=%d", workerFinal, proxyFinal, reached)
	}
	// Marking a run that has no worker row, or one already audited, is a no-op.
	if err := store.MarkFinal(ctx, "exec-none", app.WorkerReplica, now); err != nil {
		t.Fatal(err)
	}
}

// The project of a report is the kernel's, by execution and generation.
func TestExecutionProjectReadsTheKernel(t *testing.T) {
	store := newEgressSummaries(t)
	seedEgressExecution(t, store.pool, "exec-p", 9, 0)
	project, err := store.ExecutionProject(context.Background(), "exec-p", 1)
	if err != nil || project != 9 {
		t.Fatalf("project=%d err=%v", project, err)
	}
	if _, err := store.ExecutionProject(context.Background(), "exec-p", 2); err == nil {
		t.Fatal("an unknown generation answered a project")
	}
}

// The hosts view sums per project, agent and host, maps an agent run to its
// application, filters the source and the window, and says when it cut a list.
func TestWorkerEgressHostsAggregates(t *testing.T) {
	store := newEgressSummaries(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedEgressExecution(t, store.pool, "exec-a1", 3, 77)
	seedEgressExecution(t, store.pool, "exec-a2", 3, 77)
	seedEgressExecution(t, store.pool, "exec-chat", 3, 0)
	seedEgressExecution(t, store.pool, "exec-p4", 4, 0)
	github := func(n int64) app.HostReached {
		return app.HostReached{Host: "api.github.com", Port: 443, Connections: n}
	}
	reports := []struct {
		report app.Report
		at     time.Time
	}{
		{workerReport("exec-a1", 3, github(2)), now.Add(-2 * time.Hour)},
		{workerReport("exec-a2", 3, github(5), app.HostReached{Host: "pypi.org", Port: 443, Connections: 1}), now.Add(-time.Hour)},
		{workerReport("exec-chat", 3, github(1)), now.Add(-time.Hour)},
		{workerReport("exec-p4", 4, github(3)), now.Add(-time.Hour)},
		// Outside a 30-day window.
		{workerReport("exec-old", 3, github(100)), now.Add(-40 * 24 * time.Hour)},
		// The proxy's row of an agent run.
		{app.Report{ReplicaID: "proxy-a", ExecutionID: "exec-a1", ProjectID: 3,
			HostsReached: []app.HostReached{{Host: "files.pythonhosted.org", Port: 443, Connections: 6}}}, now.Add(-time.Hour)},
	}
	refused := workerReport("exec-a2", 3, github(5), app.HostReached{Host: "pypi.org", Port: 443, Connections: 1})
	refused.HostsRefused = []app.HostRefused{{Host: "evil.example", Port: 443, Reason: "egress_host_not_allowed", Count: 3}}
	reports[1].report = refused
	for _, r := range reports {
		if err := store.Upsert(ctx, r.report, r.at); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-30 * 24 * time.Hour)

	report, err := store.Hosts(ctx, app.HostsQuery{Since: since, Source: app.SourceWorker, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		project int64
		agent   int64
		host    string
	}
	got := map[key]app.ReachedHost{}
	for _, host := range report.Reached {
		agent := int64(0)
		if host.AgentID != nil {
			agent = *host.AgentID
		}
		got[key{host.ProjectID, agent, host.Host}] = host
	}
	if len(got) != 4 || report.ReachedTruncated {
		t.Fatalf("reached=%+v truncated=%v", report.Reached, report.ReachedTruncated)
	}
	agentGitHub := got[key{3, 77, "api.github.com"}]
	if agentGitHub.Connections != 7 || agentGitHub.Executions != 2 || !agentGitHub.LastSeen.Equal(now.Add(-time.Hour)) {
		t.Fatalf("agent row=%+v", agentGitHub)
	}
	if chat := got[key{3, 0, "api.github.com"}]; chat.Connections != 1 || chat.AgentID != nil {
		t.Fatalf("no-agent row=%+v", chat)
	}
	if got[key{3, 77, "pypi.org"}].Connections != 1 || got[key{4, 0, "api.github.com"}].Connections != 3 {
		t.Fatalf("reached=%+v", report.Reached)
	}
	if report.Reached[0].Connections != 7 {
		t.Fatalf("not ordered by use: %+v", report.Reached)
	}
	if len(report.Refused) != 1 || report.Refused[0].Host != "evil.example" || report.Refused[0].Count != 3 ||
		report.Refused[0].AgentID == nil || *report.Refused[0].AgentID != 77 || report.Refused[0].Reason != "egress_host_not_allowed" {
		t.Fatalf("refused=%+v", report.Refused)
	}

	proxyOnly, err := store.Hosts(ctx, app.HostsQuery{Since: since, Source: app.SourceProxy, Limit: 100})
	if err != nil || len(proxyOnly.Reached) != 1 || proxyOnly.Reached[0].Host != "files.pythonhosted.org" ||
		proxyOnly.Reached[0].AgentID == nil || *proxyOnly.Reached[0].AgentID != 77 {
		t.Fatalf("proxy=%+v err=%v", proxyOnly, err)
	}
	all, err := store.Hosts(ctx, app.HostsQuery{Since: since, Source: app.SourceAll, Limit: 100})
	if err != nil || len(all.Reached) != 5 {
		t.Fatalf("all=%+v err=%v", all.Reached, err)
	}
	project4, err := store.Hosts(ctx, app.HostsQuery{Since: since, Source: app.SourceWorker, ProjectID: 4, Limit: 100})
	if err != nil || len(project4.Reached) != 1 || project4.Reached[0].ProjectID != 4 || len(project4.Refused) != 0 {
		t.Fatalf("project 4=%+v err=%v", project4, err)
	}
	cut, err := store.Hosts(ctx, app.HostsQuery{Since: since, Source: app.SourceWorker, Limit: 2})
	if err != nil || len(cut.Reached) != 2 || !cut.ReachedTruncated || cut.RefusedTruncated {
		t.Fatalf("cut=%+v err=%v", cut, err)
	}
	wide, err := store.Hosts(ctx, app.HostsQuery{Since: now.Add(-90 * 24 * time.Hour), Source: app.SourceWorker, Limit: 100})
	if err != nil || len(wide.Reached) != 4 {
		t.Fatalf("90 days=%+v err=%v", wide.Reached, err)
	}
	for _, host := range wide.Reached {
		if host.ProjectID == 3 && host.AgentID == nil && host.Host == "api.github.com" && host.Connections != 101 {
			t.Fatalf("the old run was not counted in 90 days: %+v", host)
		}
	}
}

// The command assembly's sources: the platform section, the project row and
// the bundle's entries, as stored.
func TestWorkerEgressSourcesRead(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	sources, err := NewWorkerEgressSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	// A database without the admin bootstrap schema never saved the section.
	settings, err := sources.WorkerEgressSettings(ctx)
	if err != nil || settings.Mode != platformconfig.WorkerEgressOpen || len(settings.Allowlist) != 0 {
		t.Fatalf("no table: %+v %v", settings, err)
	}
	// centry.platform_config comes from the bootstrap schema
	// (internal/infra/db/migrations/001_initial.sql), not the ledgered corpus.
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`); err != nil {
		t.Fatal(err)
	}
	settings, err = sources.WorkerEgressSettings(ctx)
	if err != nil || settings.Mode != platformconfig.WorkerEgressOpen || len(settings.Allowlist) != 0 {
		t.Fatalf("never saved: %+v %v", settings, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at) VALUES
    ('worker_egress', 'mode', '"allowlist"', now() AT TIME ZONE 'UTC'),
    ('worker_egress', 'allowlist', '["api.github.com", "*.atlassian.net"]', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("seed the platform section: %v", err)
	}
	settings, err = sources.WorkerEgressSettings(ctx)
	if err != nil || settings.Mode != platformconfig.WorkerEgressAllowlist || len(settings.Allowlist) != 2 || settings.Allowlist[1] != "*.atlassian.net" {
		t.Fatalf("saved: %+v %v", settings, err)
	}

	entries, err := sources.WorkerEgressProjectEntries(ctx, 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no row: %v %v", entries, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, created_at)
VALUES (gen_random_uuid(), 1, 'Worker egress', 'worker_egress', 'worker_egress', 'project_settings',
        '{"allowlist": ["registry.npmjs.org", "example.com:8443"]}'::jsonb, '{}'::jsonb, false, true, 'user', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("seed the project row: %v", err)
	}
	entries, err = sources.WorkerEgressProjectEntries(ctx, 1)
	if err != nil || len(entries) != 2 || entries[0] != "registry.npmjs.org" {
		t.Fatalf("row: %v %v", entries, err)
	}

	seedEgressExecution(t, pool, "exec-in", 1, 0)
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ('bundle-exec-in', 'settings', '1', 'request', 'application/json',
    decode(repeat('cd', 32), 'hex'), 2, 'inline://settings', 'internal', 'worker', '{}'::bytea)`); err != nil {
		t.Fatalf("seed an entry: %v", err)
	}
	read, err := sources.InputEntries(ctx, "bundle-exec-in", []string{"settings", "missing"})
	if err != nil || len(read) != 1 || string(read["settings"]) != "{}" {
		t.Fatalf("entries=%v err=%v", read, err)
	}
}
