// Package agentteams is the rules of a saved team (ADR-0050 decision 8,
// issue 1620): a project's roster of agents, its floor policy and its lead,
// kept so a person can start a new conversation with the same agents.
//
// # A team adds no capability
//
// A team names agents; it grants nothing. Starting a conversation from a team
// adds its agents through the ordinary participant path, which applies the
// same project-access and composition checks a hand-made "add participant"
// does, under the caller's own identity. So a person can start from a team
// only a roster they could have assembled themself, and each agent keeps its
// own grants. A team that names an agent the caller cannot use is refused
// whole (team_member_unavailable), not started with that agent missing.
//
// # The floor
//
// The policy and the lead are the conversation floor of package turntaking
// (meta.floor): `addressed`, or `lead` with one member as the lead. A team
// stores its lead as a member position because a participant id belongs to
// one conversation; starting from the team writes the new conversation's
// floor with the lead's new participant id.
//
// The tables are shared/0233 (elitea_chat.agent_teams and
// agent_team_members); storage is repos.AgentTeams.
package agentteams

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// ListCursor is the keyset position of a teams page: the name and id of the
// last team the caller holds. The list is ordered by lower(name), then id, so
// the next page is every team after that pair, and a team created or deleted
// between pages cannot shift the window (issue 1955).
type ListCursor struct {
	Name string
	ID   int64
}

// The permissions (shared 0233).
const (
	ListPermission   = "models.teams.list"
	CreatePermission = "models.teams.create"
	UpdatePermission = "models.teams.update"
	DeletePermission = "models.teams.delete"
)

// Limits.
const (
	// MaxMembers bounds a roster: the agents one message may address.
	MaxMembers  = turntaking.MaxAddressed
	MaxName     = 128
	MaxDesc     = 2000
	MaxNameSnap = 256
)

// Errors. ErrInvalid is a team that breaks a rule; the message says which.
var (
	ErrInvalid           = errors.New("invalid team")
	ErrNotFound          = errors.New("team not found")
	ErrNameTaken         = errors.New("a team with this name already exists in the project")
	ErrMemberUnavailable = errors.New("an agent of the team is not available to the caller")
)

// MemberUnavailable names the member a start refused (1-based position).
type MemberUnavailable struct {
	Position int
}

func (e *MemberUnavailable) Error() string {
	return fmt.Sprintf("agent %d of the team does not exist or is not available to you", e.Position)
}

func (e *MemberUnavailable) Unwrap() error { return ErrMemberUnavailable }

// Member is one agent of a roster.
type Member struct {
	ApplicationID int64 `json:"application_id"`
	// ProjectID is the project that owns the agent: the team's own, or the
	// public catalogue project.
	ProjectID int64 `json:"project_id"`
	VersionID int64 `json:"version_id"`
	// Name is the agent's name when the team was saved.
	Name string `json:"name"`
}

// Team is a saved team.
type Team struct {
	Name        string
	Description string
	Policy      turntaking.Policy
	// Lead is the lead member's position (1-based); 0 under `addressed`.
	Lead    int
	Members []Member
}

// Stored is a team as the store keeps it.
type Stored struct {
	Team
	ID        int64
	ProjectID int64
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Normalize trims the text fields and checks every rule.
func (t *Team) Normalize() error {
	t.Name = strings.TrimSpace(t.Name)
	t.Description = strings.TrimSpace(t.Description)
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalid)
	case utf8.RuneCountInString(t.Name) > MaxName:
		return fmt.Errorf("%w: name is longer than %d characters", ErrInvalid, MaxName)
	case utf8.RuneCountInString(t.Description) > MaxDesc:
		return fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, MaxDesc)
	case len(t.Members) == 0:
		return fmt.Errorf("%w: a team needs at least one agent", ErrInvalid)
	case len(t.Members) > MaxMembers:
		return fmt.Errorf("%w: a team has at most %d agents", ErrInvalid, MaxMembers)
	}
	if t.Policy == "" {
		t.Policy = turntaking.PolicyAddressed
	}
	seen := make(map[[2]int64]bool, len(t.Members))
	for index := range t.Members {
		member := &t.Members[index]
		if member.ApplicationID <= 0 || member.ProjectID <= 0 || member.VersionID <= 0 {
			return fmt.Errorf("%w: member %d needs a positive application_id, project_id and version_id", ErrInvalid, index+1)
		}
		key := [2]int64{member.ProjectID, member.ApplicationID}
		if seen[key] {
			return fmt.Errorf("%w: an agent is in the team twice", ErrInvalid)
		}
		seen[key] = true
		member.Name = truncate(strings.TrimSpace(member.Name), MaxNameSnap)
	}
	switch t.Policy {
	case turntaking.PolicyAddressed:
		if t.Lead != 0 {
			return fmt.Errorf("%w: a lead needs floor_policy lead", ErrInvalid)
		}
	case turntaking.PolicyLead:
		if t.Lead < 1 || t.Lead > len(t.Members) {
			return fmt.Errorf("%w: floor_policy lead needs a lead that is a member", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: floor_policy must be addressed or lead", ErrInvalid)
	}
	return nil
}

func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}
