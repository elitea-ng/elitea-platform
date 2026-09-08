package eliteacore_test

// Real-PostgreSQL coverage for the skill-mapping copy in the PUBLISH path
// (handler.go, Handler.Publish) — issue #351.
//
// Publish copied entity_tool_mapping and nothing else, so a published agent
// carried no skills. The caller got 200 and a published version, and the
// attachments were simply absent. Fork already copies them
// (internal/api/oapiserver/publishing.go:149), so publish now matches fork.
//
// These tests never assert on the status code alone. A 200 from Publish is
// exactly what the defect produced. The assertions read the mapping rows back
// out of the database, through the predicate the chat read uses
// (internal/db/queries/agent_chat.sql:131-132): entity_version_id AND
// entity_type. There is no `entity_id` column on this table
// (001_initial.sql:422-432) — those two columns are the whole key that makes a
// copied row visible to the reader.
//
// The schema comes from the REAL migration corpus, through the
// newPublishCopyPool helper of publish_tool_copy_postgres_integration_test.go.
// A hand-built fixture that declares entity_type nullable is how this class of
// defect survives.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// publishSkillAttachment is one seeded row of p_1.entity_skill_mapping.
type publishSkillAttachment struct {
	skillID int
	// skillVersionID is nullable in the table, so it is held as a pointer. A
	// published skill whose version is dropped renders with empty instructions.
	skillVersionID *int
	entityType     string
}

// publishSkillFixture is one agent in p_1 with two skills attached to its single
// draft version.
type publishSkillFixture struct {
	appID       int
	versionID   int
	attachments []publishSkillAttachment
}

// TestPublishCarriesSkillAttachments is the acceptance test #351 asks for:
// publish an agent that HAS skills attached, then prove the copied rows exist on
// the published version and carry the key the reader matches on.
func TestPublishCarriesSkillAttachments(t *testing.T) {
	pool := newPublishCopyPool(t)
	assertPublishSkillKeyColumns(t, pool)
	fixture := seedPublishSkillFixture(t, pool)
	router := publishCopyRouter(eliteacore.NewHandler(pool))

	recorder := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	publishedVersionID := publishedVersionIDFrom(t, recorder.Body.Bytes())
	if publishedVersionID == fixture.versionID {
		t.Fatalf("publish returned the source version %d", publishedVersionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Read the copied rows through the chat read's own predicate. A row that
	// carries the wrong entity_type is invisible to that read, so selecting on it
	// proves the column and the presence of the row in one statement.
	copied := readPublishSkillAttachments(t, ctx, pool, publishedVersionID)
	if len(copied) != len(fixture.attachments) {
		t.Fatalf("published version carries %d skill attachments, want %d", len(copied), len(fixture.attachments))
	}
	for index, want := range fixture.attachments {
		got := copied[index]
		if got.skillID != want.skillID {
			t.Errorf("copied attachment %d: skill_id = %d, want %d", index, got.skillID, want.skillID)
		}
		if got.entityType != want.entityType {
			t.Errorf("copied attachment %d: entity_type = %q, want %q", index, got.entityType, want.entityType)
		}
		// The chat read LEFT JOINs skill_version_id for the skill instructions.
		// Dropping it publishes a named skill with an empty body.
		switch {
		case want.skillVersionID == nil && got.skillVersionID != nil:
			t.Errorf("copied attachment %d: skill_version_id = %d, want NULL", index, *got.skillVersionID)
		case want.skillVersionID != nil && got.skillVersionID == nil:
			t.Errorf("copied attachment %d: skill_version_id = NULL, want %d", index, *want.skillVersionID)
		case want.skillVersionID != nil && *got.skillVersionID != *want.skillVersionID:
			t.Errorf("copied attachment %d: skill_version_id = %d, want %d", index, *got.skillVersionID, *want.skillVersionID)
		}
	}

	// The source version must keep its own attachments. A copy that moved them
	// would satisfy every assertion above and still break the draft.
	if source := readPublishSkillAttachments(t, ctx, pool, fixture.versionID); len(source) != len(fixture.attachments) {
		t.Errorf("source version carries %d skill attachments after the publish, want %d", len(source), len(fixture.attachments))
	}
}

// TestPublishReportsAFailedSkillCopy proves the new copy does not repeat the
// defect #350 fixed on the tool copy. A failing skill copy must roll the publish
// back and answer an error, so the caller can see that it must retry.
func TestPublishReportsAFailedSkillCopy(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedPublishSkillFixture(t, pool)
	router := publishCopyRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Make the skill-mapping copy the statement that fails, and leave the version
	// clone before it untouched. The seeded rows satisfy this constraint; every
	// copied row carries a higher entity_version_id and cannot. The fixture
	// attaches no toolkit, so the tool copy ahead of it writes no row and passes.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
ALTER TABLE p_1.entity_skill_mapping ADD CONSTRAINT copy_must_fail CHECK (entity_version_id < %d)`,
		fixture.versionID+1)); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	recorder := publishCopyDo(t, router, fixture.versionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.versionID),
	})
	if recorder.Code == http.StatusOK {
		t.Fatalf("publish reported success while the skill copy failed: %s", recorder.Body.String())
	}

	var publishedVersions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND status = 'published'`,
		fixture.appID).Scan(&publishedVersions); err != nil {
		t.Fatal(err)
	}
	if publishedVersions != 0 {
		t.Fatalf("failed publish left %d published versions behind", publishedVersions)
	}

	// The source version must still hold its own attachments.
	if source := readPublishSkillAttachments(t, ctx, pool, fixture.versionID); len(source) != len(fixture.attachments) {
		t.Fatalf("failed publish left the source with %d skill attachments, want the untouched %d",
			len(source), len(fixture.attachments))
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func publishedVersionIDFrom(t *testing.T, body []byte) int {
	t.Helper()
	var response struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode publish response %q: %v", string(body), err)
	}
	var publishedVersionID int
	if _, err := fmt.Sscanf(response.PublicVersionID, "%d", &publishedVersionID); err != nil {
		t.Fatalf("publish returned public_version_id %q: %v", response.PublicVersionID, err)
	}
	return publishedVersionID
}

// readPublishSkillAttachments selects the attachments of one version through the
// predicate of the chat read (internal/db/queries/agent_chat.sql:131-132), in
// its ordering.
func readPublishSkillAttachments(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionID int) []publishSkillAttachment {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT skill_id, skill_version_id, entity_type
FROM p_1.entity_skill_mapping
WHERE entity_version_id = $1 AND entity_type = 'agent'
ORDER BY id`, versionID)
	if err != nil {
		t.Fatalf("read skill attachments of version %d: %v", versionID, err)
	}
	defer rows.Close()
	var attachments []publishSkillAttachment
	for rows.Next() {
		var attachment publishSkillAttachment
		if err := rows.Scan(&attachment.skillID, &attachment.skillVersionID, &attachment.entityType); err != nil {
			t.Fatalf("scan skill attachment of version %d: %v", versionID, err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read skill attachments of version %d: %v", versionID, err)
	}
	return attachments
}

// seedPublishSkillFixture creates the agent, its draft version and two skills,
// and attaches them through the four-column shape the production writer uses
// (internal/api/v2/skillpublish/attach.go:136). The second attachment leaves
// skill_version_id NULL, which the column permits, so the copy is exercised on
// both shapes.
func seedPublishSkillFixture(t *testing.T, pool *pgxpool.Pool) publishSkillFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fixture publishSkillFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('skill fixture agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the thing', 'agent') RETURNING id`, fixture.appID).Scan(&fixture.versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}

	for _, seed := range []struct {
		name        string
		withVersion bool
	}{
		{name: "fixture skill one", withVersion: true},
		{name: "fixture skill two", withVersion: false},
	} {
		attachment := publishSkillAttachment{entityType: "agent"}
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skills (name, description, owner_id, author_id) VALUES ($1, 'seeded', 1, 1) RETURNING id`, seed.name).
			Scan(&attachment.skillID); err != nil {
			t.Fatalf("seed skill %q: %v", seed.name, err)
		}
		if seed.withVersion {
			var skillVersionID int
			if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, status)
VALUES ($1, 'base', 'follow the steps', 1, 'published') RETURNING id`, attachment.skillID).Scan(&skillVersionID); err != nil {
				t.Fatalf("seed skill version for %q: %v", seed.name, err)
			}
			attachment.skillVersionID = &skillVersionID
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
VALUES ($1, $2, $3, $4)`, fixture.versionID, attachment.entityType, attachment.skillID, attachment.skillVersionID); err != nil {
			t.Fatalf("seed skill attachment for %q: %v", seed.name, err)
		}
		fixture.attachments = append(fixture.attachments, attachment)
	}
	return fixture
}

// assertPublishSkillKeyColumns guards the point of the exercise. entity_type is
// NOT NULL with no default, so a copy that omitted it would raise 23502 rather
// than write a silently mis-keyed row. If the corpus stopped delivering that,
// these tests would pass against a schema that cannot reproduce the defect.
func assertPublishSkillKeyColumns(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var notNull bool
	if err := pool.QueryRow(ctx, `
SELECT attnotnull FROM pg_attribute
WHERE attrelid = 'p_1.entity_skill_mapping'::regclass AND attname = 'entity_type' AND NOT attisdropped`).
		Scan(&notNull); err != nil {
		t.Fatalf("read p_1.entity_skill_mapping.entity_type: %v", err)
	}
	if !notNull {
		t.Fatal("p_1.entity_skill_mapping.entity_type is not NOT NULL — the migration corpus no longer reproduces the defect")
	}
	// The table carries no entity_id. If a migration ever adds one, the copy in
	// Handler.Publish must set it, exactly as the tool copy beside it does.
	var hasEntityID bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM pg_attribute
WHERE attrelid = 'p_1.entity_skill_mapping'::regclass AND attname = 'entity_id' AND NOT attisdropped)`).
		Scan(&hasEntityID); err != nil {
		t.Fatalf("look for p_1.entity_skill_mapping.entity_id: %v", err)
	}
	if hasEntityID {
		t.Fatal("p_1.entity_skill_mapping now has an entity_id column — the publish copy must set it")
	}
}
