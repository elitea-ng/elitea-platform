package browsersession

// The session records of ADR-0042 decision 5 (#1496): the sweep that stamps
// a session's expiry when no request meets it, and the relay that turns
// every lifecycle change into an audit event.
//
// WHY A RELAY AND NOT A WRITE AT EACH CALL SITE. A session ends in five
// places: a request meets an expired row (Manager.Validate), the sweep finds
// one, a browser signs out (Manager.Revoke), an operator signs a user out
// (Manager.RevokeUser), and a deactivation revokes every session inside the
// suspension's own transaction (nativeauth.RevokeUserSessions). The last
// one cannot write an event that must not exist if its caller rolls back.
// So every change is a ROW change (ended_at, end_reason, shared 0200) in the
// statement that makes it, and one relay writes the events: it inserts the
// centry.audit_events rows and stamps the row's mark in ONE transaction.
// An event is therefore written exactly once, after its change committed,
// and a relay that fails leaves the row for the next pass.
//
// THE EVENTS (centry.audit_events, so the audit trail and the SIEM export
// carry them; the export envelope maps event_type session to OCSF
// Authentication):
//
//	event_type  session
//	action      session.start | session.expire | session.logout | session.revoke
//	entity_type browser_session
//	entity_id   browser_sessions.ref (never the id: that is the cookie value)
//	entity_name provider=<oidc|saml|form> [reason=<idle|absolute>]
//	user_id, user_email
//	timestamp   created_at for a start, ended_at for an end
//	duration_ms 0 for a start; the session's age at its end
//
// A run a browser request admitted carries the same ref
// (execution_jobs.browser_session_ref, shared 0201), so
//
//	SELECT j.execution_id FROM elitea_runtime.execution_jobs j
//	JOIN centry.audit_events e ON e.entity_type = 'browser_session'
//	 AND e.entity_id = j.browser_session_ref AND e.action = 'session.start'
//
// lists the runs each session started.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LifecycleRoutineID is the routine registry row of the sweep and relay.
const LifecycleRoutineID = "main.browser_session_audit"

// LifecycleDefaultSchedule is how often the pass runs. An audit event is
// written within about this long of its change.
const LifecycleDefaultSchedule = "1m"

// The audit vocabulary.
const (
	AuditEventType  = "session"
	AuditEntityType = "browser_session"
	AuditStart      = "session.start"
	AuditExpire     = "session.expire"
	AuditLogout     = "session.logout"
	AuditRevoke     = "session.revoke"
)

// lifecycleBatch bounds one statement of a pass. A pass loops until a
// statement moves fewer rows, up to lifecycleMaxRounds statements.
const (
	lifecycleBatch     = 500
	lifecycleMaxRounds = 40
)

// Lifecycle runs the sweep and the relay.
type Lifecycle struct {
	pool   *pgxpool.Pool
	bounds BoundsSource
	now    func() time.Time
}

// NewLifecycle builds the pass. bounds may be nil (no administrator bounds).
func NewLifecycle(pool *pgxpool.Pool, bounds BoundsSource) (*Lifecycle, error) {
	if pool == nil {
		return nil, errors.New("browsersession: the lifecycle pass needs a database pool")
	}
	return &Lifecycle{pool: pool, bounds: bounds, now: time.Now}, nil
}

// LifecycleResult is what one pass did.
type LifecycleResult struct {
	Expired int64
	Events  int64
}

// Pass stamps the expired sessions and writes the pending events.
func (l *Lifecycle) Pass(ctx context.Context) (LifecycleResult, error) {
	var result LifecycleResult
	bounds := Bounds{}
	if l.bounds != nil {
		read, err := l.bounds.SessionBounds(ctx)
		if err != nil {
			return result, fmt.Errorf("browsersession: read the session policy: %w", err)
		}
		bounds = read
	}
	for round := 0; round < lifecycleMaxRounds; round++ {
		n, err := l.markExpired(ctx, bounds)
		if err != nil {
			return result, err
		}
		result.Expired += n
		if n < lifecycleBatch {
			break
		}
	}
	for round := 0; round < lifecycleMaxRounds; round++ {
		rows, events, err := l.relay(ctx)
		if err != nil {
			return result, err
		}
		result.Events += events
		// The stop rule counts rows taken, not events: a row can owe two.
		if rows < lifecycleBatch {
			break
		}
	}
	return result, nil
}

// markExpiredSQL stamps live rows past a deadline under the bounds, with the
// same rule as Session.Deadline: the effective idle timeout is the row's,
// clamped to the maximum ($2; 0 is none, and a row's 0 becomes the
// maximum); the effective absolute deadline is the earlier of expires_at and
// created_at + the maximum lifetime ($3). The row ends at the earlier
// deadline, and the reason names it.
const markExpiredSQL = `
WITH live AS (
    SELECT id,
           LEAST(expires_at,
                 CASE WHEN $3::bigint > 0
                      THEN created_at + $3::bigint * interval '1 second' END) AS absolute_at,
           CASE
               WHEN $2::bigint > 0 AND (idle_timeout_seconds = 0 OR idle_timeout_seconds > $2::bigint)
                   THEN last_seen_at + $2::bigint * interval '1 second'
               WHEN idle_timeout_seconds > 0
                   THEN last_seen_at + idle_timeout_seconds * interval '1 second'
           END AS idle_at
    FROM elitea_auth.browser_sessions
    WHERE ended_at IS NULL AND revoked_at IS NULL
), due AS (
    SELECT id,
           CASE WHEN idle_at IS NOT NULL AND idle_at < absolute_at THEN idle_at ELSE absolute_at END AS end_at,
           CASE WHEN idle_at IS NOT NULL AND idle_at < absolute_at THEN 'idle' ELSE 'absolute' END AS reason
    FROM live
), picked AS (
    SELECT s.id, due.end_at, due.reason
    FROM due
    JOIN elitea_auth.browser_sessions AS s ON s.id = due.id
    WHERE due.end_at <= $1::timestamptz
    LIMIT $4
    FOR UPDATE OF s SKIP LOCKED
)
UPDATE elitea_auth.browser_sessions AS s
SET ended_at = picked.end_at, end_reason = picked.reason
FROM picked
WHERE s.id = picked.id AND s.ended_at IS NULL AND s.revoked_at IS NULL`

func (l *Lifecycle) markExpired(ctx context.Context, bounds Bounds) (int64, error) {
	tag, err := l.pool.Exec(ctx, markExpiredSQL, l.now().UTC(),
		int64(bounds.MaxIdleTimeout/time.Second), int64(bounds.MaxAbsoluteLifetime/time.Second),
		lifecycleBatch)
	if err != nil {
		return 0, fmt.Errorf("browsersession: stamp expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// relaySQL writes one batch of events and stamps their marks, in one
// statement (one transaction). A row can owe two events (a start and an
// end); both are written in time order. SKIP LOCKED lets two replicas run a
// pass without writing an event twice.
const relaySQL = `
WITH picked AS (
    SELECT id, ref, user_id, email, provider, created_at, ended_at, end_reason,
           start_audited_at IS NULL AS owes_start,
           ended_at IS NOT NULL AND end_audited_at IS NULL AS owes_end
    FROM elitea_auth.browser_sessions
    WHERE start_audited_at IS NULL
       OR (ended_at IS NOT NULL AND end_audited_at IS NULL)
    ORDER BY COALESCE(ended_at, created_at)
    LIMIT $1
    FOR UPDATE SKIP LOCKED
), events AS (
    SELECT created_at AS at, ref, user_id, email, 'session.start' AS action,
           'provider=' || provider AS name, 0::double precision AS duration_ms
    FROM picked WHERE owes_start
    UNION ALL
    SELECT ended_at, ref, user_id, email,
           CASE end_reason
               WHEN 'logout' THEN 'session.logout'
               WHEN 'revoked' THEN 'session.revoke'
               ELSE 'session.expire'
           END,
           'provider=' || provider
               || CASE WHEN end_reason IN ('idle', 'absolute') THEN ' reason=' || end_reason ELSE '' END,
           GREATEST(EXTRACT(EPOCH FROM (ended_at - created_at)) * 1000, 0)::double precision
    FROM picked WHERE owes_end
), written AS (
    INSERT INTO centry.audit_events
        (timestamp, user_id, user_email, project_id, event_type, action,
         is_error, duration_ms, entity_type, entity_id, entity_name)
    SELECT at, user_id, NULLIF(left(email, 256), ''), NULL, 'session', action,
           FALSE, duration_ms, 'browser_session', ref, name
    FROM events
    ORDER BY at
    RETURNING 1
), marked AS (
    UPDATE elitea_auth.browser_sessions AS s
    SET start_audited_at = CASE WHEN picked.owes_start THEN clock_timestamp() ELSE s.start_audited_at END,
        end_audited_at = CASE WHEN picked.owes_end THEN clock_timestamp() ELSE s.end_audited_at END
    FROM picked
    WHERE s.id = picked.id
    RETURNING 1
)
SELECT (SELECT count(*) FROM marked), (SELECT count(*) FROM written)`

func (l *Lifecycle) relay(ctx context.Context) (rows, events int64, err error) {
	err = l.pool.QueryRow(ctx, relaySQL, lifecycleBatch).Scan(&rows, &events)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("browsersession: write session audit events: %w", err)
	}
	return rows, events, nil
}
