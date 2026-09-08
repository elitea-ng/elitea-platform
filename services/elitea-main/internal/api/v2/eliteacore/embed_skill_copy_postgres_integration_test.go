package eliteacore_test

// Real-PostgreSQL coverage for the skill-mapping copy in the EMBED path
// (handler.go, Handler.embedSubAgentsRecursive) — issue #406.
//
// The embed copied entity_tool_mapping and nothing else, so an embedded
// sub-agent ran without the skills its author gave it. This is #351 one level
// down: the parent agent now carries its own skills, and the sub-agent embedded
// into it did not.
//
// These tests never assert on the status code. A 200 from Publish is exactly
// what the defect produced, and the embed keeps answering 200 after this change
// (see TestEmbedKeepsTheStatusWhenTheSkillCopyFails). Every assertion reads the
// mapping rows back out of the database, through the predicate the chat read
// uses (internal/db/queries/agent_chat.sql:126-132): entity_version_id AND
// entity_type, with skill_versions LEFT JOINed for the instructions. There is no
// `entity_id` column on this table (001_initial.sql:422-432) — those two columns
// are the whole key that makes a copied row visible to the reader.
//
// The schema comes from the REAL migration corpus, through the
// newPublishCopyPool helper of publish_tool_copy_postgres_integration_test.go. A
// hand-built fixture that declares entity_type nullable is how this class of
// defect survives.
//
// application_tools is the one exception, and it cannot come from the corpus:
// no migration in this repository creates it. It is a pylon-owned table, which
// internal/infra/db/repos/applications.go:381-397 probes with to_regclass for
// exactly that reason. The embed path reads it, so the test creates it in the
// shape captured from a live legacy database in
// testdata/postgres/legacy-centry-catalog.json.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// embedSkillAttachment is one row of p_1.entity_skill_mapping, read back through
// the chat read's own projection.
type embedSkillAttachment struct {
	skillID int
	// skillVersionID is nullable in the table, so it is held as a pointer. An
	// attachment whose version is dropped renders with empty instructions.
	skillVersionID *int
	name           string
	// instructions is COALESCE(skill_version.instructions, ''), which is what the
	// chat read hands the model. An empty string here is a skill in name only.
	instructions string
}

// embedSkillFixture is a parent agent in p_1 whose draft version references one
// sub-agent, and that sub-agent with two skills attached to its own version.
type embedSkillFixture struct {
	parentAppID     int
	parentVersionID int
	subAppID        int
	subVersionID    int
	attachments     []embedSkillAttachment
}

// TestEmbedCarriesSubAgentSkillAttachments is the acceptance test #406 asks for:
// embed a sub-agent that HAS skills attached, then prove the copied rows exist
// on the embedded version, carry the key the reader matches on, and resolve to
// real instructions rather than an empty body.
func TestEmbedCarriesSubAgentSkillAttachments(t *testing.T) {
	pool := newPublishCopyPool(t)
	assertEmbedSkillKeyColumns(t, pool)
	createLegacyApplicationTools(t, pool)
	fixture := seedEmbedSkillFixture(t, pool)
	router := publishCopyRouter(eliteacore.NewHandler(pool))

	recorder := publishCopyDo(t, router, fixture.parentVersionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.parentVersionID),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	embeddedVersionID := embeddedVersionIDOf(t, ctx, pool, fixture.subVersionID)

	// Read the copied rows through the chat read's own predicate and projection.
	// A row that carries the wrong entity_type is invisible to that read, so
	// selecting on it proves the column and the presence of the row in one
	// statement.
	copied := readEmbedSkillAttachments(t, ctx, pool, embeddedVersionID)
	if len(copied) != len(fixture.attachments) {
		t.Fatalf("embedded sub-agent carries %d skill attachments, want %d", len(copied), len(fixture.attachments))
	}
	for index, want := range fixture.attachments {
		got := copied[index]
		if got.skillID != want.skillID {
			t.Errorf("copied attachment %d: skill_id = %d, want %d", index, got.skillID, want.skillID)
		}
		if got.name != want.name {
			t.Errorf("copied attachment %d: name = %q, want %q", index, got.name, want.name)
		}
		// The chat read LEFT JOINs skill_version_id for the skill instructions.
		// Dropping it embeds a named skill with an empty body.
		switch {
		case want.skillVersionID == nil && got.skillVersionID != nil:
			t.Errorf("copied attachment %d: skill_version_id = %d, want NULL", index, *got.skillVersionID)
		case want.skillVersionID != nil && got.skillVersionID == nil:
			t.Errorf("copied attachment %d: skill_version_id = NULL, want %d", index, *want.skillVersionID)
		case want.skillVersionID != nil && *got.skillVersionID != *want.skillVersionID:
			t.Errorf("copied attachment %d: skill_version_id = %d, want %d", index, *got.skillVersionID, *want.skillVersionID)
		}
		// The point of carrying skill_version_id. A copy that dropped the column
		// would still return a named skill here, with instructions "".
		if got.instructions != want.instructions {
			t.Errorf("copied attachment %d (%q): instructions = %q, want %q",
				index, want.name, got.instructions, want.instructions)
		}
	}

	// The source sub-agent version must keep its own attachments. A copy that
	// moved them would satisfy every assertion above and still break the draft.
	if source := readEmbedSkillAttachments(t, ctx, pool, fixture.subVersionID); len(source) != len(fixture.attachments) {
		t.Errorf("source sub-agent carries %d skill attachments after the embed, want %d",
			len(source), len(fixture.attachments))
	}
}

// TestEmbedKeepsTheStatusWhenTheSkillCopyFails pins the decision this issue
// turns on, and states what a user sees when the copy fails.
//
// The embed runs after the publish transaction has committed (handler.go, the
// Commit before the embedSubAgents call). The publish cannot be withdrawn, so
// the caller keeps its 200 and its published agent, and the embedded sub-agent
// is there but carries no skills. What changes is that the failure is no longer
// discarded: it is reported on the service log, which is the only channel left
// once the transaction is durable. This test holds both halves of that
// statement, so neither can drift into a comment nobody checks.
func TestEmbedKeepsTheStatusWhenTheSkillCopyFails(t *testing.T) {
	pool := newPublishCopyPool(t)
	createLegacyApplicationTools(t, pool)
	fixture := seedEmbedSkillFixture(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Make the skill copy the statement that fails, and leave the application and
	// version clones before it untouched. The seeded rows satisfy this
	// constraint; every copied row carries a higher entity_version_id and cannot.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
ALTER TABLE p_1.entity_skill_mapping ADD CONSTRAINT embed_copy_must_fail CHECK (entity_version_id < %d)`,
		fixture.subVersionID+1)); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	log := captureSlog(t)
	router := publishCopyRouter(eliteacore.NewHandler(pool))
	recorder := publishCopyDo(t, router, fixture.parentVersionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.parentVersionID),
	})
	// The publish is durable before the embed starts. Reporting a failure here
	// would tell the caller to retry a publish that already succeeded.
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, want %d — the publish had already committed: %s",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}

	// The sub-agent is embedded and linked, and holds no skills. That is the
	// degraded state the log has to describe.
	embeddedVersionID := embeddedVersionIDOf(t, ctx, pool, fixture.subVersionID)
	if copied := readEmbedSkillAttachments(t, ctx, pool, embeddedVersionID); len(copied) != 0 {
		t.Fatalf("the failing constraint let %d skill attachments through", len(copied))
	}

	if entry := log.String(); !strings.Contains(entry, "embed_sub_agents: skill attachment copy failed") {
		t.Errorf("the failed skill copy was not reported on the log; log was:\n%s", entry)
	} else if !strings.Contains(entry, strconv.Itoa(embeddedVersionID)) {
		t.Errorf("the report names no embedded version, so an operator cannot find the sub-agent; log was:\n%s", entry)
	}

	// The source sub-agent version must still hold its own attachments.
	if source := readEmbedSkillAttachments(t, ctx, pool, fixture.subVersionID); len(source) != len(fixture.attachments) {
		t.Errorf("the failed copy left the source with %d skill attachments, want the untouched %d",
			len(source), len(fixture.attachments))
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// embeddedVersionIDOf finds the embedded clone of one source version, by the
// meta the embed writes onto it.
func embeddedVersionIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sourceVersionID int) int {
	t.Helper()
	var embeddedVersionID int
	if err := pool.QueryRow(ctx, `
SELECT id FROM p_1.application_versions
WHERE status = 'embedded' AND meta ->> 'source_version_id' = $1`,
		strconv.Itoa(sourceVersionID)).Scan(&embeddedVersionID); err != nil {
		t.Fatalf("the publish embedded no copy of sub-agent version %d: %v", sourceVersionID, err)
	}
	return embeddedVersionID
}

// readEmbedSkillAttachments selects the attachments of one version through the
// joins, predicate and ordering of the chat read
// (internal/db/queries/agent_chat.sql:126-132), including its
// COALESCE(skill_version.instructions, ”) projection.
func readEmbedSkillAttachments(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionID int) []embedSkillAttachment {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT skill_mapping.skill_id,
       skill_mapping.skill_version_id,
       skill.name,
       COALESCE(skill_version.instructions, '')
FROM p_1.entity_skill_mapping AS skill_mapping
JOIN p_1.skills AS skill
  ON skill.id = skill_mapping.skill_id
LEFT JOIN p_1.skill_versions AS skill_version
  ON skill_version.id = skill_mapping.skill_version_id
WHERE skill_mapping.entity_version_id = $1
  AND skill_mapping.entity_type = 'agent'
ORDER BY skill_mapping.id`, versionID)
	if err != nil {
		t.Fatalf("read skill attachments of version %d: %v", versionID, err)
	}
	defer rows.Close()
	var attachments []embedSkillAttachment
	for rows.Next() {
		var attachment embedSkillAttachment
		if err := rows.Scan(&attachment.skillID, &attachment.skillVersionID,
			&attachment.name, &attachment.instructions); err != nil {
			t.Fatalf("scan skill attachment of version %d: %v", versionID, err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read skill attachments of version %d: %v", versionID, err)
	}
	return attachments
}

// seedEmbedSkillFixture creates the sub-agent with two skills attached, and the
// parent agent whose draft version references it through an application_tools
// row of type 'application'. The attachments use the four-column shape the
// production writer uses (internal/api/v2/skillpublish/attach.go). The second
// attachment leaves skill_version_id NULL, which the column permits, so the copy
// is exercised on both shapes and the instructions assertion discriminates.
func seedEmbedSkillFixture(t *testing.T, pool *pgxpool.Pool) embedSkillFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fixture embedSkillFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('embed fixture sub-agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.subAppID); err != nil {
		t.Fatalf("seed sub-agent application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the sub thing', 'agent') RETURNING id`, fixture.subAppID).
		Scan(&fixture.subVersionID); err != nil {
		t.Fatalf("seed sub-agent version: %v", err)
	}

	for _, seed := range []struct {
		name         string
		instructions string
	}{
		{name: "embed fixture skill one", instructions: "follow the steps"},
		{name: "embed fixture skill two"},
	} {
		attachment := embedSkillAttachment{name: seed.name}
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skills (name, description, owner_id, author_id) VALUES ($1, 'seeded', 1, 1) RETURNING id`, seed.name).
			Scan(&attachment.skillID); err != nil {
			t.Fatalf("seed skill %q: %v", seed.name, err)
		}
		if seed.instructions != "" {
			var skillVersionID int
			if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, status)
VALUES ($1, 'base', $2, 1, 'published') RETURNING id`, attachment.skillID, seed.instructions).
				Scan(&skillVersionID); err != nil {
				t.Fatalf("seed skill version for %q: %v", seed.name, err)
			}
			attachment.skillVersionID = &skillVersionID
			attachment.instructions = seed.instructions
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
VALUES ($1, 'agent', $2, $3)`, fixture.subVersionID, attachment.skillID, attachment.skillVersionID); err != nil {
			t.Fatalf("seed skill attachment for %q: %v", seed.name, err)
		}
		fixture.attachments = append(fixture.attachments, attachment)
	}

	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('embed fixture parent agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.parentAppID); err != nil {
		t.Fatalf("seed parent application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'delegate the thing', 'agent') RETURNING id`, fixture.parentAppID).
		Scan(&fixture.parentVersionID); err != nil {
		t.Fatalf("seed parent version: %v", err)
	}
	// The two keys the embed reads out of settings (handler.go, the settings
	// decode in embedSubAgentsRecursive): application_id and version_id.
	settings, err := json.Marshal(map[string]any{
		"application_id": strconv.Itoa(fixture.subAppID),
		"version_id":     strconv.Itoa(fixture.subVersionID),
	})
	if err != nil {
		t.Fatalf("marshal sub-agent tool settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_tools (application_version_id, name, type, settings)
VALUES ($1, 'embed fixture sub-agent', 'application', $2)`, fixture.parentVersionID, settings); err != nil {
		t.Fatalf("seed sub-agent reference: %v", err)
	}
	return fixture
}

// createLegacyApplicationTools builds the one table the embed path needs and the
// migration corpus does not own. No migration in this repository creates
// application_tools; internal/infra/db/repos/applications.go:381-397 probes for
// it with to_regclass for that reason. The columns, types and nullability below
// are the live legacy shape recorded in
// testdata/postgres/legacy-centry-catalog.json, schema p_1. A looser stub would
// let the embed write rows that pylon would reject.
func createLegacyApplicationTools(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE TABLE p_1.application_tools (
    id SERIAL PRIMARY KEY,
    application_version_id INTEGER NOT NULL REFERENCES p_1.application_versions(id),
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    type VARCHAR NOT NULL,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(1024),
    settings JSONB NOT NULL
)`); err != nil {
		t.Fatalf("create the legacy application_tools table: %v", err)
	}
}

// assertEmbedSkillKeyColumns guards the point of the exercise. entity_type is
// NOT NULL with no default, so a copy that omitted it would raise 23502 rather
// than write a silently mis-keyed row. If the corpus stopped delivering that,
// these tests would pass against a schema that cannot reproduce the defect.
func assertEmbedSkillKeyColumns(t *testing.T, pool *pgxpool.Pool) {
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
	// The table carries no entity_id. If a migration ever adds one, the embed
	// copy must set it, exactly as the tool copy beside it does.
	var hasEntityID bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM pg_attribute
WHERE attrelid = 'p_1.entity_skill_mapping'::regclass AND attname = 'entity_id' AND NOT attisdropped)`).
		Scan(&hasEntityID); err != nil {
		t.Fatalf("look for p_1.entity_skill_mapping.entity_id: %v", err)
	}
	if hasEntityID {
		t.Fatal("p_1.entity_skill_mapping now has an entity_id column — the embed copy must set it")
	}
}

// captureSlog redirects the default logger into a buffer for one test, and puts
// the previous logger back afterwards.
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}

// lockedBuffer is a bytes.Buffer that is safe to write and read from different
// goroutines, so the race detector has nothing to say about the capture.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}
