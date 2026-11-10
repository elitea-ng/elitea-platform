package repos

// The daily agent scorecard (ADR-0050 decision 5h, issue 1616): one row per
// (project, agent, day) from platform data only. It reads no transcript:
// run outcomes and cap stops (execution_jobs), breaker trips (suspensions of
// source breaker), approval rejections (approval_ledger), guardrail hits
// (guardrail_verdicts), spend (llm_request_logs priced as the breaker
// prices them), people's ratings (chat_message_feedback on the agent's
// answers) and the certification state.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// scorecardPlatformSQL computes every scorecard column that lives in the
// shared schemas for the agents of the registry, as of $1 (the day's end).
const scorecardPlatformSQL = `
WITH agents AS (
    SELECT tenant_project_id AS project_id, application_id, certification_state
    FROM elitea_lifecycle.agent_registry
    WHERE lifecycle_state = 'active'
),
runs AS (
    SELECT j.resource_project_id::bigint AS project_id, a.application_id, j.execution_id, j.state, j.cancel_reason,
           j.admitted_at, j.settled_at
    FROM elitea_runtime.execution_jobs AS j
    JOIN elitea_runtime.agent_execution_jobs AS a ON a.execution_id = j.execution_id AND a.generation = j.generation
    WHERE j.capability_id = 'agent.execute.application.v1' AND a.application_id IS NOT NULL
      AND j.admitted_at > $1::timestamptz - interval '30 days' AND j.admitted_at <= $1::timestamptz
),
run_counts AS (
    SELECT project_id, application_id,
           count(*) FILTER (WHERE settled_at IS NOT NULL) AS runs,
           count(*) FILTER (WHERE state = 'FAILED') AS failed,
           count(*) FILTER (WHERE cancel_reason = 'cap_reached') AS capped,
           max(admitted_at) AS last_used
    FROM runs GROUP BY project_id, application_id
),
last_use AS (
    SELECT j.resource_project_id::bigint AS project_id, a.application_id, max(j.admitted_at) AS last_used
    FROM elitea_runtime.execution_jobs AS j
    JOIN elitea_runtime.agent_execution_jobs AS a ON a.execution_id = j.execution_id AND a.generation = j.generation
    WHERE j.capability_id = 'agent.execute.application.v1' AND a.application_id IS NOT NULL
      AND j.admitted_at <= $1::timestamptz
    GROUP BY 1, 2
),
trips AS (
    SELECT project_id::bigint AS project_id, scope_key::bigint AS application_id, count(*) AS n
    FROM elitea_runtime.execution_suspensions
    WHERE scope = 'agent' AND source = 'breaker' AND scope_key ~ '^[0-9]{1,18}$'
      AND created_at > $1::timestamptz - interval '7 days' AND created_at <= $1::timestamptz
    GROUP BY 1, 2
),
rejections AS (
    SELECT r.project_id, r.application_id, count(*) AS n
    FROM elitea_runtime.approval_ledger AS l
    JOIN runs AS r ON r.execution_id = l.execution_id AND r.project_id = l.project_id
    WHERE l.outcome = 'rejected' AND l.created_at <= $1::timestamptz
    GROUP BY 1, 2
),
hits AS (
    SELECT r.project_id, r.application_id, count(*) AS n
    FROM gateway.guardrail_verdicts AS v
    JOIN runs AS r ON r.execution_id = v.execution_id
    WHERE v.outcome IN ('flagged', 'masked', 'blocked') AND v.occurred_at <= $1::timestamptz
    GROUP BY 1, 2
),
spend AS (
    SELECT r.project_id, r.application_id, sum(` + requestLogCostSQL + `)::double precision AS usd
    FROM gateway.llm_request_logs AS l
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model AND l.operation <> 'tool'
    JOIN runs AS r ON r.execution_id = l.execution_id
    WHERE l.occurred_at > $1::timestamptz - interval '30 days' AND l.occurred_at <= $1::timestamptz
    GROUP BY 1, 2
)
SELECT g.project_id, g.application_id, g.certification_state,
       COALESCE(rc.runs, 0), COALESCE(rc.failed, 0), COALESCE(rc.capped, 0),
       COALESCE(t.n, 0), COALESCE(rj.n, 0), COALESCE(h.n, 0), COALESCE(sp.usd, 0), lu.last_used
FROM agents AS g
LEFT JOIN run_counts AS rc ON rc.project_id = g.project_id AND rc.application_id = g.application_id
LEFT JOIN last_use AS lu ON lu.project_id = g.project_id AND lu.application_id = g.application_id
LEFT JOIN trips AS t ON t.project_id = g.project_id AND t.application_id = g.application_id
LEFT JOIN rejections AS rj ON rj.project_id = g.project_id AND rj.application_id = g.application_id
LEFT JOIN hits AS h ON h.project_id = g.project_id AND h.application_id = g.application_id
LEFT JOIN spend AS sp ON sp.project_id = g.project_id AND sp.application_id = g.application_id
ORDER BY g.project_id, g.application_id`

// ScorecardSummary is what one pass did.
type ScorecardSummary struct {
	Agents   int
	Projects int
	Pruned   int64
}

func (s ScorecardSummary) String() string {
	return fmt.Sprintf("agents=%d projects=%d pruned=%d", s.Agents, s.Projects, s.Pruned)
}

// ScorecardPass computes today's scorecard of every active agent and
// prunes the scorecards past retention.
func (s *AgentWorkforce) ScorecardPass(ctx context.Context) (ScorecardSummary, error) {
	now := s.now().UTC()
	day := now.Truncate(24 * time.Hour)
	rows, err := s.pool.Query(ctx, scorecardPlatformSQL, now)
	if err != nil {
		return ScorecardSummary{}, fmt.Errorf("agent workforce: compute the scorecards: %w", err)
	}
	type key struct{ project, application int64 }
	cards := map[key]*agentworkforce.Scorecard{}
	byProject := map[int64][]int64{}
	order := []key{}
	for rows.Next() {
		var (
			k    key
			card agentworkforce.Scorecard
		)
		if err := rows.Scan(&k.project, &k.application, &card.CertificationState, &card.Runs30d, &card.Failed30d,
			&card.CapReached30d, &card.BreakerTrips7d, &card.ApprovalRejections30d, &card.GuardrailHits30d,
			&card.Spend30dUSD, &card.LastUsedAt); err != nil {
			rows.Close()
			return ScorecardSummary{}, fmt.Errorf("agent workforce: scan a scorecard: %w", err)
		}
		cards[k] = &card
		byProject[k.project] = append(byProject[k.project], k.application)
		order = append(order, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ScorecardSummary{}, fmt.Errorf("agent workforce: compute the scorecards: %w", err)
	}
	summary := ScorecardSummary{Projects: len(byProject)}
	for project, applications := range byProject {
		feedback, err := s.projectFeedback(ctx, project, applications, now)
		if err != nil {
			return summary, err
		}
		spends := make([]float64, 0, len(applications))
		for _, application := range applications {
			card := cards[key{project, application}]
			if counts, ok := feedback[application]; ok {
				card.ThumbsUp30d, card.ThumbsDown30d = counts[0], counts[1]
			}
			spends = append(spends, card.Spend30dUSD)
		}
		median := medianOf(spends)
		for _, application := range applications {
			cards[key{project, application}].ProjectMedianSpendUSD = median
		}
	}
	for _, k := range order {
		card := cards[k]
		if _, err := s.pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_scorecards
    (project_id, application_id, day, runs_30d, failed_30d, cap_reached_30d, breaker_trips_7d,
     approval_rejections_30d, guardrail_hits_30d, spend_30d_usd, project_median_spend_usd,
     thumbs_up_30d, thumbs_down_30d, last_used_at, certification_state, computed_at)
VALUES ($1, $2, $3::date, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, clock_timestamp())
ON CONFLICT (project_id, application_id, day) DO UPDATE SET
    runs_30d = EXCLUDED.runs_30d, failed_30d = EXCLUDED.failed_30d, cap_reached_30d = EXCLUDED.cap_reached_30d,
    breaker_trips_7d = EXCLUDED.breaker_trips_7d, approval_rejections_30d = EXCLUDED.approval_rejections_30d,
    guardrail_hits_30d = EXCLUDED.guardrail_hits_30d, spend_30d_usd = EXCLUDED.spend_30d_usd,
    project_median_spend_usd = EXCLUDED.project_median_spend_usd, thumbs_up_30d = EXCLUDED.thumbs_up_30d,
    thumbs_down_30d = EXCLUDED.thumbs_down_30d, last_used_at = EXCLUDED.last_used_at,
    certification_state = EXCLUDED.certification_state, computed_at = EXCLUDED.computed_at`,
			k.project, k.application, day, card.Runs30d, card.Failed30d, card.CapReached30d, card.BreakerTrips7d,
			card.ApprovalRejections30d, card.GuardrailHits30d, card.Spend30dUSD, card.ProjectMedianSpendUSD,
			card.ThumbsUp30d, card.ThumbsDown30d, card.LastUsedAt, card.CertificationState); err != nil {
			return summary, fmt.Errorf("agent workforce: store a scorecard: %w", err)
		}
		summary.Agents++
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM elitea_lifecycle.agent_scorecards WHERE day < $1::date`,
		day.Add(-agentworkforce.ScorecardRetention))
	if err != nil {
		return summary, fmt.Errorf("agent workforce: prune the scorecards: %w", err)
	}
	summary.Pruned = tag.RowsAffected()
	return summary, nil
}

// projectFeedback counts the thumbs up and down people gave the agents'
// answers in the project in the last 30 days.
func (s *AgentWorkforce) projectFeedback(ctx context.Context, projectID int64, applications []int64, now time.Time) (map[int64][2]int, error) {
	out := map[int64][2]int{}
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return out, nil
	}
	exists, err := relationExists(ctx, s.pool, schema+".chat_message_feedback")
	if err != nil || !exists {
		return out, err
	}
	ids := make([]string, 0, len(applications))
	for _, id := range applications {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT (participant.entity_meta ->> 'id')::bigint,
       count(*) FILTER (WHERE feedback.rating = 1), count(*) FILTER (WHERE feedback.rating = -1)
FROM %[1]s.chat_message_feedback AS feedback
JOIN %[1]s.chat_message_group AS message ON message.uuid = feedback.message_group_uuid
JOIN %[1]s.chat_participants AS participant
  ON participant.id = message.author_participant_id AND participant.entity_name = 'application'
WHERE participant.entity_meta ->> 'id' = ANY($1::text[])
  AND feedback.updated_at > ($2::timestamptz - interval '30 days') AT TIME ZONE 'UTC'
GROUP BY 1`, schema), ids, now)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: count the ratings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id       int64
			up, down int
		)
		if err := rows.Scan(&id, &up, &down); err != nil {
			return nil, err
		}
		out[id] = [2]int{up, down}
	}
	return out, rows.Err()
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

const scorecardColumns = `day::text, runs_30d, failed_30d, cap_reached_30d, breaker_trips_7d, approval_rejections_30d,
guardrail_hits_30d, spend_30d_usd, project_median_spend_usd, thumbs_up_30d, thumbs_down_30d, last_used_at,
certification_state, computed_at`

func scanScorecard(row pgx.Row, now time.Time) (agentworkforce.Scorecard, error) {
	var card agentworkforce.Scorecard
	if err := row.Scan(&card.Day, &card.Runs30d, &card.Failed30d, &card.CapReached30d, &card.BreakerTrips7d,
		&card.ApprovalRejections30d, &card.GuardrailHits30d, &card.Spend30dUSD, &card.ProjectMedianSpendUSD,
		&card.ThumbsUp30d, &card.ThumbsDown30d, &card.LastUsedAt, &card.CertificationState, &card.ComputedAt); err != nil {
		return agentworkforce.Scorecard{}, err
	}
	card.Finish(now)
	return card, nil
}

// latestScorecard answers the agent's newest scorecard, or nil when the
// routine has not scored it yet.
func latestScorecard(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64, now time.Time) (*agentworkforce.Scorecard, error) {
	card, err := scanScorecard(q.QueryRow(ctx, `SELECT `+scorecardColumns+`
FROM elitea_lifecycle.agent_scorecards WHERE project_id = $1 AND application_id = $2
ORDER BY day DESC LIMIT 1`, projectID, applicationID), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent workforce: read the scorecard: %w", err)
	}
	return &card, nil
}

// Scorecards answers the agent's daily scorecards, newest first.
func (s *AgentWorkforce) Scorecards(ctx context.Context, projectID, applicationID int64, days int) ([]agentworkforce.Scorecard, error) {
	if days <= 0 || days > 400 {
		days = 30
	}
	now := s.now().UTC()
	rows, err := s.pool.Query(ctx, `SELECT `+scorecardColumns+`
FROM elitea_lifecycle.agent_scorecards WHERE project_id = $1 AND application_id = $2
ORDER BY day DESC LIMIT $3`, projectID, applicationID, days)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: read the scorecards: %w", err)
	}
	defer rows.Close()
	out := []agentworkforce.Scorecard{}
	for rows.Next() {
		card, err := scanScorecard(rows, now)
		if err != nil {
			return nil, fmt.Errorf("agent workforce: read the scorecards: %w", err)
		}
		out = append(out, card)
	}
	return out, rows.Err()
}
