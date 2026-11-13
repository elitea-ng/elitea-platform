package repos

// Saved teams against a real PostgreSQL (ADR-0050 decision 8, issue 1620):
// the CRUD of shared/0233, the per-project unique name, a conversation's
// roster and lead read back as a team, the missing-member probe, and a
// conversation created from a team writing its floor with the lead's new
// participant id. Requires ELITEA_TEST_DATABASE_URL; every test skips
// without it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

const teamActor = 7

func newAgentTeamsPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 30*time.Second)
	t.Cleanup(cancel)
	for _, statement := range []string{
		// The shared seed writes fixed ids; move the sequences past them.
		`SELECT setval(pg_get_serial_sequence('p_1.chat_conversations', 'id'), (SELECT max(id) FROM p_1.chat_conversations))`,
		`SELECT setval(pg_get_serial_sequence('p_1.chat_participants', 'id'), (SELECT max(id) FROM p_1.chat_participants))`,
		`SELECT setval(pg_get_serial_sequence('p_1.chat_participant_mapping', 'id'), (SELECT max(id) FROM p_1.chat_participant_mapping))`,
		`INSERT INTO p_1.applications (id, name, description, owner_id) VALUES
		 (131, 'Mira', 'release notes', 7), (132, 'CI Doctor', 'fixes CI', 7)`,
		`INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
		 conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at) VALUES
		 (141, 131, 'base', 'draft', 7, '80000000-0000-4000-8000-000000000141', '{}'::jsonb, '', '[]'::json, '', 'agent',
		  '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
		 (142, 132, 'base', 'draft', 7, '80000000-0000-4000-8000-000000000142', '{}'::jsonb, '', '[]'::json, '', 'agent',
		  '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	grantPinMembership(t, pool, 1, teamActor)
	return pool, ctx
}

func teamOfTwo(name string) agentteams.Team {
	return agentteams.Team{Name: name, Policy: turntaking.PolicyLead, Lead: 2, Members: []agentteams.Member{
		{ApplicationID: 131, ProjectID: 1, VersionID: 141, Name: "Mira"},
		{ApplicationID: 132, ProjectID: 1, VersionID: 142, Name: "CI Doctor"},
	}}
}

func TestAgentTeamsCrudAndUniqueName(t *testing.T) {
	pool, ctx := newAgentTeamsPool(t)
	store := NewAgentTeams(pool)

	created, err := store.CreateTeam(ctx, 1, teamActor, teamOfTwo("Release crew"))
	if err != nil || created.ID == 0 || created.CreatedBy != teamActor {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	if _, err := store.CreateTeam(ctx, 1, teamActor, teamOfTwo("release CREW")); !errors.Is(err, agentteams.ErrNameTaken) {
		t.Fatalf("duplicate name err = %v", err)
	}
	got, err := store.GetTeam(ctx, 1, created.ID)
	if err != nil || got.Lead != 2 || got.Policy != turntaking.PolicyLead || len(got.Members) != 2 || got.Members[1].Name != "CI Doctor" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	// Another project does not see it.
	if _, err := store.GetTeam(ctx, 2, created.ID); !errors.Is(err, agentteams.ErrNotFound) {
		t.Fatalf("other project err = %v", err)
	}

	rows, total, err := store.ListTeams(ctx, 1, "100%_crew", 50, 0, nil)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("a LIKE metacharacter matched: rows=%d total=%d err=%v", len(rows), total, err)
	}
	rows, total, err = store.ListTeams(ctx, 1, "lease", 50, 0, nil)
	if err != nil || total != 1 || len(rows) != 1 || len(rows[0].Members) != 2 {
		t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
	}

	update := agentteams.Team{Name: "Solo", Policy: turntaking.PolicyAddressed,
		Members: []agentteams.Member{{ApplicationID: 132, ProjectID: 1, VersionID: 142}}}
	updated, err := store.UpdateTeam(ctx, 1, created.ID, update)
	if err != nil || updated.Name != "Solo" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	got, err = store.GetTeam(ctx, 1, created.ID)
	if err != nil || got.Lead != 0 || len(got.Members) != 1 || got.Members[0].ApplicationID != 132 {
		t.Fatalf("after update=%+v err=%v", got, err)
	}
	if _, err := store.UpdateTeam(ctx, 2, created.ID, update); !errors.Is(err, agentteams.ErrNotFound) {
		t.Fatalf("update in another project err = %v", err)
	}
	if err := store.DeleteTeam(ctx, 1, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTeam(ctx, 1, created.ID); !errors.Is(err, agentteams.ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_chat.agent_team_members`).Scan(&members); err != nil || members != 0 {
		t.Fatalf("members left after delete = %d err=%v", members, err)
	}
}

func TestAgentTeamsMissingMembers(t *testing.T) {
	pool, ctx := newAgentTeamsPool(t)
	store := NewAgentTeams(pool)
	team := teamOfTwo("x").Members
	if missing, err := store.MissingMembers(ctx, 1, team); err != nil || missing != 0 {
		t.Fatalf("missing=%d err=%v", missing, err)
	}
	team[1].VersionID = 99
	if missing, err := store.MissingMembers(ctx, 1, team); err != nil || missing != 2 {
		t.Fatalf("missing=%d err=%v, want 2", missing, err)
	}
	// A member of another project is left to the participant path.
	other := []agentteams.Member{{ApplicationID: 5, ProjectID: 2, VersionID: 6}}
	if missing, err := store.MissingMembers(ctx, 1, other); err != nil || missing != 0 {
		t.Fatalf("missing=%d err=%v", missing, err)
	}
}

func TestAConversationFromATeamRoundTrips(t *testing.T) {
	pool, ctx := newAgentTeamsPool(t)
	store := NewAgentTeams(pool)
	team := teamOfTwo("Release crew")
	participants := make([]conversations.Participant, 0, len(team.Members))
	for _, member := range team.Members {
		participants = append(participants, conversations.Participant{
			EntityName:     "application",
			EntityMeta:     map[string]any{"id": member.ApplicationID, "project_id": member.ProjectID, "name": member.Name},
			EntitySettings: map[string]any{"version_id": member.VersionID},
		})
	}
	created, err := NewConversationsRepo(pool).Create(ctx, "1", conversations.Conversation{
		Name: "Release crew", Participants: participants, LeadParticipant: team.Lead,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	floor, err := turntaking.Parse(created.Meta["floor"])
	if err != nil || floor.Policy != turntaking.PolicyLead || floor.Lead == 0 {
		t.Fatalf("floor=%+v err=%v meta=%v", floor, err, created.Meta)
	}
	var leadApplication string
	if err := pool.QueryRow(ctx, `SELECT entity_meta ->> 'id' FROM p_1.chat_participants WHERE id = $1`,
		floor.Lead).Scan(&leadApplication); err != nil || leadApplication != "132" {
		t.Fatalf("the lead is participant of application %q err=%v, want 132", leadApplication, err)
	}

	// Saving that conversation as a team gives the same roster and lead back.
	members, lead, err := store.ConversationRoster(ctx, 1, created.ID)
	if err != nil || lead != 2 || len(members) != 2 || members[0].ApplicationID != 131 || members[1].VersionID != 142 {
		t.Fatalf("members=%+v lead=%d err=%v", members, lead, err)
	}
	if _, _, err := store.ConversationRoster(ctx, 1, "999999"); !errors.Is(err, agentteams.ErrNotFound) {
		t.Fatalf("unknown conversation err = %v", err)
	}

	// A lead whose participant row has no stored version cannot be a member;
	// dropping it would save the team with an addressed floor, so the roster
	// refuses instead.
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_participant_mapping SET entity_settings = entity_settings - 'version_id'
		WHERE participant_id = $1`, floor.Lead); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConversationRoster(ctx, 1, created.ID); !errors.Is(err, agentteams.ErrInvalid) {
		t.Fatalf("unpinned lead err = %v, want ErrInvalid", err)
	}
}

// A keyset cursor pages by (lower(name), id), so a team deleted between pages
// does not make the next page skip one (issue 1955).
func TestAgentTeamsKeysetPaging(t *testing.T) {
	pool, ctx := newAgentTeamsPool(t)
	store := NewAgentTeams(pool)
	for _, name := range []string{"Alpha", "bravo", "Charlie", "delta"} {
		if _, err := store.CreateTeam(ctx, 1, teamActor, teamOfTwo(name)); err != nil {
			t.Fatal(err)
		}
	}
	first, total, err := store.ListTeams(ctx, 1, "", 2, 0, nil)
	if err != nil || total != 4 || len(first) != 2 || first[1].Name != "bravo" {
		t.Fatalf("first=%+v total=%d err=%v", first, total, err)
	}
	// The team the first page ended on is deleted before the second is read:
	// an offset of 2 would now skip "Charlie"; the cursor does not.
	if err := store.DeleteTeam(ctx, 1, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ListTeams(ctx, 1, "", 2, 0, &agentteams.ListCursor{Name: first[1].Name, ID: first[1].ID})
	if err != nil || len(second) != 2 || second[0].Name != "Charlie" || second[1].Name != "delta" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}
