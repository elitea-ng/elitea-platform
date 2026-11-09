package repos

// Saved teams (ADR-0050 decision 8, issue 1620): the store over shared/0232's
// elitea_chat.agent_teams and agent_team_members, and the reads that turn a
// conversation's roster into a team.
//
// A team grants nothing. This store never checks whether the caller may use
// a member agent: the conversation create that starts a team does, with the
// participant path's own project-access and composition checks, so the rule
// lives in one place (application_composition.go participantSchema).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// agentTeamNameKey is the per-project, case-insensitive name index.
const agentTeamNameKey = "agent_teams_project_name_key"

// AgentTeams is the team store.
type AgentTeams struct {
	pool *pgxpool.Pool
}

// NewAgentTeams builds the store. A nil pool answers nil.
func NewAgentTeams(pool *pgxpool.Pool) *AgentTeams {
	if pool == nil {
		return nil
	}
	return &AgentTeams{pool: pool}
}

const agentTeamColumns = `team.id, team.project_id, team.name, team.description, team.floor_policy,
       COALESCE(team.lead_position, 0), team.created_by, team.created_at, team.updated_at`

func scanAgentTeam(row pgx.Row) (agentteams.Stored, error) {
	var team agentteams.Stored
	var policy string
	err := row.Scan(&team.ID, &team.ProjectID, &team.Name, &team.Description, &policy,
		&team.Lead, &team.CreatedBy, &team.CreatedAt, &team.UpdatedAt)
	team.Policy = turntaking.Policy(policy)
	return team, err
}

// ListTeams answers a project's teams by name, with their members, and the
// total before limit and offset. query filters on the name.
func (s *AgentTeams) ListTeams(ctx context.Context, projectID int64, query string, limit, offset int) ([]agentteams.Stored, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	pattern := ""
	if query = strings.TrimSpace(query); query != "" {
		pattern = "%" + likeEscaper.Replace(query) + "%"
	}
	var total int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_chat.agent_teams AS team
WHERE team.project_id = $1 AND ($2 = '' OR team.name ILIKE $2)`, projectID, pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("agent teams: count: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+agentTeamColumns+`
FROM elitea_chat.agent_teams AS team
WHERE team.project_id = $1 AND ($2 = '' OR team.name ILIKE $2)
ORDER BY lower(team.name), team.id
LIMIT $3 OFFSET $4`, projectID, pattern, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("agent teams: list: %w", err)
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (agentteams.Stored, error) { return scanAgentTeam(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("agent teams: scan: %w", err)
	}
	if err := s.loadMembers(ctx, teams); err != nil {
		return nil, 0, err
	}
	return teams, total, nil
}

// likeEscaper escapes the ILIKE wildcards of a search text.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// GetTeam answers one team of the project, or agentteams.ErrNotFound.
func (s *AgentTeams) GetTeam(ctx context.Context, projectID, teamID int64) (agentteams.Stored, error) {
	team, err := scanAgentTeam(s.pool.QueryRow(ctx, `
SELECT `+agentTeamColumns+`
FROM elitea_chat.agent_teams AS team WHERE team.project_id = $1 AND team.id = $2`, projectID, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return agentteams.Stored{}, agentteams.ErrNotFound
	}
	if err != nil {
		return agentteams.Stored{}, fmt.Errorf("agent teams: get: %w", err)
	}
	teams := []agentteams.Stored{team}
	if err := s.loadMembers(ctx, teams); err != nil {
		return agentteams.Stored{}, err
	}
	return teams[0], nil
}

func (s *AgentTeams) loadMembers(ctx context.Context, teams []agentteams.Stored) error {
	if len(teams) == 0 {
		return nil
	}
	ids := make([]int64, len(teams))
	index := make(map[int64]int, len(teams))
	for position, team := range teams {
		ids[position] = team.ID
		index[team.ID] = position
	}
	rows, err := s.pool.Query(ctx, `
SELECT team_id, application_id, agent_project_id, version_id, name
FROM elitea_chat.agent_team_members WHERE team_id = ANY($1::bigint[])
ORDER BY team_id, position`, ids)
	if err != nil {
		return fmt.Errorf("agent teams: members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var teamID int64
		var member agentteams.Member
		if err := rows.Scan(&teamID, &member.ApplicationID, &member.ProjectID, &member.VersionID, &member.Name); err != nil {
			return fmt.Errorf("agent teams: scan a member: %w", err)
		}
		team := &teams[index[teamID]]
		team.Members = append(team.Members, member)
	}
	return rows.Err()
}

// CreateTeam saves a normalized team in the project.
func (s *AgentTeams) CreateTeam(ctx context.Context, projectID, actorID int64, team agentteams.Team) (agentteams.Stored, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_chat.agent_teams (project_id, name, description, floor_policy, lead_position, created_by)
VALUES ($1, $2, $3, $4, NULLIF($5, 0), $6) RETURNING id`,
			projectID, team.Name, team.Description, string(team.Policy), team.Lead, actorID).Scan(&id); err != nil {
			return err
		}
		return writeTeamMembers(ctx, tx, id, team.Members)
	})
	if err != nil {
		return agentteams.Stored{}, teamWriteError("create", err)
	}
	return s.GetTeam(ctx, projectID, id)
}

// UpdateTeam replaces a team's fields and roster.
func (s *AgentTeams) UpdateTeam(ctx context.Context, projectID, teamID int64, team agentteams.Team) (agentteams.Stored, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE elitea_chat.agent_teams
SET name = $3, description = $4, floor_policy = $5, lead_position = NULLIF($6, 0), updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2`,
			projectID, teamID, team.Name, team.Description, string(team.Policy), team.Lead)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return agentteams.ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM elitea_chat.agent_team_members WHERE team_id = $1`, teamID); err != nil {
			return err
		}
		return writeTeamMembers(ctx, tx, teamID, team.Members)
	})
	if err != nil {
		return agentteams.Stored{}, teamWriteError("update", err)
	}
	return s.GetTeam(ctx, projectID, teamID)
}

// DeleteTeam deletes a team of the project.
func (s *AgentTeams) DeleteTeam(ctx context.Context, projectID, teamID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM elitea_chat.agent_teams WHERE project_id = $1 AND id = $2`, projectID, teamID)
	if err != nil {
		return fmt.Errorf("agent teams: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return agentteams.ErrNotFound
	}
	return nil
}

func writeTeamMembers(ctx context.Context, tx pgx.Tx, teamID int64, members []agentteams.Member) error {
	for index, member := range members {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_chat.agent_team_members (team_id, position, application_id, agent_project_id, version_id, name)
VALUES ($1, $2, $3, $4, $5, $6)`, teamID, index+1, member.ApplicationID, member.ProjectID, member.VersionID, member.Name); err != nil {
			return err
		}
	}
	return nil
}

func teamWriteError(verb string, err error) error {
	switch {
	case errors.Is(err, agentteams.ErrNotFound):
		return err
	case isUniqueViolation(err, agentTeamNameKey):
		return agentteams.ErrNameTaken
	}
	return fmt.Errorf("agent teams: %s: %w", verb, err)
}

// ConversationRoster reads a conversation's agents as a team roster, in the
// order they joined, and the lead's position under its floor (0 without a
// lead). Only `application` participants with a stored version are members;
// toolkits, models and people are not part of a team. The caller authorized
// the conversation (AuthorizeChatResource) before this read.
func (s *AgentTeams) ConversationRoster(ctx context.Context, projectID int64, conversationID string) ([]agentteams.Member, int, error) {
	schema, err := tenantSchema(fmt.Sprint(projectID))
	if err != nil {
		return nil, 0, err
	}
	predicate, ok := idPredicate(conversationID)
	if !ok {
		return nil, 0, agentteams.ErrNotFound
	}
	var rawFloor []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT c.meta -> 'floor' FROM %s.chat_conversations c WHERE %s`, schema, predicate),
		conversationID).Scan(&rawFloor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, agentteams.ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("agent teams: read the conversation: %w", err)
	}
	floor := turntaking.Default
	if len(rawFloor) > 0 {
		var decoded any
		if json.Unmarshal(rawFloor, &decoded) == nil {
			if parsed, parseErr := turntaking.Parse(decoded); parseErr == nil {
				floor = parsed
			}
		}
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT participant.id,
       participant.entity_meta ->> 'id',
       COALESCE(participant.entity_meta ->> 'project_id', ''),
       COALESCE(mapping.entity_settings ->> 'version_id', ''),
       COALESCE(NULLIF(btrim(mapping.persona ->> 'display_name'), ''),
                NULLIF(btrim(participant.meta ->> 'name'), ''),
                NULLIF(btrim(participant.entity_meta ->> 'name'), ''), '')
FROM %[1]s.chat_conversations AS c
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = c.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'application'
WHERE %[2]s
ORDER BY mapping.id`, schema, predicate), conversationID)
	if err != nil {
		return nil, 0, fmt.Errorf("agent teams: read the roster: %w", err)
	}
	defer rows.Close()
	var members []agentteams.Member
	lead := 0
	for rows.Next() {
		var participantID int64
		var applicationText, projectText, versionText, name string
		if err := rows.Scan(&participantID, &applicationText, &projectText, &versionText, &name); err != nil {
			return nil, 0, fmt.Errorf("agent teams: scan the roster: %w", err)
		}
		applicationID, okApplication := positiveInteger(applicationText)
		versionID, okVersion := positiveInteger(versionText)
		if !okApplication || !okVersion {
			continue
		}
		memberProject, okProject := positiveInteger(projectText)
		if !okProject {
			memberProject = projectID
		}
		members = append(members, agentteams.Member{
			ApplicationID: applicationID, ProjectID: memberProject, VersionID: versionID, Name: name,
		})
		if floor.Policy == turntaking.PolicyLead && participantID == floor.Lead {
			lead = len(members)
		}
	}
	return members, lead, rows.Err()
}

// MissingMembers answers the position (1-based) of the first member of the
// team's own project whose agent or version no longer exists there, 0 when
// every one does. A member of another project is not read here: the
// participant path authorizes that project before it reads anything in it,
// and refuses without saying whether the agent exists.
func (s *AgentTeams) MissingMembers(ctx context.Context, projectID int64, members []agentteams.Member) (int, error) {
	schema, err := tenantSchema(fmt.Sprint(projectID))
	if err != nil {
		return 0, err
	}
	for index, member := range members {
		if member.ProjectID != projectID {
			continue
		}
		var exists bool
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (
	SELECT 1 FROM %[1]s.application_versions AS version
	JOIN %[1]s.applications AS application ON application.id = version.application_id
	WHERE version.id = $1 AND version.application_id = $2)`, schema),
			member.VersionID, member.ApplicationID).Scan(&exists); err != nil {
			return 0, fmt.Errorf("agent teams: read a member: %w", err)
		}
		if !exists {
			return index + 1, nil
		}
	}
	return 0, nil
}
