package repos

// The heartbeat of proactive agents (ADR-0050 decision 2, issue 1618): the
// Postgres half of internal/application/heartbeat, and the settings and
// history reads of the heartbeat routes.
//
// Tables: tenant 0166 (agent_heartbeat_settings,
// project_proactivity_settings) and shared 0235
// (elitea_runtime.agent_heartbeats). Every tenant statement names its
// schema through tenantschema, never through the catalogue string.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/heartbeat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// HeartbeatConversationSource is chat_conversations.source of an agent's
// home thread and of an act's new conversation.
const HeartbeatConversationSource = "agent_heartbeat"

// HeartbeatNotificationEventType is centry.notifications.event_type of a
// heartbeat message (a mention-class notification, ADR-0050 decision 2a).
const HeartbeatNotificationEventType = "agent_heartbeat"

// ErrHeartbeatNotFound is an agent with no heartbeat row.
var ErrHeartbeatNotFound = errors.New("agent heartbeat: no settings row")

// AgentHeartbeats is the store.
type AgentHeartbeats struct {
	pool *pgxpool.Pool
}

// NewAgentHeartbeats builds the store.
func NewAgentHeartbeats(pool *pgxpool.Pool) *AgentHeartbeats { return &AgentHeartbeats{pool: pool} }

var _ heartbeat.Store = (*AgentHeartbeats)(nil)

// Maintenance reads the maintenance switch. Every failure reads as off, as
// the pipeline schedule tick reads it.
func (s *AgentHeartbeats) Maintenance(ctx context.Context) bool {
	state, err := platformconfig.LoadMaintenance(ctx, s.pool)
	return err == nil && state.Enabled
}

// Settings reads the platform section.
func (s *AgentHeartbeats) Settings(ctx context.Context) (platformconfig.ProactivitySettings, error) {
	return platformconfig.LoadProactivitySettings(ctx, s.pool)
}

// Projects lists the tenant schemas that hold agent_heartbeat_settings, as
// the pipeline schedule tick lists its own.
func (s *AgentHeartbeats) Projects(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
SELECT namespace.nspname
  FROM pg_class AS table_entry
  JOIN pg_namespace AS namespace ON namespace.oid = table_entry.relnamespace
 WHERE table_entry.relname = 'agent_heartbeat_settings'
   AND table_entry.relkind = 'r'
   AND namespace.nspname ~ '^p_[0-9]+$'
 ORDER BY namespace.nspname`)
	if err != nil {
		return nil, fmt.Errorf("agent heartbeat: list the projects: %w", err)
	}
	defer rows.Close()
	var projects []int64
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(name, "p_"), 10, 64); err == nil && id > 0 {
			projects = append(projects, id)
		}
	}
	return projects, rows.Err()
}

// ProjectEnabled reads proactive_agents_enabled. No row: on.
func (s *AgentHeartbeats) ProjectEnabled(ctx context.Context, projectID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, err
	}
	var enabled bool
	err = s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT proactive_agents_enabled FROM %s.project_proactivity_settings WHERE id = 1`, schema)).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("agent heartbeat: read the project switch: %w", err)
	}
	return enabled, nil
}

// SetProjectEnabled writes proactive_agents_enabled.
func (s *AgentHeartbeats) SetProjectEnabled(ctx context.Context, projectID, actorID int64, enabled bool) error {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.project_proactivity_settings (id, proactive_agents_enabled, updated_by)
VALUES (1, $1, NULLIF($2, 0))
ON CONFLICT (id) DO UPDATE SET proactive_agents_enabled = EXCLUDED.proactive_agents_enabled,
    updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp()`, schema), enabled, actorID)
	if err != nil {
		return fmt.Errorf("agent heartbeat: write the project switch: %w", err)
	}
	return nil
}

const heartbeatAgentColumns = `application_id, owner_user_id, COALESCE(mandate_id::text, ''),
       COALESCE(home_conversation_uuid::text, ''), paused_until, cursor_at`

func scanHeartbeatAgent(projectID int64, row pgx.Row) (heartbeat.Agent, error) {
	agent := heartbeat.Agent{ProjectID: projectID}
	err := row.Scan(&agent.ApplicationID, &agent.OwnerUserID, &agent.MandateID,
		&agent.HomeConversationUUID, &agent.PausedUntil, &agent.CursorAt)
	return agent, err
}

// DueAgents lists the enabled agents whose next wake has come.
func (s *AgentHeartbeats) DueAgents(ctx context.Context, projectID int64, now time.Time, limit int) ([]heartbeat.Agent, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT `+heartbeatAgentColumns+`
FROM %s.agent_heartbeat_settings
WHERE enabled AND next_wake_at <= $1
ORDER BY next_wake_at, application_id
LIMIT $2`, schema), now, limit)
	if err != nil {
		return nil, fmt.Errorf("agent heartbeat: list the due agents: %w", err)
	}
	defer rows.Close()
	var agents []heartbeat.Agent
	for rows.Next() {
		agent, err := scanHeartbeatAgent(projectID, rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// Claim moves a still-due agent's next wake to until, in one statement, so
// two passes cannot both take it: the second finds next_wake_at in the
// future and claims nothing.
func (s *AgentHeartbeats) Claim(ctx context.Context, agent heartbeat.Agent, now, until time.Time) (bool, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings
SET next_wake_at = $3, updated_at = clock_timestamp()
WHERE application_id = $1 AND enabled AND next_wake_at <= $2`, schema), agent.ApplicationID, now, until)
	if err != nil {
		return false, fmt.Errorf("agent heartbeat: claim the wake: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CurrentRelease reads the agent's published release: its version's meta
// and soul goals, the agent name, the registry tier, whether an approved
// change request published it, and whether the project is the owner's
// personal project.
func (s *AgentHeartbeats) CurrentRelease(ctx context.Context, agent heartbeat.Agent) (heartbeat.Release, bool, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return heartbeat.Release{}, false, err
	}
	var (
		release heartbeat.Release
		meta    []byte
		goals   []byte
		tier    *string
	)
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT r.id, r.application_version_id, COALESCE(a.name, ''),
       COALESCE(NULLIF(v.meta::jsonb, 'null'::jsonb), '{}'::jsonb),
       COALESCE(CASE WHEN jsonb_typeof(v.soul -> 'proactive_goals') = 'array'
                     THEN v.soul -> 'proactive_goals' END, '[]'::jsonb),
       g.risk_tier,
       EXISTS (SELECT 1 FROM elitea_lifecycle.change_requests AS c
               WHERE c.tenant_project_id = r.tenant_project_id AND c.application_id = r.application_id
                 AND c.release_id = r.id AND c.state = 'published' AND c.kind = 'release'
                 AND c.required_approvals >= 1 AND NOT c.self_approved),
       EXISTS (SELECT 1 FROM centry.project AS p WHERE p.id = $1 AND p.name = $3)
FROM elitea_lifecycle.application_release_heads AS h
JOIN elitea_lifecycle.application_releases AS r
  ON r.id = h.release_id AND r.state = 'published'
JOIN %[1]s.application_versions AS v ON v.id = r.application_version_id
LEFT JOIN %[1]s.applications AS a ON a.id = r.application_id
LEFT JOIN elitea_lifecycle.agent_registry AS g
  ON g.tenant_project_id = h.tenant_project_id AND g.application_id = h.application_id
WHERE h.tenant_project_id = $1 AND h.application_id = $2`, schema),
		agent.ProjectID, agent.ApplicationID, "project_user_"+strconv.FormatInt(agent.OwnerUserID, 10),
	).Scan(&release.ReleaseID, &release.VersionID, &release.AgentName, &meta, &goals, &tier,
		&release.Reviewed, &release.Personal)
	if errors.Is(err, pgx.ErrNoRows) {
		return heartbeat.Release{}, false, nil
	}
	if err != nil {
		return heartbeat.Release{}, false, fmt.Errorf("agent heartbeat: read the release: %w", err)
	}
	if err := json.Unmarshal(meta, &release.Meta); err != nil {
		return heartbeat.Release{}, false, fmt.Errorf("agent heartbeat: the release meta is not an object: %w", err)
	}
	var rawGoals []any
	_ = json.Unmarshal(goals, &rawGoals)
	for _, goal := range rawGoals {
		if text, ok := goal.(string); ok && strings.TrimSpace(text) != "" {
			release.Goals = append(release.Goals, text)
		}
	}
	release.Tier = agentlifecycle.TierLow
	if tier != nil && agentlifecycle.Tier(*tier).Valid() {
		release.Tier = agentlifecycle.Tier(*tier)
	}
	return release, true, nil
}

// Reschedule moves the next wake.
func (s *AgentHeartbeats) Reschedule(ctx context.Context, agent heartbeat.Agent, next time.Time, skipReason string) error {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings
SET next_wake_at = $2, last_skip_reason = $3, updated_at = clock_timestamp()
WHERE application_id = $1`, schema), agent.ApplicationID, next, skipReason)
	if err != nil {
		return fmt.Errorf("agent heartbeat: reschedule: %w", err)
	}
	return nil
}

// RunActive reports a message that an act of this agent started and that
// still streams or waits (parked on an approval, an authorization or an
// output limit, chat_group_meta_paused). A message older than a day no
// longer counts, so one wedged run cannot stop the heartbeat for good.
func (s *AgentHeartbeats) RunActive(ctx context.Context, agent heartbeat.Agent) (bool, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return false, err
	}
	var active bool
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1
      FROM %[1]s.chat_message_group AS response
      JOIN %[1]s.chat_conversations AS conversation ON conversation.id = response.conversation_id
     WHERE conversation.source = $1
       AND conversation.meta ->> 'heartbeat_application_id' = $2
       AND (response.is_streaming OR %[1]s.chat_group_meta_paused(response.meta))
       AND response.created_at > (now() AT TIME ZONE 'UTC') - interval '1 day'
)`, schema), HeartbeatConversationSource, strconv.FormatInt(agent.ApplicationID, 10)).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("agent heartbeat: read the overlap: %w", err)
	}
	return active, nil
}

// DayEnvelope reads the day so far: the triaged wakes, the acts, and the
// spend of the triage calls and of every run the acts started (the run, its
// run brief and its goal check).
func (s *AgentHeartbeats) DayEnvelope(ctx context.Context, agent heartbeat.Agent, since time.Time) (heartbeat.Day, error) {
	var day heartbeat.Day
	err := s.pool.QueryRow(ctx, `
WITH wakes AS (
    SELECT wake_id, outcome, execution_id
    FROM elitea_runtime.agent_heartbeats
    WHERE project_id = $1 AND application_id = $2 AND woke_at >= $3 AND outcome <> 'paused'
), keys AS (
    SELECT $4 || wake_id::text AS execution_id FROM wakes
    UNION ALL
    SELECT unnest(ARRAY[w.execution_id, $5 || w.execution_id, $6 || w.execution_id])
    FROM wakes AS w WHERE w.execution_id IS NOT NULL
    UNION ALL
    SELECT link.execution_id
    FROM wakes AS w
    JOIN elitea_runtime.run_chain_links AS link ON link.root_execution_id = w.execution_id
    WHERE link.kind <> 'root'
)
SELECT (SELECT count(*) FROM wakes),
       (SELECT count(*) FROM wakes WHERE outcome = 'act'),
       COALESCE((
           SELECT sum(`+requestLogCostSQL+`)::float8
           FROM gateway.llm_request_logs AS l
           LEFT JOIN gateway.gateway_models AS m ON m.provider = l.provider AND m.model_name = l.model
           WHERE l.project_id = $1 AND l.occurred_at >= $3 AND l.execution_id IS NOT NULL
             AND l.execution_id IN (SELECT execution_id FROM keys)
       ), 0)`,
		agent.ProjectID, agent.ApplicationID, since, heartbeat.AttributionPrefix,
		"run_brief:", "goal_check:",
	).Scan(&day.Wakes, &day.Acts, &day.SpendUSD)
	if err != nil {
		return heartbeat.Day{}, fmt.Errorf("agent heartbeat: read the day: %w", err)
	}
	return day, nil
}

// Material reads the digest's material: the agent's latest active memory
// entries (no cursor), the owner's notes (pinned facts written by a person)
// when the block watches schedule_note, and the new messages since the
// cursor in the conversations the agent takes part in.
func (s *AgentHeartbeats) Material(ctx context.Context, agent heartbeat.Agent, block proactivity.Block) (heartbeat.Material, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return heartbeat.Material{}, err
	}
	var material heartbeat.Material
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT kind, content, written_by_kind = 'person' AND pinned AND kind = 'fact'
FROM %s.agent_memory_entries
WHERE application_id = $1 AND status = 'active' AND kind <> 'cursor' AND subject_user_id IS NULL
ORDER BY pinned DESC, updated_at DESC, id DESC
LIMIT $2`, schema), agent.ApplicationID, heartbeat.MaxMemoryEntries+heartbeat.MaxNotes)
	if err != nil && !undefinedTable(err) {
		return heartbeat.Material{}, fmt.Errorf("agent heartbeat: read the memory: %w", err)
	}
	if err == nil {
		for rows.Next() {
			var kind, content string
			var note bool
			if err := rows.Scan(&kind, &content, &note); err != nil {
				rows.Close()
				return heartbeat.Material{}, err
			}
			switch {
			case note && block.Watches(proactivity.WatchScheduleNote) && len(material.Notes) < heartbeat.MaxNotes:
				material.Notes = append(material.Notes, content)
			case len(material.Memory) < heartbeat.MaxMemoryEntries:
				material.Memory = append(material.Memory, kind+": "+content)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return heartbeat.Material{}, err
		}
	}
	if !block.Watches(proactivity.WatchConversation) {
		return material, nil
	}
	// The conversations the agent takes part in, its home thread included.
	// Only visible messages ('all'); never an agent-to-agent side message.
	// created_at is a UTC wall clock without a zone (0123).
	since := time.Now().UTC().Add(-24 * time.Hour)
	if agent.CursorAt != nil && agent.CursorAt.After(since) {
		since = agent.CursorAt.UTC()
	}
	messageRows, err := s.pool.Query(ctx, fmt.Sprintf(`
WITH agent_conversations AS (
    -- The digest goes to the owner: only the agent's conversations that
    -- the owner takes part in, or that are not private. Another person's
    -- private chat with the agent never reaches it.
    SELECT DISTINCT mapping.conversation_id
    FROM %[1]s.chat_participant_mapping AS mapping
    JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
    JOIN %[1]s.chat_conversations AS conversation ON conversation.id = mapping.conversation_id
    WHERE participant.entity_name = 'application'
      AND participant.entity_meta ->> 'id' = $1::text
      AND (NOT conversation.is_private
           OR conversation.author_id = $4
           OR EXISTS (SELECT 1
                      FROM %[1]s.chat_participant_mapping AS owner_mapping
                      JOIN %[1]s.chat_participants AS owner ON owner.id = owner_mapping.participant_id
                      WHERE owner_mapping.conversation_id = mapping.conversation_id
                        AND owner.entity_name = 'user'
                        AND owner.entity_meta ->> 'id' = $4::text))
), recent AS (
    SELECT conversation.name, author.entity_name, author.entity_meta, author.meta::jsonb AS author_meta,
           message.created_at, message.id,
           (SELECT string_agg(text.content, E'\n' ORDER BY item.order_index)
            FROM %[1]s.chat_message_items AS item
            JOIN %[1]s.chat_messages_text AS text ON text.id = item.id
            WHERE item.message_group_id = message.id) AS content
    FROM %[1]s.chat_message_group AS message
    JOIN agent_conversations AS mine ON mine.conversation_id = message.conversation_id
    JOIN %[1]s.chat_conversations AS conversation ON conversation.id = message.conversation_id
    JOIN %[1]s.chat_participants AS author ON author.id = message.author_participant_id
    WHERE message.created_at > $2::timestamptz AT TIME ZONE 'UTC'
      AND message.visibility = 'all'
      AND COALESCE(message.cause, '') <> 'heartbeat'
      -- The oldest unread first: the cursor moves to the newest message
      -- read, so the ones past the limit are read by the next wake. A
      -- message still streaming stops the read there, so it is read
      -- whole once it ends.
      AND NOT EXISTS (SELECT 1
                      FROM %[1]s.chat_message_group AS open
                      JOIN agent_conversations AS open_mine ON open_mine.conversation_id = open.conversation_id
                      WHERE open.is_streaming
                        AND open.created_at > $2::timestamptz AT TIME ZONE 'UTC'
                        AND open.created_at <= message.created_at
                        AND open.visibility = 'all'
                        AND COALESCE(open.cause, '') <> 'heartbeat')
    ORDER BY message.created_at, message.id
    LIMIT $3
)
SELECT name, entity_name, COALESCE(author_meta ->> 'name', entity_meta ->> 'id', entity_name),
       created_at, COALESCE(content, '')
FROM recent ORDER BY created_at, id`, schema),
		strconv.FormatInt(agent.ApplicationID, 10), since, heartbeat.MaxMessages, agent.OwnerUserID)
	if err != nil {
		return heartbeat.Material{}, fmt.Errorf("agent heartbeat: read the messages: %w", err)
	}
	defer messageRows.Close()
	for messageRows.Next() {
		var (
			conversation, entity, author, content string
			at                                    time.Time
		)
		if err := messageRows.Scan(&conversation, &entity, &author, &at, &content); err != nil {
			return heartbeat.Material{}, err
		}
		at = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC)
		newest := at
		material.NewestAt = &newest
		if strings.TrimSpace(content) == "" {
			continue
		}
		if entity == "user" {
			author = "person " + author
		}
		material.Messages = append(material.Messages, proactivity.DigestMessage{
			Conversation: conversation, Author: author, At: at, Text: content,
		})
	}
	return material, messageRows.Err()
}

// RecordWake writes the heartbeat row and the agent's clock in one
// transaction.
func (s *AgentHeartbeats) RecordWake(ctx context.Context, wake heartbeat.Wake) error {
	schema, err := tenantschema.QuoteInt(wake.ProjectID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_heartbeats
    (wake_id, project_id, application_id, release_id, version_id, owner_user_id, outcome, reason_code,
     reason, digest_sha256, triage_spend_usd, execution_id, conversation_uuid, paused_until)
VALUES ($1::uuid, $2::bigint, $3::bigint, NULLIF($4::bigint, 0), NULLIF($5::bigint, 0), $6::bigint, $7::text,
        $8::text, $9::text, NULLIF($10::text, ''),
        COALESCE((SELECT sum(`+requestLogCostSQL+`)
                  FROM gateway.llm_request_logs AS l
                  LEFT JOIN gateway.gateway_models AS m ON m.provider = l.provider AND m.model_name = l.model
                  WHERE l.project_id = $2::bigint AND l.execution_id = $14::text), 0),
        NULLIF($11::text, ''), NULLIF($12::text, '')::uuid, $13::timestamptz)
ON CONFLICT (wake_id) DO NOTHING`,
			wake.WakeID, wake.ProjectID, wake.ApplicationID, wake.ReleaseID, wake.VersionID, wake.OwnerUserID,
			wake.Outcome, wake.ReasonCode, wake.Reason, wake.DigestSHA256, wake.ExecutionID,
			wake.ConversationUUID, wake.PausedUntil, heartbeat.AttributionPrefix+wake.WakeID,
		); err != nil {
			return fmt.Errorf("agent heartbeat: record the wake: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings
SET next_wake_at = $2, last_wake_at = clock_timestamp(), last_skip_reason = '',
    release_id = NULLIF($3, 0), paused_until = $4,
    cursor_at = COALESCE($5, cursor_at), updated_at = clock_timestamp()
WHERE application_id = $1`, schema),
			wake.ApplicationID, wake.Next, wake.ReleaseID, wake.PausedUntil, wake.CursorAt); err != nil {
			return fmt.Errorf("agent heartbeat: move the clock: %w", err)
		}
		return nil
	})
}

// PostToOwner writes one agent message to the owner in the home thread
// (created on first use) and one notification.
func (s *AgentHeartbeats) PostToOwner(ctx context.Context, post heartbeat.Post) (string, error) {
	schema, err := tenantschema.QuoteInt(post.ProjectID)
	if err != nil {
		return "", err
	}
	var conversationUUID string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		home, err := s.homeThread(ctx, tx, schema, post.ProjectID, post.ApplicationID, post.OwnerUserID,
			post.VersionID, post.AgentName)
		if err != nil {
			return err
		}
		conversationUUID = home.uuid
		meta, err := json.Marshal(map[string]any{
			"heartbeat": map[string]any{"wake_id": post.WakeID, "outcome": post.Outcome, "reason_code": post.ReasonCode},
		})
		if err != nil {
			return err
		}
		var groupID int64
		var messageUUID string
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, cause, created_at)
VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, FALSE, 'heartbeat', GREATEST(
    clock_timestamp() AT TIME ZONE 'UTC',
    (SELECT max(created_at) + interval '1 millisecond'
     FROM %[1]s.chat_message_group WHERE conversation_id = $2)))
RETURNING id, uuid::text`, schema), home.agentParticipant, home.id, home.ownerParticipant, string(meta),
		).Scan(&groupID, &messageUUID); err != nil {
			return fmt.Errorf("agent heartbeat: post the message: %w", err)
		}
		if err := insertLocalTurnText(ctx, tx, schema, groupID, post.Text); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %s.chat_conversations SET updated_at = now() AT TIME ZONE 'UTC' WHERE id = $1`, schema), home.id); err != nil {
			return err
		}
		notification, err := json.Marshal(map[string]any{
			"conversation_id": home.uuid, "message_id": messageUUID, "project_id": post.ProjectID,
			"application_id": post.ApplicationID, "agent_name": post.AgentName,
			"outcome": post.Outcome, "reason_code": post.ReasonCode, "text": clipNotificationText(post.Text),
			// The web's `[text]()` link syntax: the client resolves the empty
			// link to the conversation (`HREF_RESOLVERS.agent_heartbeat`).
			"message": heartbeatNotificationMessage(post.AgentName, post.Text),
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES (gen_random_uuid(), FALSE, $1, $2, $3::jsonb, $4, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')`,
			post.ProjectID, post.OwnerUserID, string(notification), HeartbeatNotificationEventType); err != nil {
			return fmt.Errorf("agent heartbeat: create the notification: %w", err)
		}
		return nil
	})
	return conversationUUID, err
}

// heartbeatNotificationMessage is meta.message: the agent's name, the start
// of its message and a link. Brackets in the agent's text would break the
// link syntax, so they become parentheses.
func heartbeatNotificationMessage(agentName, text string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "Your agent"
	}
	brackets := strings.NewReplacer("[", "(", "]", ")")
	clean := brackets.Replace(clipNotificationText(strings.Join(strings.Fields(text), " ")))
	return brackets.Replace(name) + ": " + clean + " [Open the conversation]()"
}

func clipNotificationText(text string) string {
	const limit = 280
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "…"
}

type homeThread struct {
	id               int64
	uuid             string
	agentParticipant int64
	ownerParticipant int64
}

// homeThread finds the agent's home thread, or creates it and stores its
// uuid on the settings row. The settings row is locked first, so two wakes
// cannot create two threads.
func (s *AgentHeartbeats) homeThread(
	ctx context.Context, tx pgx.Tx, schema string, projectID, applicationID, ownerID, versionID int64, agentName string,
) (homeThread, error) {
	var stored *string
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT home_conversation_uuid::text FROM %s.agent_heartbeat_settings WHERE application_id = $1 FOR UPDATE`, schema),
		applicationID).Scan(&stored); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return homeThread{}, ErrHeartbeatNotFound
		}
		return homeThread{}, fmt.Errorf("agent heartbeat: lock the settings row: %w", err)
	}
	var home homeThread
	if stored != nil {
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT id, uuid::text FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema),
			*stored).Scan(&home.id, &home.uuid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return homeThread{}, err
		}
	}
	if home.id == 0 {
		created, err := createHeartbeatConversation(ctx, tx, schema, projectID, applicationID, ownerID, agentName, true)
		if err != nil {
			return homeThread{}, err
		}
		home.id, home.uuid = created.id, created.uuid
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings SET home_conversation_uuid = $2::uuid, updated_at = clock_timestamp()
WHERE application_id = $1`, schema), applicationID, home.uuid); err != nil {
			return homeThread{}, err
		}
	}
	var err error
	if home.agentParticipant, home.ownerParticipant, err = mapHeartbeatParticipants(ctx, tx, schema, home.id,
		projectID, applicationID, ownerID, versionID); err != nil {
		return homeThread{}, err
	}
	return home, nil
}

type createdConversation struct {
	id   int64
	uuid string
}

func createHeartbeatConversation(
	ctx context.Context, tx pgx.Tx, schema string, projectID, applicationID, ownerID int64, agentName string, home bool,
) (createdConversation, error) {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "Agent " + strconv.FormatInt(applicationID, 10)
	}
	if home {
		name += " (heartbeat)"
	} else {
		name += " (heartbeat run)"
	}
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[:200], "")
	}
	meta, err := json.Marshal(map[string]any{
		"conversation_type":        HeartbeatConversationSource,
		"heartbeat_application_id": strconv.FormatInt(applicationID, 10),
		"heartbeat_home":           home,
		"project_id":               projectID,
	})
	if err != nil {
		return createdConversation{}, err
	}
	var created createdConversation
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source, created_at)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4, now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, schema), name, ownerID, string(meta), HeartbeatConversationSource,
	).Scan(&created.id, &created.uuid); err != nil {
		return createdConversation{}, fmt.Errorf("agent heartbeat: create the conversation: %w", err)
	}
	return created, nil
}

// mapHeartbeatParticipants maps the owner and the agent (pinned to the
// release's version) to the conversation, as the pipeline trigger
// admission maps them, and answers both participant ids.
func mapHeartbeatParticipants(
	ctx context.Context, tx pgx.Tx, schema string, conversationID, projectID, applicationID, ownerID, versionID int64,
) (int64, int64, error) {
	owner, err := heartbeatParticipant(ctx, tx, schema, "user", map[string]any{"id": ownerID})
	if err != nil {
		return 0, 0, err
	}
	agent, err := heartbeatParticipant(ctx, tx, schema, "application", map[string]any{"id": applicationID, "project_id": projectID})
	if err != nil {
		return 0, 0, err
	}
	settings, err := json.Marshal(map[string]any{"version_id": versionID})
	if err != nil {
		return 0, 0, err
	}
	for participant, entity := range map[int64]string{owner: `{}`, agent: string(settings)} {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (participant_id, conversation_id)
DO UPDATE SET entity_settings = EXCLUDED.entity_settings, updated_at = now() AT TIME ZONE 'UTC'`, schema),
			conversationID, participant, entity); err != nil {
			return 0, 0, fmt.Errorf("agent heartbeat: map a participant: %w", err)
		}
	}
	return agent, owner, nil
}

func heartbeatParticipant(ctx context.Context, tx pgx.Tx, schema, entityName string, entityMeta map[string]any) (int64, error) {
	raw, err := json.Marshal(entityMeta)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = $1::text
  AND entity_meta ->> 'id' IS NOT DISTINCT FROM $2::jsonb ->> 'id'
  AND entity_meta ->> 'project_id' IS NOT DISTINCT FROM $2::jsonb ->> 'project_id'
ORDER BY id LIMIT 1`, schema), entityName, string(raw)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("agent heartbeat: find a participant: %w", err)
	}
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json)
RETURNING id`, schema), entityName, string(raw)).Scan(&id); err != nil {
		return 0, fmt.Errorf("agent heartbeat: create a participant: %w", err)
	}
	return id, nil
}

// PrepareAct finds the home thread (or creates a new conversation for a
// new_conversation target) and writes the run's notify_and_wait policy.
func (s *AgentHeartbeats) PrepareAct(ctx context.Context, agent heartbeat.Agent, release heartbeat.Release, target string) (heartbeat.ActTarget, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return heartbeat.ActTarget{}, err
	}
	var act heartbeat.ActTarget
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var conversationID int64
		if target == proactivity.ActTargetNewConversation {
			created, err := createHeartbeatConversation(ctx, tx, schema, agent.ProjectID, agent.ApplicationID,
				agent.OwnerUserID, release.AgentName, false)
			if err != nil {
				return err
			}
			conversationID, act.ConversationUUID, act.Created = created.id, created.uuid, true
			participant, _, err := mapHeartbeatParticipants(ctx, tx, schema, conversationID, agent.ProjectID,
				agent.ApplicationID, agent.OwnerUserID, release.VersionID)
			if err != nil {
				return err
			}
			act.ParticipantID = participant
		} else {
			home, err := s.homeThread(ctx, tx, schema, agent.ProjectID, agent.ApplicationID, agent.OwnerUserID,
				release.VersionID, release.AgentName)
			if err != nil {
				return err
			}
			act.ConversationUUID, act.ParticipantID = home.uuid, home.agentParticipant
		}
		// ADR-0050 decision 2a: on_approval_required is notify_and_wait, a
		// snapshot written before the run can pause (shared 0211). The row
		// is per conversation; the home thread keeps one row for every act.
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES ($1, $2::uuid, 'schedule', $3, $4, 'notify_and_wait')
ON CONFLICT (project_id, conversation_uuid)
DO UPDATE SET version_id = EXCLUDED.version_id, owner_user_id = EXCLUDED.owner_user_id,
    on_approval_required = 'notify_and_wait'`,
			agent.ProjectID, act.ConversationUUID, release.VersionID, agent.OwnerUserID); err != nil {
			return fmt.Errorf("agent heartbeat: write the approval policy: %w", err)
		}
		return nil
	})
	return act, err
}

// DiscardAct removes a new conversation whose start was refused.
func (s *AgentHeartbeats) DiscardAct(ctx context.Context, agent heartbeat.Agent, target heartbeat.ActTarget) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil || !target.Created {
		return
	}
	_, _ = DiscardUnadmittedConversation(context.WithoutCancel(ctx), s.pool, schema, target.ConversationUUID,
		HeartbeatConversationSource)
	_, _ = s.pool.Exec(context.WithoutCancel(ctx), `
DELETE FROM elitea_runtime.unattended_run_policies WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		agent.ProjectID, target.ConversationUUID)
}

// ---------------------------------------------------------------------------
// The settings and history routes

// HeartbeatSettings is one agent's settings row, as the route answers it.
type HeartbeatSettings struct {
	ApplicationID        int64      `json:"application_id"`
	Enabled              bool       `json:"enabled"`
	OwnerUserID          int64      `json:"owner_user_id"`
	MandateID            *string    `json:"mandate_id"`
	HomeConversationUUID *string    `json:"home_conversation_uuid"`
	ReleaseID            *int64     `json:"release_id"`
	NextWakeAt           time.Time  `json:"next_wake_at"`
	LastWakeAt           *time.Time `json:"last_wake_at"`
	PausedUntil          *time.Time `json:"paused_until"`
	LastSkipReason       string     `json:"last_skip_reason"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// GetHeartbeatSettings reads one agent's row.
func (s *AgentHeartbeats) GetHeartbeatSettings(ctx context.Context, projectID, applicationID int64) (HeartbeatSettings, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return HeartbeatSettings{}, err
	}
	var row HeartbeatSettings
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT application_id, enabled, owner_user_id, mandate_id::text, home_conversation_uuid::text, release_id,
       next_wake_at, last_wake_at, paused_until, last_skip_reason, updated_at
FROM %s.agent_heartbeat_settings WHERE application_id = $1`, schema), applicationID).Scan(
		&row.ApplicationID, &row.Enabled, &row.OwnerUserID, &row.MandateID, &row.HomeConversationUUID,
		&row.ReleaseID, &row.NextWakeAt, &row.LastWakeAt, &row.PausedUntil, &row.LastSkipReason, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HeartbeatSettings{}, ErrHeartbeatNotFound
	}
	if err != nil {
		return HeartbeatSettings{}, fmt.Errorf("agent heartbeat: read the settings: %w", err)
	}
	return row, nil
}

// HeartbeatSettingsUpdate is a save of the settings row. A nil field keeps
// its value; ClearMandate removes the mandate.
type HeartbeatSettingsUpdate struct {
	Enabled      bool
	OwnerUserID  int64
	MandateID    *string
	ClearMandate bool
	ActorID      int64
}

// SaveHeartbeatSettings creates or updates one agent's row. Turning the
// heartbeat on makes it due now; the person who saves it becomes its
// owner.
func (s *AgentHeartbeats) SaveHeartbeatSettings(ctx context.Context, projectID, applicationID int64, update HeartbeatSettingsUpdate) (HeartbeatSettings, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return HeartbeatSettings{}, err
	}
	_, err = s.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.agent_heartbeat_settings
    (application_id, enabled, owner_user_id, mandate_id, updated_by, next_wake_at)
VALUES ($1, $2, $3, $4::uuid, NULLIF($6, 0), clock_timestamp())
ON CONFLICT (application_id) DO UPDATE SET
    enabled = EXCLUDED.enabled,
    owner_user_id = EXCLUDED.owner_user_id,
    mandate_id = CASE WHEN $5 THEN NULL
                      ELSE COALESCE(EXCLUDED.mandate_id, agent_heartbeat_settings.mandate_id) END,
    next_wake_at = CASE WHEN EXCLUDED.enabled AND NOT agent_heartbeat_settings.enabled
                        THEN clock_timestamp() ELSE agent_heartbeat_settings.next_wake_at END,
    paused_until = CASE WHEN EXCLUDED.enabled AND NOT agent_heartbeat_settings.enabled
                        THEN NULL ELSE agent_heartbeat_settings.paused_until END,
    updated_by = EXCLUDED.updated_by,
    updated_at = clock_timestamp()`, schema),
		applicationID, update.Enabled, update.OwnerUserID, update.MandateID, update.ClearMandate, update.ActorID)
	if err != nil {
		return HeartbeatSettings{}, fmt.Errorf("agent heartbeat: save the settings: %w", err)
	}
	return s.GetHeartbeatSettings(ctx, projectID, applicationID)
}

// HeartbeatEntry is one heartbeat row, as the history route answers it.
type HeartbeatEntry struct {
	ID               int64      `json:"id"`
	WakeID           string     `json:"wake_id"`
	WokeAt           time.Time  `json:"woke_at"`
	Outcome          string     `json:"outcome"`
	ReasonCode       string     `json:"reason_code"`
	Reason           string     `json:"reason"`
	ReleaseID        *int64     `json:"release_id"`
	DigestSHA256     *string    `json:"digest_sha256"`
	TriageSpendUSD   float64    `json:"triage_spend_usd"`
	ExecutionID      *string    `json:"execution_id"`
	ConversationUUID *string    `json:"conversation_uuid"`
	PausedUntil      *time.Time `json:"paused_until"`
}

// ListHeartbeats reads one agent's heartbeat history, newest first.
func (s *AgentHeartbeats) ListHeartbeats(ctx context.Context, projectID, applicationID int64, limit, offset int) ([]HeartbeatEntry, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.agent_heartbeats WHERE project_id = $1 AND application_id = $2`,
		projectID, applicationID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("agent heartbeat: count the history: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, wake_id::text, woke_at, outcome, reason_code, reason, release_id, digest_sha256,
       triage_spend_usd::float8, execution_id, conversation_uuid::text, paused_until
FROM elitea_runtime.agent_heartbeats
WHERE project_id = $1 AND application_id = $2
ORDER BY woke_at DESC, id DESC
LIMIT $3 OFFSET $4`, projectID, applicationID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("agent heartbeat: read the history: %w", err)
	}
	defer rows.Close()
	entries := []HeartbeatEntry{}
	for rows.Next() {
		var entry HeartbeatEntry
		if err := rows.Scan(&entry.ID, &entry.WakeID, &entry.WokeAt, &entry.Outcome, &entry.ReasonCode,
			&entry.Reason, &entry.ReleaseID, &entry.DigestSHA256, &entry.TriageSpendUSD, &entry.ExecutionID,
			&entry.ConversationUUID, &entry.PausedUntil); err != nil {
			return nil, 0, err
		}
		entries = append(entries, entry)
	}
	return entries, total, rows.Err()
}

// HeartbeatGovernance is what the settings route needs to decide whether a
// caller may turn the heartbeat on.
type HeartbeatGovernance struct {
	Tier     agentlifecycle.Tier
	Personal bool
	Owner    bool
}

// HeartbeatGovernanceOf reads the agent's tier, whether the project is the
// caller's personal project, and whether the caller owns the agent.
func (s *AgentHeartbeats) HeartbeatGovernanceOf(ctx context.Context, projectID, applicationID, userID int64) (HeartbeatGovernance, error) {
	var (
		governance HeartbeatGovernance
		tier       *string
	)
	err := s.pool.QueryRow(ctx, `
SELECT (SELECT risk_tier FROM elitea_lifecycle.agent_registry
        WHERE tenant_project_id = $1 AND application_id = $2),
       EXISTS (SELECT 1 FROM centry.project WHERE id = $1 AND name = $4),
       EXISTS (SELECT 1 FROM elitea_lifecycle.agent_registry
               WHERE tenant_project_id = $1 AND application_id = $2
                 AND $3 IN (business_owner_id, technical_owner_id))`,
		projectID, applicationID, userID, "project_user_"+strconv.FormatInt(userID, 10),
	).Scan(&tier, &governance.Personal, &governance.Owner)
	if err != nil {
		return HeartbeatGovernance{}, fmt.Errorf("agent heartbeat: read the governance: %w", err)
	}
	governance.Tier = agentlifecycle.TierLow
	if tier != nil && agentlifecycle.Tier(*tier).Valid() {
		governance.Tier = agentlifecycle.Tier(*tier)
	}
	return governance, nil
}

// ProactiveApplicationIDs answers which of ids have an enabled heartbeat:
// the registry's proactive badge.
func (s *AgentHeartbeats) ProactiveApplicationIDs(ctx context.Context, projectID int64, ids []int64) (map[int64]bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`SELECT application_id FROM %s.agent_heartbeat_settings WHERE enabled AND application_id = ANY($1::bigint[])`,
		schema), ids)
	if undefinedTable(err) {
		return map[int64]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
