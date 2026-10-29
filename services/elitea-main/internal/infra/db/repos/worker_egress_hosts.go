package repos

import (
	"context"
	"fmt"
	"time"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
)

// The admin view of the hosts runs reached and were refused (ADR-0044
// decision 2), aggregated from elitea_runtime.egress_run_summaries (shared
// 0169) per project, agent and host. A run's agent is the application of its
// agent execution job (agent_execution_jobs.application_id, shared 0194);
// every other execution (an ad hoc chat, a tool call, an index run) has none.
// The rows read are whatever the retention kept
// (ELITEA_EGRESS_AUDIT_RETENTION_DAYS, default 30).

const egressHostRows = `
SELECT s.execution_id, s.project_id, s.updated_at, s.%[1]s AS hosts,
       (SELECT a.application_id FROM elitea_runtime.agent_execution_jobs AS a
        WHERE a.execution_id = s.execution_id AND a.application_id IS NOT NULL
        ORDER BY a.generation DESC LIMIT 1) AS agent_id
FROM elitea_runtime.egress_run_summaries AS s
WHERE s.updated_at >= $1
  AND ($2::text = 'all' OR ($2::text = 'worker') = (s.replica = 'worker' OR s.replica LIKE 'worker:%%'))
  AND ($3::bigint = 0 OR s.project_id = $3::bigint)`

// Hosts answers one admin query. Each list is cut at query.Limit, most
// used first, and the report says so.
func (s *EgressRunSummaries) Hosts(ctx context.Context, query app.HostsQuery) (app.HostsReport, error) {
	report := app.HostsReport{Reached: []app.ReachedHost{}, Refused: []app.RefusedHost{}}
	source := string(query.Source)
	limit := query.Limit
	if limit <= 0 || limit > app.MaxHostsRows {
		limit = app.MaxHostsRows
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
WITH runs AS (`+egressHostRows+`)
SELECT r.project_id, r.agent_id, h.value->>'host', COALESCE((h.value->>'port')::int, 0),
       sum(COALESCE((h.value->>'connections')::bigint, 0)), count(DISTINCT r.execution_id), max(r.updated_at)
FROM runs AS r CROSS JOIN LATERAL jsonb_array_elements(r.hosts) AS h(value)
GROUP BY 1, 2, 3, 4
ORDER BY 5 DESC, 3, 4, 1, 2 NULLS FIRST
LIMIT $4`, "hosts_reached"), query.Since, source, query.ProjectID, limit+1)
	if err != nil {
		return app.HostsReport{}, fmt.Errorf("read the hosts reached: %w", err)
	}
	for rows.Next() {
		var host app.ReachedHost
		var last time.Time
		if err := rows.Scan(&host.ProjectID, &host.AgentID, &host.Host, &host.Port,
			&host.Connections, &host.Executions, &last); err != nil {
			rows.Close()
			return app.HostsReport{}, fmt.Errorf("read a host reached: %w", err)
		}
		host.LastSeen = last.UTC()
		report.Reached = append(report.Reached, host)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return app.HostsReport{}, fmt.Errorf("read the hosts reached: %w", err)
	}
	if len(report.Reached) > limit {
		report.Reached, report.ReachedTruncated = report.Reached[:limit], true
	}

	rows, err = s.pool.Query(ctx, fmt.Sprintf(`
WITH runs AS (`+egressHostRows+`)
SELECT r.project_id, r.agent_id, h.value->>'host', COALESCE((h.value->>'port')::int, 0), h.value->>'reason',
       sum(COALESCE((h.value->>'count')::bigint, 0)), count(DISTINCT r.execution_id), max(r.updated_at)
FROM runs AS r CROSS JOIN LATERAL jsonb_array_elements(r.hosts) AS h(value)
GROUP BY 1, 2, 3, 4, 5
ORDER BY 6 DESC, 3, 4, 5, 1, 2 NULLS FIRST
LIMIT $4`, "hosts_refused"), query.Since, source, query.ProjectID, limit+1)
	if err != nil {
		return app.HostsReport{}, fmt.Errorf("read the hosts refused: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var host app.RefusedHost
		var last time.Time
		if err := rows.Scan(&host.ProjectID, &host.AgentID, &host.Host, &host.Port, &host.Reason,
			&host.Count, &host.Executions, &last); err != nil {
			return app.HostsReport{}, fmt.Errorf("read a host refused: %w", err)
		}
		host.LastSeen = last.UTC()
		report.Refused = append(report.Refused, host)
	}
	if err := rows.Err(); err != nil {
		return app.HostsReport{}, fmt.Errorf("read the hosts refused: %w", err)
	}
	if len(report.Refused) > limit {
		report.Refused, report.RefusedTruncated = report.Refused[:limit], true
	}
	return report, nil
}
