package repos

// ADR-0050 decision 6 (issue 1619): tenant/0234's avatar_pack column round
// trips through the version reads and writes, and an agent participant's
// persona carries its version's pack on the participants read. Against real
// PostgreSQL, because each path is a hand-written query a fake cannot check.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

func TestAvatarPackRoundTripsAndReachesTheParticipantPersona(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	still := "/api/v2/agent_media/1/" + strings.Repeat("ab", 32) + ".png"
	pack := `{"schema":"elitea.avatar_pack/v1","slots":{"still":{"url":"` + still + `","media_type":"image/png"}}}`

	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.applications (id, name, description, owner_id) VALUES (61, 'Avatar Agent', 'fixture', 11);
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at
) VALUES (
    71, 61, 'base', 'draft', 11, '80000000-0000-4000-8000-000000000061',
    '{}'::jsonb, '', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC'
)`); err != nil {
		t.Fatal(err)
	}

	apps := NewApplicationsRepo(pool)
	if _, err := apps.UpdateVersion(ctx, "1", "61", "71", applications.Version{
		AvatarPack: json.RawMessage(pack),
		Present:    applications.VersionFieldSet{AvatarPack: true},
	}); err != nil {
		t.Fatalf("update version: %v", err)
	}
	version, err := apps.GetVersion(ctx, "1", "61", "71")
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(version.AvatarPack, &stored); err != nil || stored["schema"] != "elitea.avatar_pack/v1" {
		t.Fatalf("stored pack = %s (%v)", version.AvatarPack, err)
	}

	var conversationID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'avatar', 7, 'agent', now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'application', '{"id": 61, "project_id": 1}'::jsonb)
    RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
SELECT $1, participant.id, '{"version_id": 71}'::jsonb, now() AT TIME ZONE 'UTC' FROM participant`, conversationID); err != nil {
		t.Fatal(err)
	}

	participants, err := NewConversationsRepo(pool).ListParticipants(ctx, "1", itoa(int64(conversationID)))
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants = %d", len(participants))
	}
	got, _ := participants[0].Persona["avatar_pack"].(map[string]any)
	slots, _ := got["slots"].(map[string]any)
	stillSlot, _ := slots["still"].(map[string]any)
	if stillSlot["url"] != still {
		t.Fatalf("persona.avatar_pack = %v", participants[0].Persona)
	}

	// Cleared: the next read carries no pack.
	if _, err := apps.UpdateVersion(ctx, "1", "61", "71", applications.Version{Present: applications.VersionFieldSet{AvatarPack: true}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	participants, err = NewConversationsRepo(pool).ListParticipants(ctx, "1", itoa(int64(conversationID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := participants[0].Persona["avatar_pack"]; ok {
		t.Fatalf("a cleared pack still reached the persona: %v", participants[0].Persona)
	}
}
