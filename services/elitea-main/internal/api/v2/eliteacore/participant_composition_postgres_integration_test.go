package eliteacore_test

// FU-244, PLAN §8.8: a conversation runs an agent or pipeline participant as a
// root, so adding one and switching its version are refused when the saved
// composition breaks a bound (a cycle, more than 3 agent levels), with the
// typed composition refusal; nothing is written.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type composedVersion struct{ app, version int64 }

func seedComposedVersion(t *testing.T, pool *pgxpool.Pool, name string) composedVersion {
	t.Helper()
	var saved composedVersion
	if err := pool.QueryRow(context.Background(), `
WITH app AS (INSERT INTO p_1.applications (name, description, owner_id) VALUES ($1, 'seeded', 1) RETURNING id)
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
SELECT id, 'latest', 'draft', 1, 'work', 'agent' FROM app RETURNING application_id, id`, name).Scan(&saved.app, &saved.version); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return saved
}

// attachForTest writes the reference pair Main's attach writes.
func attachForTest(t *testing.T, pool *pgxpool.Pool, parent, child composedVersion, name string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
WITH tool AS (
  INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
  VALUES ($1, 'application', '', jsonb_build_object('application_id', $2::bigint, 'application_version_id', $3::bigint), '{}'::jsonb, 1, 1, now())
  RETURNING id)
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_type, tool_id, entity_id, created_at)
SELECT $4, 'agent', tool.id, $5, now() FROM tool`, name, child.app, child.version, parent.version, parent.app); err != nil {
		t.Fatalf("attach %s: %v", name, err)
	}
}

func participantBody(version composedVersion) map[string]any {
	return map[string]any{
		"entity_name":     "application",
		"entity_meta":     map[string]any{"id": float64(version.app), "name": "agent"},
		"entity_settings": map[string]any{"version_id": float64(version.version)},
	}
}

func expectCompositionRefusal(t *testing.T, err error) {
	t.Helper()
	var api *apierr.APIError
	if !errors.As(err, &api) || api.Status != http.StatusBadRequest || api.Code != applicationcomposition.Code {
		t.Fatalf("err = %v", err)
	}
}

func TestAddingAParticipantWhoseCompositionBreaksABoundIsRefused(t *testing.T) {
	pool := newPublishCopyPool(t)
	repo := repos.NewConversationsRepo(pool)
	conversation := seedCompositionConversation(t, pool)
	ctx := context.Background()

	// Three levels: allowed.
	top, middle, leaf := seedComposedVersion(t, pool, "Top"), seedComposedVersion(t, pool, "Middle"), seedComposedVersion(t, pool, "Leaf")
	attachForTest(t, pool, top, middle, "Middle")
	attachForTest(t, pool, middle, leaf, "Leaf")
	if _, err := repo.AddParticipants(ctx, "1", conversation, []map[string]any{participantBody(top)}); err != nil {
		t.Fatalf("three agent levels must be addable: %v", err)
	}
	// Four levels: refused, nothing mapped.
	root := seedComposedVersion(t, pool, "Root")
	attachForTest(t, pool, root, top, "Top")
	_, err := repo.AddParticipants(ctx, "1", conversation, []map[string]any{participantBody(root)})
	expectCompositionRefusal(t, err)
	// A cycle: refused.
	attachForTest(t, pool, leaf, top, "Top")
	_, err = repo.AddParticipants(ctx, "1", conversation, []map[string]any{participantBody(middle)})
	expectCompositionRefusal(t, err)
	if !strings.Contains(err.Error(), "circular") {
		t.Fatalf("the refusal says circular: %v", err)
	}
	var mapped int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_participant_mapping m JOIN p_1.chat_participants p ON p.id = m.participant_id
		JOIN p_1.chat_conversations c ON c.id = m.conversation_id WHERE c.uuid::text = $1 AND p.entity_name = 'application'`, conversation).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 1 {
		t.Fatalf("only the valid participant is mapped, got %d", mapped)
	}
}

func TestSwitchingAParticipantToAVersionThatClosesACycleIsRefused(t *testing.T) {
	pool := newPublishCopyPool(t)
	repo := repos.NewConversationsRepo(pool)
	conversation := seedCompositionConversation(t, pool)
	ctx := context.Background()

	parent := seedComposedVersion(t, pool, "Parent")
	child := seedComposedVersion(t, pool, "Child")
	attachForTest(t, pool, parent, child, "Child")
	ids, err := repo.AddParticipants(ctx, "1", conversation, []map[string]any{participantBody(child)})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// A second version of the child whose attachment reaches back to that same
	// version: running it closes a cycle. (Reaching ANOTHER version of the same
	// agent is not a cycle: cycles are per version.)
	var cyclic composedVersion
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
		VALUES ($1, 'v2', 'draft', 1, 'work', 'agent') RETURNING application_id, id`, child.app).Scan(&cyclic.app, &cyclic.version); err != nil {
		t.Fatal(err)
	}
	helper := seedComposedVersion(t, pool, "Helper")
	attachForTest(t, pool, cyclic, helper, "Helper")
	attachForTest(t, pool, helper, cyclic, "Child")
	participant := strconv.Itoa(ids[0])
	err = repo.UpdateEntitySettings(ctx, "1", conversation, participant, map[string]any{"version_id": float64(cyclic.version)})
	expectCompositionRefusal(t, err)
	var stored float64
	if err := pool.QueryRow(ctx, `SELECT (m.entity_settings->>'version_id')::float FROM p_1.chat_participant_mapping m WHERE m.participant_id = $1`, ids[0]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if int64(stored) != child.version {
		t.Fatalf("a refused switch left version %v", stored)
	}
	// Switching back to a valid version is accepted.
	if err := repo.UpdateEntitySettings(ctx, "1", conversation, participant, map[string]any{"version_id": float64(child.version)}); err != nil {
		t.Fatalf("a valid switch: %v", err)
	}
}

func seedCompositionConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var conversation string
	if err := pool.QueryRow(context.Background(), `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'composition', 7, 'agent', now() AT TIME ZONE 'UTC') RETURNING uuid::text`).Scan(&conversation); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return conversation
}
