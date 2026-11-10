package repos

// The ladder stages of the agent workforce (ADR-0050 decision 5g, issue
// 1616), each applied in the approving decision's transaction, and their
// reversal: the appeal and the reinstatement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicproject"
)

// applyStage applies one stage and answers what it did and the suspensions
// it wrote (for the kill switch gate's invalidation after the commit).
func (s *AgentWorkforce) applyStage(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, stage agentworkforce.Stage, proposal WorkforceProposal, decision Decision) (map[string]any, []int64, error) {
	switch stage {
	case agentworkforce.StageWatch:
		return map[string]any{"under_review": true}, nil, setWatch(ctx, tx, agent)
	case agentworkforce.StageDemote:
		applied, err := s.demote(ctx, tx, agent, proposal.Narrowing)
		return applied, nil, err
	case agentworkforce.StageSuspend:
		id, err := suspendForDismissal(ctx, tx, agent, decision.Actor.UserID, proposal.ID)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"suspension_id": id}, []int64{id}, nil
	case agentworkforce.StageRetire:
		impact, err := impactOf(ctx, tx, agent.ProjectID, agent.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		if len(impact.NestedBy) > 0 && !decision.ConfirmNested {
			return nil, nil, &agentworkforce.Refusal{Code: agentworkforce.ConfirmNestedCode,
				Message: "Other agents use this agent as a sub-agent; their runs will fail with nested_agent_retired. Confirm to retire it anyway.",
				Detail:  map[string]any{"nested_by": impact.NestedBy}}
		}
		applied, err := s.retire(ctx, tx, agent, decision.Actor.UserID)
		return applied, nil, err
	case agentworkforce.StageDelete:
		applied, err := s.purge(ctx, tx, agent)
		return applied, nil, err
	}
	return nil, nil, agentworkforce.Refuse(agentworkforce.InvalidCode, "Unknown stage.")
}

/* ── watch ─────────────────────────────────────────────────────────────── */

func setWatch(ctx context.Context, tx pgx.Tx, agent WorkforceAgent) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET under_review = true, under_review_since = COALESCE(under_review_since, clock_timestamp()), updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID); err != nil {
		return fmt.Errorf("agent workforce: set the watch flag: %w", err)
	}
	return nil
}

func clearWatch(ctx context.Context, tx pgx.Tx, agent WorkforceAgent) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET under_review = false, under_review_since = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID); err != nil {
		return fmt.Errorf("agent workforce: clear the watch flag: %w", err)
	}
	return nil
}

/* ── demote ────────────────────────────────────────────────────────────── */

// demote writes the narrowing onto the agent's working draft and drops it
// as a lead. The published release is untouched: the narrowed draft is the
// next release, published through the agent's own release path (a change
// request when its tier asks approval, or directly). applied records the
// draft version the narrowing went onto, so the owner can see and publish
// it. A lead role is dropped at once.
func (s *AgentWorkforce) demote(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, narrowing agentworkforce.Narrowing) (map[string]any, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return nil, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	applied := map[string]any{}
	if narrowing.TouchesVersion() {
		var (
			draftID int64
			rawMeta []byte
		)
		err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id, COALESCE(meta, '{}'::jsonb)::text::bytea FROM %s.application_versions
WHERE application_id = $1 AND status = 'draft'
ORDER BY id DESC LIMIT 1 FOR UPDATE`, schema), agent.ApplicationID).Scan(&draftID, &rawMeta)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent has no draft version to narrow.")
		}
		if err != nil {
			return nil, fmt.Errorf("agent workforce: read the draft: %w", err)
		}
		meta := map[string]any{}
		if err := json.Unmarshal(rawMeta, &meta); err != nil {
			return nil, fmt.Errorf("agent workforce: decode the draft meta: %w", err)
		}
		encoded, err := json.Marshal(agentworkforce.NarrowMeta(meta, narrowing))
		if err != nil {
			return nil, fmt.Errorf("agent workforce: encode the draft meta: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.application_versions SET meta = $2::jsonb WHERE id = $1`, schema),
			draftID, string(encoded)); err != nil {
			return nil, fmt.Errorf("agent workforce: write the draft meta: %w", err)
		}
		detached, err := detachToolkits(ctx, tx, schema, draftID, narrowing)
		if err != nil {
			return nil, err
		}
		applied["draft_version_id"] = draftID
		applied["detached_toolkit_ids"] = detached
	}
	if narrowing.ProactivityOff {
		stopped, err := stopHeartbeat(ctx, tx, schema, agent.ApplicationID)
		if err != nil {
			return nil, err
		}
		applied["heartbeat_stopped"] = stopped
	}
	if narrowing.RemoveLead {
		leads, err := dropLeads(ctx, tx, schema, agent.ApplicationID, agent.ProjectID)
		if err != nil {
			return nil, err
		}
		applied["leads_removed"] = leads
	}
	applied["narrowing"] = narrowing
	applied["publish_needed"] = narrowing.TouchesVersion()
	return applied, nil
}

// stopHeartbeat disables the agent's heartbeat (tenant 0166, #1618) at
// once. The heartbeat reads the block of the RELEASE, which keeps it until
// the narrowed draft is published; a demotion or a retirement takes the
// heartbeat away now. stopped is false without an enabled row, or on a
// tenant schema older than 0166.
func stopHeartbeat(ctx context.Context, tx pgx.Tx, schema string, applicationID int64) (bool, error) {
	exists, err := relationExists(ctx, tx, schema+".agent_heartbeat_settings")
	if err != nil || !exists {
		return false, err
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings SET enabled = false, updated_at = clock_timestamp()
WHERE application_id = $1 AND enabled`, schema), applicationID)
	if err != nil {
		return false, fmt.Errorf("agent workforce: stop the heartbeat: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// detachToolkits removes the named toolkits, and those of a type outside
// the ceiling, from the draft. Sub-agent entries are not toolkits and stay.
func detachToolkits(ctx context.Context, tx pgx.Tx, schema string, draftID int64, narrowing agentworkforce.Narrowing) ([]int64, error) {
	if len(narrowing.RemoveToolkitIDs) == 0 && len(narrowing.ToolkitTypeCeiling) == 0 {
		return []int64{}, nil
	}
	ceiling := narrowing.ToolkitTypeCeiling
	if ceiling == nil {
		ceiling = []string{}
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
DELETE FROM %[1]s.entity_tool_mapping AS mapping
USING %[1]s.elitea_tools AS tool
WHERE tool.id = mapping.tool_id AND mapping.entity_version_id = $1 AND mapping.entity_type = 'agent'
  AND tool.type <> 'application'
  AND (mapping.tool_id = ANY($2::bigint[])
       OR (cardinality($3::text[]) > 0 AND NOT (lower(tool.type) = ANY($3::text[]))))
RETURNING mapping.tool_id`, schema), draftID, narrowing.RemoveToolkitIDs, ceiling)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: detach the toolkits: %w", err)
	}
	defer rows.Close()
	detached := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("agent workforce: detach the toolkits: %w", err)
		}
		detached = append(detached, id)
	}
	slices.Sort(detached)
	return slices.Compact(detached), rows.Err()
}

// agentParticipantsSQL is the participants of the project's conversations
// that are this agent (entity_meta id, and project when it names one).
const agentParticipantsSQL = `
SELECT participant.id FROM %[1]s.chat_participants AS participant
WHERE participant.entity_name = 'application'
  AND participant.entity_meta ->> 'id' = $1::text
  AND (participant.entity_meta ->> 'project_id' IS NULL OR participant.entity_meta ->> 'project_id' = $2::text)`

// dropLeads clears the agent as the lead of every conversation it leads:
// the floor goes back to addressed.
func dropLeads(ctx context.Context, tx pgx.Tx, schema string, applicationID, projectID int64) (int64, error) {
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %[1]s.chat_conversations AS conversation
SET meta = conversation.meta - 'floor'
WHERE conversation.meta -> 'floor' ->> 'lead_participant_id' IN (
    SELECT participant.id::text FROM (`+agentParticipantsSQL+`) AS participant)`, schema),
		strconv.FormatInt(applicationID, 10), strconv.FormatInt(projectID, 10))
	if err != nil {
		return 0, fmt.Errorf("agent workforce: drop the lead roles: %w", err)
	}
	return tag.RowsAffected(), nil
}

/* ── suspend ───────────────────────────────────────────────────────────── */

func suspendForDismissal(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, actorID, proposalID int64) (int64, error) {
	if agent.SuspensionID > 0 {
		return 0, agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent is already suspended.")
	}
	created, err := (&ExecutionSuspensionRepository{}).CreateSuspensionTx(ctx, tx, safety.NewSuspension{
		Target:  safety.Target{Scope: safety.ScopeAgent, ProjectID: agent.ProjectID, ApplicationID: agent.ApplicationID},
		Reason:  fmt.Sprintf("Dismissal ladder: suspension approved on workforce proposal %d.", proposalID),
		ActorID: actorID, Source: safety.SourceDismissal, ResumeRequires: safety.ResumeAnyAdmin,
		Detail: map[string]any{"workforce_proposal_id": proposalID},
	})
	var already *safety.AlreadySuspendedError
	if errors.As(err, &already) {
		return 0, agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent is already suspended.")
	}
	if err != nil {
		return 0, fmt.Errorf("agent workforce: suspend the agent: %w", err)
	}
	return created.ID, nil
}

// resumeDismissalSuspension resumes the agent's active dismissal
// suspension. Another source's suspension stays (the breaker's, a
// certification lapse, an administrator's).
func resumeDismissalSuspension(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, actorID int64, reason string) ([]int64, error) {
	rows, err := tx.Query(ctx, `
UPDATE elitea_runtime.execution_suspensions
SET resumed_at = greatest(clock_timestamp(), created_at), resumed_by = $3, resume_reason = $4
WHERE scope = 'agent' AND project_id = $1 AND scope_key = $2 AND resumed_at IS NULL AND source = 'dismissal'
RETURNING id`, agent.ProjectID, strconv.FormatInt(agent.ApplicationID, 10), actorID, reason)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: resume the dismissal suspension: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

/* ── retire ────────────────────────────────────────────────────────────── */

// retireSnapshot is what retirement changed, so a reinstatement restores it.
type retireSnapshot struct {
	PublishedVersions []snapshotVersion `json:"published_versions"`
	WithdrawnReleases []int64           `json:"withdrawn_releases"`
	HeadReleaseID     int64             `json:"head_release_id"`
	Rosters           []snapshotRoster  `json:"rosters"`
	Leads             []snapshotLead    `json:"leads"`
	Schedules         []int64           `json:"schedules"`
	Triggers          []int64           `json:"triggers"`
	Teams             []snapshotTeam    `json:"teams"`
	// Heartbeat: retirement disabled the agent's heartbeat row (#1618).
	Heartbeat bool `json:"heartbeat,omitempty"`
}

type snapshotVersion struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type snapshotRoster struct {
	ConversationID int64           `json:"conversation_id"`
	ParticipantID  int64           `json:"participant_id"`
	EntitySettings json.RawMessage `json:"entity_settings"`
	Persona        json.RawMessage `json:"persona,omitempty"`
}

type snapshotLead struct {
	ConversationID int64           `json:"conversation_id"`
	Floor          json.RawMessage `json:"floor"`
}

type snapshotTeam struct {
	TeamID   int64  `json:"team_id"`
	Position int    `json:"position"`
	Version  int64  `json:"version_id"`
	Name     string `json:"name"`
	// LeadPosition: the team's lead was this member (the team falls back to
	// addressed while it is retired).
	Lead bool `json:"lead"`
}

// retire unpublishes the agent, takes it out of rosters, leads, saved teams
// and schedules, records all of it in the retire snapshot, and moves the
// registry to retired.
func (s *AgentWorkforce) retire(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, actorID int64) (map[string]any, error) {
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return nil, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	app := strconv.FormatInt(agent.ApplicationID, 10)
	project := strconv.FormatInt(agent.ProjectID, 10)
	var snap retireSnapshot

	// Published versions: back to draft, their catalogue twins gone, their
	// releases withdrawn (the same three steps as the unpublish route).
	if err := collect(ctx, tx, fmt.Sprintf(`
UPDATE %s.application_versions AS v SET status = 'draft'
FROM (SELECT id, status FROM %s.application_versions WHERE application_id = $1 AND status IN ('published', 'embedded') FOR UPDATE) AS old
WHERE v.id = old.id
RETURNING old.id, old.status`, schema, schema), []any{agent.ApplicationID}, func(rows pgx.Rows) error {
		var version snapshotVersion
		if err := rows.Scan(&version.ID, &version.Status); err != nil {
			return err
		}
		snap.PublishedVersions = append(snap.PublishedVersions, version)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("agent workforce: unpublish: %w", err)
	}
	versionIDs := make([]int64, 0, len(snap.PublishedVersions))
	for _, version := range snap.PublishedVersions {
		versionIDs = append(versionIDs, version.ID)
	}
	if err := tx.QueryRow(ctx, `
SELECT COALESCE((SELECT release_id FROM elitea_lifecycle.application_release_heads
                 WHERE tenant_project_id = $1 AND application_id = $2), 0)`,
		agent.ProjectID, agent.ApplicationID).Scan(&snap.HeadReleaseID); err != nil {
		return nil, fmt.Errorf("agent workforce: read the release head: %w", err)
	}
	if err := collect(ctx, tx, `
SELECT id FROM elitea_lifecycle.application_releases
WHERE tenant_project_id = $1 AND application_version_id = ANY($2::bigint[]) AND state = 'published'`,
		[]any{agent.ProjectID, versionIDs}, func(rows pgx.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			snap.WithdrawnReleases = append(snap.WithdrawnReleases, id)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("agent workforce: read the releases: %w", err)
	}
	if len(versionIDs) > 0 {
		if err := removePublicTwins(ctx, tx, agent.ProjectID, versionIDs); err != nil {
			return nil, err
		}
		if err := withdrawVersions(ctx, pgxExecutor{queryer: tx}, agent.ProjectID, versionIDs, actorID); err != nil {
			return nil, err
		}
	}

	// Leads first (they name a participant the roster removal deletes).
	if err := collect(ctx, tx, fmt.Sprintf(`
WITH led AS (
    SELECT conversation.id, conversation.meta -> 'floor' AS floor
    FROM %[1]s.chat_conversations AS conversation
    WHERE conversation.meta -> 'floor' ->> 'lead_participant_id' IN (
        SELECT participant.id::text FROM (`+agentParticipantsSQL+`) AS participant)
    FOR UPDATE
)
UPDATE %[1]s.chat_conversations AS conversation
SET meta = conversation.meta - 'floor'
FROM led WHERE led.id = conversation.id
RETURNING conversation.id, led.floor::text`, schema), []any{app, project}, func(rows pgx.Rows) error {
		var lead snapshotLead
		var floor string
		if err := rows.Scan(&lead.ConversationID, &floor); err != nil {
			return err
		}
		lead.Floor = json.RawMessage(floor)
		snap.Leads = append(snap.Leads, lead)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("agent workforce: drop the lead roles: %w", err)
	}
	if err := collect(ctx, tx, fmt.Sprintf(`
DELETE FROM %[1]s.chat_participant_mapping AS mapping
WHERE mapping.participant_id IN (SELECT participant.id FROM (`+agentParticipantsSQL+`) AS participant)
RETURNING mapping.conversation_id, mapping.participant_id, mapping.entity_settings::text, COALESCE(mapping.persona::text, '')`, schema),
		[]any{app, project}, func(rows pgx.Rows) error {
			var roster snapshotRoster
			var settings, persona string
			if err := rows.Scan(&roster.ConversationID, &roster.ParticipantID, &settings, &persona); err != nil {
				return err
			}
			roster.EntitySettings = json.RawMessage(settings)
			if persona != "" {
				roster.Persona = json.RawMessage(persona)
			}
			snap.Rosters = append(snap.Rosters, roster)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("agent workforce: take the agent out of the rosters: %w", err)
	}
	teams, err := removeFromTeams(ctx, tx, agent)
	if err != nil {
		return nil, err
	}
	snap.Teams = teams

	pipelines, err := relationExists(ctx, tx, schema+".pipeline_schedules")
	if err != nil {
		return nil, err
	}
	if pipelines {
		if err := collect(ctx, tx, fmt.Sprintf(`
	UPDATE %s.pipeline_schedules SET active = false, updated_at = now()
	WHERE application_id = $1 AND active RETURNING id`, schema), []any{agent.ApplicationID}, func(rows pgx.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			snap.Schedules = append(snap.Schedules, id)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("agent workforce: turn the schedules off: %w", err)
		}
		if err := collect(ctx, tx, fmt.Sprintf(`
	UPDATE %s.pipeline_triggers SET revoked_at = now(), revoked_by = $2
	WHERE application_id = $1 AND revoked_at IS NULL RETURNING id`, schema), []any{agent.ApplicationID, nullablePositive(actorID)},
			func(rows pgx.Rows) error {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				snap.Triggers = append(snap.Triggers, id)
				return nil
			}); err != nil {
			return nil, fmt.Errorf("agent workforce: revoke the triggers: %w", err)
		}
	}

	if snap.Heartbeat, err = stopHeartbeat(ctx, tx, schema, agent.ApplicationID); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: encode the retire snapshot: %w", err)
	}
	now := s.now().UTC()
	reinstateUntil := now.Add(agentworkforce.ReinstateWindow)
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET lifecycle_state = 'retired', retired_at = $3, retired_by = $4, reinstate_until = $5, purge_after = $5,
    retire_snapshot = $6::jsonb, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`,
		agent.ProjectID, agent.ApplicationID, now, nullablePositive(actorID), reinstateUntil, string(encoded)); err != nil {
		return nil, fmt.Errorf("agent workforce: retire the registry row: %w", err)
	}
	return map[string]any{
		"unpublished_versions": len(snap.PublishedVersions), "withdrawn_releases": len(snap.WithdrawnReleases),
		"rosters_left": len(snap.Rosters), "leads_removed": len(snap.Leads), "teams_left": len(snap.Teams),
		"schedules_off": len(snap.Schedules), "triggers_revoked": len(snap.Triggers),
		"heartbeat_stopped": snap.Heartbeat, "reinstate_until": reinstateUntil,
	}, nil
}

func collect(ctx context.Context, tx pgx.Tx, statement string, args []any, scan func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// removePublicTwins deletes the catalogue twins of the versions in the
// public project's schema (eliteacore's removeCatalogTwins).
func removePublicTwins(ctx context.Context, tx pgx.Tx, projectID int64, versionIDs []int64) error {
	public := int64(publicproject.ID())
	if projectID == public {
		return nil
	}
	schema, err := tenantschema.QuoteInt(public)
	if err != nil {
		return nil
	}
	var hasTwins bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = $1 AND table_name = 'application_versions' AND column_name = 'shared_owner_id')`,
		"p_"+strconv.FormatInt(public, 10)).Scan(&hasTwins); err != nil {
		return fmt.Errorf("agent workforce: probe the catalogue: %w", err)
	}
	if !hasTwins {
		return nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
DELETE FROM %s.application_versions WHERE shared_owner_id = $1 AND shared_id = ANY($2::bigint[])`, schema),
		projectID, versionIDs); err != nil {
		return fmt.Errorf("agent workforce: remove the catalogue twins: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
DELETE FROM %[1]s.applications AS a WHERE a.shared_owner_id = $1
  AND NOT EXISTS (SELECT 1 FROM %[1]s.application_versions AS v WHERE v.application_id = a.id)`, schema), projectID); err != nil {
		return fmt.Errorf("agent workforce: remove the catalogue twins: %w", err)
	}
	return nil
}

// removeFromTeams takes the agent out of the project's saved teams (shared
// 0233, issue 1620): a team whose lead it was falls back to addressed.
// A database without saved teams has none to leave.
func removeFromTeams(ctx context.Context, tx pgx.Tx, agent WorkforceAgent) ([]snapshotTeam, error) {
	var present *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('elitea_chat.agent_team_members')::text`).Scan(&present); err != nil {
		return nil, fmt.Errorf("agent workforce: probe the saved teams: %w", err)
	}
	teams := []snapshotTeam{}
	if present == nil {
		return teams, nil
	}
	if err := collect(ctx, tx, `
SELECT member.team_id, member.position, member.version_id, member.name, COALESCE(team.lead_position = member.position, false)
FROM elitea_chat.agent_team_members AS member
JOIN elitea_chat.agent_teams AS team ON team.id = member.team_id
WHERE member.agent_project_id = $1 AND member.application_id = $2
FOR UPDATE OF team, member`, []any{agent.ProjectID, agent.ApplicationID}, func(rows pgx.Rows) error {
		var team snapshotTeam
		if err := rows.Scan(&team.TeamID, &team.Position, &team.Version, &team.Name, &team.Lead); err != nil {
			return err
		}
		teams = append(teams, team)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("agent workforce: read the saved teams: %w", err)
	}
	for _, team := range teams {
		if team.Lead {
			if _, err := tx.Exec(ctx, `
UPDATE elitea_chat.agent_teams SET floor_policy = 'addressed', lead_position = NULL, updated_at = clock_timestamp()
WHERE id = $1`, team.TeamID); err != nil {
				return nil, fmt.Errorf("agent workforce: drop the team lead: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM elitea_chat.agent_team_members WHERE team_id = $1 AND position = $2`,
			team.TeamID, team.Position); err != nil {
			return nil, fmt.Errorf("agent workforce: leave the saved team: %w", err)
		}
	}
	return teams, nil
}

/* ── reinstate ─────────────────────────────────────────────────────────── */

// Reinstate brings a retired agent back within the reinstate window: what
// retirement changed is restored from the snapshot, a dismissal suspension
// is resumed and the watch flag is cleared.
func (s *AgentWorkforce) Reinstate(ctx context.Context, projectID, applicationID int64, actor WorkforceActor, reason string) (WorkforceAgent, error) {
	if !actor.Manager {
		return WorkforceAgent{}, agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager reinstates an agent.")
	}
	if _, err := agentworkforce.CheckNote(reason, agentworkforce.MaxNoteBytes); err != nil {
		return WorkforceAgent{}, err
	}
	var resumed []int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		agent, err := readWorkforceAgent(ctx, tx, projectID, applicationID, true)
		if err != nil {
			return err
		}
		if agent.State != agentworkforce.StateRetired {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent is not retired.")
		}
		resumed, err = s.reinstate(ctx, tx, agent, actor.UserID, "manager")
		return err
	})
	if err != nil {
		return WorkforceAgent{}, err
	}
	s.notify(resumed...)
	return s.Agent(ctx, projectID, applicationID)
}

func (s *AgentWorkforce) reinstate(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, actorID int64, via string) ([]int64, error) {
	if agent.ReinstateUntil == nil || s.now().After(*agent.ReinstateUntil) {
		return nil, agentworkforce.Refuse(agentworkforce.ReinstateClosedCode, "The 30-day reinstate window has closed.")
	}
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return nil, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(retire_snapshot, '{}'::jsonb)::text::bytea FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("agent workforce: read the retire snapshot: %w", err)
	}
	var snap retireSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("agent workforce: decode the retire snapshot: %w", err)
	}

	for _, version := range snap.PublishedVersions {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.application_versions SET status = $2 WHERE id = $1 AND application_id = $3 AND status = 'draft'`, schema),
			version.ID, version.Status, agent.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent workforce: republish: %w", err)
		}
	}
	// A release is immutable but its state: the withdrawn ones go back to
	// published, and the head points where it pointed.
	if len(snap.WithdrawnReleases) > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.application_releases SET state = 'published', withdrawn_at = NULL, withdrawn_by = NULL
WHERE tenant_project_id = $1 AND id = ANY($2::bigint[]) AND state = 'withdrawn'`,
			agent.ProjectID, snap.WithdrawnReleases); err != nil {
			return nil, fmt.Errorf("agent workforce: restore the releases: %w", err)
		}
	}
	if snap.HeadReleaseID > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_lifecycle.application_release_heads (tenant_project_id, application_id, release_id, pointed_by, cause, reason)
VALUES ($1, $2, $3, $4, 'rollback', 'Reinstated by the agent workforce.')
ON CONFLICT (tenant_project_id, application_id)
DO UPDATE SET release_id = EXCLUDED.release_id, pointed_by = EXCLUDED.pointed_by, pointed_at = clock_timestamp(),
              cause = EXCLUDED.cause, reason = EXCLUDED.reason`,
			agent.ProjectID, agent.ApplicationID, snap.HeadReleaseID, nullablePositive(actorID)); err != nil {
			return nil, fmt.Errorf("agent workforce: restore the release head: %w", err)
		}
	}
	for _, roster := range snap.Rosters {
		persona := any(nil)
		if len(roster.Persona) > 0 {
			persona = string(roster.Persona)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.chat_participant_mapping (conversation_id, participant_id, entity_settings, persona, created_at)
SELECT $1, $2, $3::jsonb, $4::jsonb, now() AT TIME ZONE 'UTC'
WHERE EXISTS (SELECT 1 FROM %[1]s.chat_conversations WHERE id = $1)
  AND EXISTS (SELECT 1 FROM %[1]s.chat_participants WHERE id = $2)
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
			roster.ConversationID, roster.ParticipantID, string(roster.EntitySettings), persona); err != nil {
			return nil, fmt.Errorf("agent workforce: restore a roster: %w", err)
		}
	}
	for _, lead := range snap.Leads {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.chat_conversations SET meta = jsonb_set(meta, '{floor}', $2::jsonb)
WHERE id = $1 AND NOT (meta ? 'floor')`, schema), lead.ConversationID, string(lead.Floor)); err != nil {
			return nil, fmt.Errorf("agent workforce: restore a lead: %w", err)
		}
	}
	for _, team := range snap.Teams {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_chat.agent_team_members (team_id, position, application_id, agent_project_id, version_id, name)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM elitea_chat.agent_teams WHERE id = $1)
ON CONFLICT DO NOTHING`, team.TeamID, team.Position, agent.ApplicationID, agent.ProjectID, team.Version, team.Name); err != nil {
			return nil, fmt.Errorf("agent workforce: restore a team seat: %w", err)
		}
		if team.Lead {
			if _, err := tx.Exec(ctx, `
UPDATE elitea_chat.agent_teams SET floor_policy = 'lead', lead_position = $2, updated_at = clock_timestamp()
WHERE id = $1 AND floor_policy = 'addressed'
  AND EXISTS (SELECT 1 FROM elitea_chat.agent_team_members WHERE team_id = $1 AND position = $2 AND application_id = $3)`,
				team.TeamID, team.Position, agent.ApplicationID); err != nil {
				return nil, fmt.Errorf("agent workforce: restore a team lead: %w", err)
			}
		}
	}
	if len(snap.Schedules) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_schedules SET active = true, updated_at = now() WHERE id = ANY($1::int[]) AND application_id = $2`, schema),
			snap.Schedules, agent.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent workforce: turn the schedules back on: %w", err)
		}
	}
	if len(snap.Triggers) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_triggers SET revoked_at = NULL, revoked_by = NULL WHERE id = ANY($1::int[]) AND application_id = $2`, schema),
			snap.Triggers, agent.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent workforce: restore the triggers: %w", err)
		}
	}
	if snap.Heartbeat {
		// The next wake is now, not the stale next_wake_at of before the
		// retirement; a pause from back then is over.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.agent_heartbeat_settings
SET enabled = true, next_wake_at = clock_timestamp(), paused_until = NULL, updated_at = clock_timestamp()
WHERE application_id = $1 AND NOT enabled`, schema), agent.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent workforce: restart the heartbeat: %w", err)
		}
	}
	resumed, err := resumeDismissalSuspension(ctx, tx, agent, actorID, "Reinstated.")
	if err != nil {
		return nil, err
	}
	// An automatically hired agent restarts its probation (ADR-0050
	// decision 5l): the old clock is not restored.
	probationUntil := s.now().UTC().Add(agentworkforce.ProbationWindow)
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET lifecycle_state = 'active', retired_at = NULL, retired_by = NULL, reinstate_until = NULL, purge_after = NULL,
    retire_snapshot = NULL, under_review = false, under_review_since = NULL,
    probation_until = CASE WHEN auto_hired_at IS NOT NULL THEN $3::timestamptz END,
    probation_reviewed_at = NULL, probation_reviewed_by = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID, probationUntil); err != nil {
		return nil, fmt.Errorf("agent workforce: reinstate the registry row: %w", err)
	}
	if agent.AutoHiredAt != nil {
		if _, err := s.insertAction(ctx, tx, agent.ProjectID, agent.ApplicationID, agentworkforce.ActionAutoHire,
			agentworkforce.RuleAutoHire, "", 0, agentworkforce.ActionApplied, &probationUntil,
			map[string]any{"reinstated": true, "probation_until": probationUntil}, true); err != nil {
			return nil, err
		}
	}
	if err := s.append(ctx, tx, agent.ProjectID, "agent.workforce.reinstated", personRef(actorID),
		strconv.FormatInt(agent.ApplicationID, 10), "", "via:"+via,
		fmt.Sprintf("restored_versions:%d", len(snap.PublishedVersions)), fmt.Sprintf("restored_rosters:%d", len(snap.Rosters)),
		fmt.Sprintf("restored_schedules:%d", len(snap.Schedules))); err != nil {
		return nil, err
	}
	return resumed, nil
}

/* ── delete ────────────────────────────────────────────────────────────── */

// purge deletes a retired agent's definition and memory after the
// reinstate window, never under a legal hold. The registry row (state
// deleted), the proposals, the audit, the traces and the scorecards stay.
func (s *AgentWorkforce) purge(ctx context.Context, tx pgx.Tx, agent WorkforceAgent) (map[string]any, error) {
	if agent.LegalHold {
		return nil, agentworkforce.Refuse(agentworkforce.LegalHoldCode, "A legal hold covers this agent; it cannot be deleted.")
	}
	if agent.PurgeAfter == nil || s.now().Before(*agent.PurgeAfter) {
		return nil, &agentworkforce.Refusal{Code: agentworkforce.RetentionCode,
			Message: "The agent can be deleted once its reinstate window has passed.",
			Detail:  map[string]any{"purge_after": agent.PurgeAfter}}
	}
	schema, err := tenantschema.QuoteInt(agent.ProjectID)
	if err != nil {
		return nil, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	var memories int64
	for _, owned := range []string{"agent_memory_entries", "pipeline_schedules", "pipeline_triggers"} {
		exists, err := relationExists(ctx, tx, schema+"."+owned)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.%s WHERE application_id = $1`, schema, owned), agent.ApplicationID)
		if err != nil {
			return nil, fmt.Errorf("agent workforce: purge %s: %w", owned, err)
		}
		if owned == "agent_memory_entries" {
			memories = tag.RowsAffected()
		}
	}
	for _, child := range []struct{ table, column string }{
		{"entity_tool_mapping", "entity_version_id"},
		{"application_variables", "application_version_id"},
		{"application_version_tag_association", "version_id"},
	} {
		exists, err := relationExists(ctx, tx, schema+"."+child.table)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.%s WHERE %s IN (SELECT id FROM %s.application_versions WHERE application_id = $1)`,
			schema, child.table, child.column, schema), agent.ApplicationID); err != nil {
			return nil, fmt.Errorf("agent workforce: purge %s: %w", child.table, err)
		}
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.application_versions WHERE application_id = $1`, schema), agent.ApplicationID)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: purge the versions: %w", err)
	}
	versions := tag.RowsAffected()
	// The applications row stays (its name labels the evidence) with no
	// versions left: nothing can run, publish or nest it.
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET lifecycle_state = 'deleted', retire_snapshot = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, agent.ProjectID, agent.ApplicationID); err != nil {
		return nil, fmt.Errorf("agent workforce: mark the registry row deleted: %w", err)
	}
	return map[string]any{"versions_purged": versions, "memories_purged": memories}, nil
}

/* ── impact preview ────────────────────────────────────────────────────── */

// WorkforceImpact is what a stage would touch, shown before it is applied.
type WorkforceImpact struct {
	Conversations     int              `json:"conversations"`
	ConversationsLed  int              `json:"conversations_led"`
	Teams             []ImpactTeam     `json:"teams"`
	NestedBy          []ImpactNesting  `json:"nested_by"`
	Schedules         int              `json:"schedules"`
	Triggers          int              `json:"triggers"`
	PublishedVersions int              `json:"published_versions"`
	Suspension        *ImpactSuspended `json:"suspension"`
	LegalHold         bool             `json:"legal_hold"`
	LifecycleState    string           `json:"lifecycle_state"`
}

// ImpactTeam is one saved team the agent sits in.
type ImpactTeam struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Lead bool   `json:"lead"`
}

// ImpactNesting is an agent that uses this one as a sub-agent.
type ImpactNesting struct {
	ApplicationID int64  `json:"application_id"`
	Name          string `json:"name"`
}

// ImpactSuspended is the agent's active suspension.
type ImpactSuspended struct {
	ID     int64  `json:"id"`
	Source string `json:"source"`
}

// Impact answers the impact preview of the agent.
func (s *AgentWorkforce) Impact(ctx context.Context, projectID, applicationID int64) (WorkforceImpact, error) {
	agent, err := readWorkforceAgent(ctx, s.pool, projectID, applicationID, false)
	if err != nil {
		return WorkforceImpact{}, err
	}
	impact, err := impactOf(ctx, s.pool, projectID, applicationID)
	if err != nil {
		return WorkforceImpact{}, err
	}
	impact.LegalHold, impact.LifecycleState = agent.LegalHold, agent.State
	if agent.SuspensionID > 0 {
		impact.Suspension = &ImpactSuspended{ID: agent.SuspensionID, Source: agent.SuspensionSource}
	}
	return impact, nil
}

func impactOf(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) (WorkforceImpact, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return WorkforceImpact{}, agentworkforce.Refuse(agentworkforce.InvalidCode, "The project id is invalid.")
	}
	app, project := strconv.FormatInt(applicationID, 10), strconv.FormatInt(projectID, 10)
	impact := WorkforceImpact{Teams: []ImpactTeam{}, NestedBy: []ImpactNesting{}}
	if err := q.QueryRow(ctx, fmt.Sprintf(`
WITH me AS (`+agentParticipantsSQL+`)
SELECT
  (SELECT count(DISTINCT mapping.conversation_id) FROM %[1]s.chat_participant_mapping AS mapping WHERE mapping.participant_id IN (SELECT id FROM me)),
  (SELECT count(*) FROM %[1]s.chat_conversations AS c WHERE c.meta -> 'floor' ->> 'lead_participant_id' IN (SELECT id::text FROM me)),
  (SELECT count(*) FROM %[1]s.application_versions WHERE application_id = $3 AND status IN ('published', 'embedded'))`, schema),
		app, project, applicationID).Scan(&impact.Conversations, &impact.ConversationsLed, &impact.PublishedVersions); err != nil {
		return WorkforceImpact{}, fmt.Errorf("agent workforce: read the conversations: %w", err)
	}
	pipelines, err := relationExists(ctx, q, schema+".pipeline_schedules")
	if err != nil {
		return WorkforceImpact{}, err
	}
	if pipelines {
		if err := q.QueryRow(ctx, fmt.Sprintf(`
SELECT (SELECT count(*) FROM %[1]s.pipeline_schedules WHERE application_id = $1 AND active),
       (SELECT count(*) FROM %[1]s.pipeline_triggers WHERE application_id = $1 AND revoked_at IS NULL)`, schema),
			applicationID).Scan(&impact.Schedules, &impact.Triggers); err != nil {
			return WorkforceImpact{}, fmt.Errorf("agent workforce: read the schedules: %w", err)
		}
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT parent.id, parent.name
FROM %[1]s.elitea_tools AS tool
JOIN %[1]s.entity_tool_mapping AS mapping ON mapping.tool_id = tool.id AND mapping.entity_type = 'agent'
JOIN %[1]s.application_versions AS version ON version.id = mapping.entity_version_id
JOIN %[1]s.applications AS parent ON parent.id = version.application_id
WHERE tool.type = 'application' AND tool.settings ->> 'application_id' = $1 AND parent.id <> $2
ORDER BY parent.id`, schema), app, applicationID)
	if err != nil {
		return WorkforceImpact{}, fmt.Errorf("agent workforce: read the nesting agents: %w", err)
	}
	for rows.Next() {
		var nesting ImpactNesting
		if err := rows.Scan(&nesting.ApplicationID, &nesting.Name); err != nil {
			rows.Close()
			return WorkforceImpact{}, err
		}
		impact.NestedBy = append(impact.NestedBy, nesting)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return WorkforceImpact{}, err
	}
	teams, err := relationExists(ctx, q, "elitea_chat.agent_team_members")
	if err != nil || !teams {
		return impact, err
	}
	teamRows, err := q.Query(ctx, `
SELECT team.id, team.name, COALESCE(team.lead_position = member.position, false)
FROM elitea_chat.agent_team_members AS member
JOIN elitea_chat.agent_teams AS team ON team.id = member.team_id
WHERE member.agent_project_id = $1 AND member.application_id = $2
ORDER BY team.id`, projectID, applicationID)
	if err != nil {
		return WorkforceImpact{}, fmt.Errorf("agent workforce: read the saved teams: %w", err)
	}
	defer teamRows.Close()
	for teamRows.Next() {
		var team ImpactTeam
		if err := teamRows.Scan(&team.ID, &team.Name, &team.Lead); err != nil {
			return WorkforceImpact{}, err
		}
		impact.Teams = append(impact.Teams, team)
	}
	return impact, teamRows.Err()
}
