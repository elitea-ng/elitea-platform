package repos

// The journal of an agent's memory vectors (shared/0248, trigger tenant/0248,
// issue 1657). A trigger on every tenant's agent_memory_entries writes a row
// when an entry changes; agentmemoryvector's reconciler claims rows here,
// reads each entry as it is now (Entries), embeds or removes its point, and
// closes the row at the generation it read (Complete).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// AgentMemoryVectorJournal reads and writes the journal.
type AgentMemoryVectorJournal struct {
	pool *pgxpool.Pool
}

var _ agentmemoryvector.Journal = (*AgentMemoryVectorJournal)(nil)

// NewAgentMemoryVectorJournal builds the journal over pool.
func NewAgentMemoryVectorJournal(pool *pgxpool.Pool) (*AgentMemoryVectorJournal, error) {
	if pool == nil {
		return nil, errors.New("agent memory vector journal: a pool is required")
	}
	return &AgentMemoryVectorJournal{pool: pool}, nil
}

// Claim leases up to limit due rows for lease, in one short transaction
// (FOR UPDATE SKIP LOCKED), so replicas never work the same row.
func (j *AgentMemoryVectorJournal) Claim(ctx context.Context, limit int, lease time.Duration) ([]agentmemoryvector.Job, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := j.pool.Query(ctx, `
WITH due AS (
    SELECT project_id, entry_id
    FROM centry.agent_memory_vector_journal
    WHERE next_attempt_at <= now() AND (claimed_until IS NULL OR claimed_until < now())
    ORDER BY next_attempt_at, project_id, entry_id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE centry.agent_memory_vector_journal AS journal
SET claimed_until = now() + make_interval(secs => $2), attempts = journal.attempts + 1
FROM due
WHERE journal.project_id = due.project_id AND journal.entry_id = due.entry_id
RETURNING journal.project_id, journal.entry_id, journal.application_id, journal.generation, journal.attempts`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("agent memory vector journal: claim: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentmemoryvector.Job, error) {
		var job agentmemoryvector.Job
		err := row.Scan(&job.ProjectID, &job.EntryID, &job.ApplicationID, &job.Generation, &job.Attempts)
		return job, err
	})
}

// Entries answers which of ids are active, non-cursor entries of the project
// right now, by id. A project whose schema is gone has none.
func (j *AgentMemoryVectorJournal) Entries(ctx context.Context, projectID int64, ids []int64) (map[int64]agentmemoryvector.Entry, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := j.pool.Query(ctx, fmt.Sprintf(`
SELECT id, application_id, kind, content, version
FROM %s.agent_memory_entries
WHERE id = ANY($1::bigint[]) AND status = 'active' AND kind <> 'cursor'`, schema), ids)
	if err != nil {
		if isUndefinedSchemaObject(err) {
			return map[int64]agentmemoryvector.Entry{}, nil
		}
		return nil, fmt.Errorf("agent memory vector journal: read entries: %w", err)
	}
	found := make(map[int64]agentmemoryvector.Entry, len(ids))
	for rows.Next() {
		var entry agentmemoryvector.Entry
		if err := rows.Scan(&entry.ID, &entry.ApplicationID, &entry.Kind, &entry.Content, &entry.Version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("agent memory vector journal: scan: %w", err)
		}
		found[entry.ID] = entry
	}
	if err := rows.Err(); err != nil {
		if isUndefinedSchemaObject(err) {
			return map[int64]agentmemoryvector.Entry{}, nil
		}
		return nil, fmt.Errorf("agent memory vector journal: read entries: %w", err)
	}
	return found, nil
}

// isUndefinedSchemaObject reports an undefined schema (3F000) or table
// (42P01): the project's tenant schema was dropped.
func isUndefinedSchemaObject(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "3F000" || pgErr.Code == "42P01")
}

// Complete closes the rows at the generations the jobs read. A row the trigger
// reopened meanwhile (a higher generation) stays.
func (j *AgentMemoryVectorJournal) Complete(ctx context.Context, jobs []agentmemoryvector.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	projects, entries, generations := splitJobs(jobs)
	_, err := j.pool.Exec(ctx, `
DELETE FROM centry.agent_memory_vector_journal AS journal
USING unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS done(project_id, entry_id, generation)
WHERE journal.project_id = done.project_id AND journal.entry_id = done.entry_id
  AND journal.generation = done.generation`, projects, entries, generations)
	if err != nil {
		return fmt.Errorf("agent memory vector journal: complete: %w", err)
	}
	return nil
}

// Fail releases the rows for another run after backoff and records the class
// of the failure (never a raw error), at the generations the jobs read.
func (j *AgentMemoryVectorJournal) Fail(ctx context.Context, jobs []agentmemoryvector.Job, class string, backoff time.Duration) error {
	if len(jobs) == 0 {
		return nil
	}
	projects, entries, generations := splitJobs(jobs)
	_, err := j.pool.Exec(ctx, `
UPDATE centry.agent_memory_vector_journal AS journal
SET last_error = $4, claimed_until = NULL, next_attempt_at = now() + make_interval(secs => $5)
FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS failed(project_id, entry_id, generation)
WHERE journal.project_id = failed.project_id AND journal.entry_id = failed.entry_id
  AND journal.generation = failed.generation`, projects, entries, generations, class, backoff.Seconds())
	if err != nil {
		return fmt.Errorf("agent memory vector journal: fail: %w", err)
	}
	return nil
}

func splitJobs(jobs []agentmemoryvector.Job) (projects, entries, generations []int64) {
	projects = make([]int64, len(jobs))
	entries = make([]int64, len(jobs))
	generations = make([]int64, len(jobs))
	for i, job := range jobs {
		projects[i], entries[i], generations[i] = job.ProjectID, job.EntryID, job.Generation
	}
	return projects, entries, generations
}
