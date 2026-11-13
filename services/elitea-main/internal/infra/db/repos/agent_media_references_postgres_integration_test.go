package repos

// #1855: the orphan sweep's reference set, against real PostgreSQL because
// it is hand-written SQL over every tenant schema and the lifecycle store.

import (
	"context"
	"strings"
	"testing"
)

func TestAgentMediaReferencesReadEveryPlaceAURLIsKept(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	ctx := context.Background()
	file := func(c string) string { return strings.Repeat(c, 64) + ".png" }
	url := func(project, f string) string { return "/api/v2/agent_media/" + project + "/" + f }

	pack := `{"schema":"elitea.avatar_pack/v1","slots":{"still":{"url":"` + url("1", file("a")) + `","media_type":"image/png"}}}`
	icon := `{"icon_meta":{"url":"` + url("7", file("b")) + `"}}`
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.applications (id, name, description, owner_id) VALUES (61, 'Avatar Agent', 'fixture', 11)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, avatar_pack, created_at
) VALUES (
    71, 61, 'base', 'draft', 11, '80000000-0000-4000-8000-000000000061',
    '{}'::jsonb, '', '[]'::json, '', 'agent', $1::jsonb, '{}'::jsonb, $2::jsonb, now() AT TIME ZONE 'UTC'
)`, icon, pack); err != nil {
		t.Fatal(err)
	}
	// A participant's icon snapshot.
	for _, table := range []string{"chat_conversations", "chat_participants", "chat_participant_mapping"} {
		if _, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('p_1.`+table+`', 'id'), (SELECT max(id) FROM p_1.`+table+`))`); err != nil {
			t.Fatal(err)
		}
	}
	var conversationID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'refs', 7, 'agent', now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'application', '{"id": 61, "project_id": 1}'::jsonb)
    RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
SELECT $1, participant.id, jsonb_build_object('icon_meta', jsonb_build_object('url', $2::text)), now() AT TIME ZONE 'UTC' FROM participant`,
		conversationID, url("1", file("c"))); err != nil {
		t.Fatal(err)
	}
	// A revision snapshot that a restore would bring back.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.version_revisions
	(tenant_project_id, application_id, application_version_id, revision, kind, snapshot, diff)
VALUES (1, 61, 71, 1, 'baseline', jsonb_build_object('avatar_pack', jsonb_build_object('slots', jsonb_build_object('still', jsonb_build_object('url', $1::text)))), '[]'::jsonb)`,
		url("1", file("d"))); err != nil {
		t.Fatal(err)
	}

	repo, err := NewAgentMediaReferencesRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := repo.TenantProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) == 0 || projects[0] != 1 {
		t.Fatalf("tenant projects = %v, want project 1 among them", projects)
	}
	referenced, err := repo.Referenced(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		AgentMediaReferenceKey(1, file("a")), // the live pack
		AgentMediaReferenceKey(7, file("b")), // another project's file, named by this one
		AgentMediaReferenceKey(1, file("c")), // a participant's snapshot
		AgentMediaReferenceKey(1, file("d")), // a revision
	} {
		if _, ok := referenced[key]; !ok {
			t.Errorf("%s is not in the reference set %v", key, referenced)
		}
	}
	if _, ok := referenced[AgentMediaReferenceKey(1, file("e"))]; ok {
		t.Fatal("an unnamed file is referenced")
	}
}
