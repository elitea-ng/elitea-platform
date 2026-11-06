package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
)

// BrowserSessions is elitea_runtime.browser_sessions (shared/0228,
// ADR-0049 decision 6, #1597): one row per cloud browser session. The
// worker's ReportBrowserSession writes it; the view relay reads it on any
// elitea-main replica, so no replica keeps session state of its own.
type BrowserSessions struct {
	pool *pgxpool.Pool
}

// ErrBrowserSessionRefused is a report Main does not store: no job row for
// the reported generation, a name that is not the run's, a session of
// another run, an older generation, or a move backwards.
var ErrBrowserSessionRefused = errors.New("browser session report refused")

// NewBrowserSessions reads and writes through pool.
func NewBrowserSessions(pool *pgxpool.Pool) (*BrowserSessions, error) {
	if pool == nil {
		return nil, errors.New("browser sessions require a database pool")
	}
	return &BrowserSessions{pool: pool}, nil
}

// BrowserSessionReport is one report from the worker. The execution and
// the generation are the verified fence's; everything else about the run
// (tenant, project, owner, conversation) is read from the execution kernel.
type BrowserSessionReport struct {
	ExecutionID        string
	Generation         uint64
	SupervisorAudience string
	Spec               browsersession.Spec
	State              browsersession.State
	LingerUntil        time.Time
}

// Record upserts the session row in one statement. A new session takes its
// run from execution_jobs (and agent_execution_jobs for the conversation)
// at the reported generation. An existing row moves forward only, only
// under the same run, the same supervisor and spec, and a generation no
// older than the stored one. It returns ErrBrowserSessionRefused when
// nothing matched.
func (s *BrowserSessions) Record(ctx context.Context, report BrowserSessionReport) error {
	if report.ExecutionID == "" || report.Generation == 0 || report.Generation > math.MaxInt64 ||
		report.SupervisorAudience == "" || browsersession.ValidateSpec(report.Spec) != nil ||
		report.State.Rank() == 0 || report.Spec.Name != browsersession.NameFor(report.ExecutionID) {
		return ErrBrowserSessionRefused
	}
	var linger *time.Time
	if report.State == browsersession.StateLingering && !report.LingerUntil.IsZero() {
		at := report.LingerUntil.UTC()
		linger = &at
	}
	spec := report.Spec
	tag, err := s.pool.Exec(ctx, `
WITH run AS (
    SELECT job.execution_id, job.generation, job.tenant_id, job.resource_project_id, job.actor_id,
           CASE WHEN agent.client_stream_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN agent.client_stream_id::uuid END AS conversation_uuid
    FROM elitea_runtime.execution_jobs AS job
    LEFT JOIN elitea_runtime.agent_execution_jobs AS agent
      ON agent.execution_id = job.execution_id AND agent.generation = job.generation
    WHERE job.execution_id = $2::text AND job.generation = $3::bigint
      AND job.actor_id ~ '^[1-9][0-9]{0,18}$'
)
INSERT INTO elitea_runtime.browser_sessions AS s (
    session_id, execution_id, generation, tenant_id, project_id, owner_actor_id, conversation_uuid,
    supervisor_audience, spec_revision, spec_engine, spec_cpu_millis, spec_memory_bytes,
    spec_wall_seconds, spec_max_pages, state, linger_until, ended_at)
SELECT $1::text, run.execution_id, run.generation, run.tenant_id, run.resource_project_id, run.actor_id,
       run.conversation_uuid, $4::text, $5::integer, $6::integer, $7::bigint, $8::bigint, $9::bigint,
       $10::integer, $11::text, $12::timestamptz,
       CASE WHEN $11::text = 'ended' THEN clock_timestamp() END
FROM run
ON CONFLICT (session_id) DO UPDATE SET
    generation   = EXCLUDED.generation,
    state        = EXCLUDED.state,
    linger_until = COALESCE(EXCLUDED.linger_until, s.linger_until),
    ended_at     = COALESCE(s.ended_at, EXCLUDED.ended_at),
    updated_at   = clock_timestamp()
WHERE s.execution_id = EXCLUDED.execution_id
  AND s.supervisor_audience = EXCLUDED.supervisor_audience
  AND s.spec_revision = EXCLUDED.spec_revision AND s.spec_engine = EXCLUDED.spec_engine
  AND s.spec_cpu_millis = EXCLUDED.spec_cpu_millis AND s.spec_memory_bytes = EXCLUDED.spec_memory_bytes
  AND s.spec_wall_seconds = EXCLUDED.spec_wall_seconds AND s.spec_max_pages = EXCLUDED.spec_max_pages
  AND EXCLUDED.generation >= s.generation
  AND (CASE EXCLUDED.state WHEN 'live' THEN 1 WHEN 'lingering' THEN 2 ELSE 3 END)
      >= (CASE s.state WHEN 'live' THEN 1 WHEN 'lingering' THEN 2 ELSE 3 END)`,
		spec.Name, report.ExecutionID, int64(report.Generation), report.SupervisorAudience, //nolint:gosec // bounded above
		int32(spec.Revision), spec.Engine, int64(spec.CPUMillis), int64(spec.MemoryBytes), //nolint:gosec // bounded by ValidateSpec
		int64(spec.WallSeconds), int32(spec.MaxPages), string(report.State), linger) //nolint:gosec // bounded by ValidateSpec
	if err != nil {
		return fmt.Errorf("record browser session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrBrowserSessionRefused
	}
	return nil
}

// Lookup reads one session by its id. It answers false for an id with no
// row (and for a malformed id, without a query).
func (s *BrowserSessions) Lookup(ctx context.Context, sessionID string) (browsersession.Session, bool, error) {
	if !browsersession.ValidName(sessionID) {
		return browsersession.Session{}, false, nil
	}
	var (
		row          browsersession.Session
		generation   int64
		project      int32
		conversation *string
		revision     int32
		engine       int32
		cpu, memory  int64
		wall         int64
		pages        int32
		state        string
		linger       *time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT session_id, execution_id, generation, tenant_id, project_id, owner_actor_id,
       conversation_uuid::text, supervisor_audience, spec_revision, spec_engine, spec_cpu_millis,
       spec_memory_bytes, spec_wall_seconds, spec_max_pages, state, linger_until
FROM elitea_runtime.browser_sessions WHERE session_id = $1::text`, sessionID).Scan(
		&row.SessionID, &row.ExecutionID, &generation, &row.TenantID, &project, &row.OwnerActorID,
		&conversation, &row.SupervisorAudience, &revision, &engine, &cpu, &memory, &wall, &pages, &state, &linger)
	if errors.Is(err, pgx.ErrNoRows) {
		return browsersession.Session{}, false, nil
	}
	if err != nil {
		return browsersession.Session{}, false, fmt.Errorf("read browser session: %w", err)
	}
	if generation <= 0 || project <= 0 || revision < 0 || cpu <= 0 || memory <= 0 || wall <= 0 || pages <= 0 {
		return browsersession.Session{}, false, errors.New("read browser session: the row is out of range")
	}
	row.Generation = uint64(generation)
	row.ProjectID = int64(project)
	if conversation != nil {
		row.ConversationUUID = *conversation
	}
	row.Spec = browsersession.Spec{
		Revision: uint32(revision), Name: row.SessionID, Engine: engine,
		CPUMillis: uint64(cpu), MemoryBytes: uint64(memory), WallSeconds: uint64(wall), MaxPages: uint32(pages),
	}
	row.State = browsersession.State(state)
	if linger != nil {
		row.LingerUntil = linger.UTC()
	}
	if _, err := strconv.ParseInt(row.OwnerActorID, 10, 64); err != nil {
		return browsersession.Session{}, false, errors.New("read browser session: the owner is malformed")
	}
	return row, true, nil
}
