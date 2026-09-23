package repos

// THE CONVERSATION SAYS ITS AGENT WAS WITHDRAWN (#972).
//
// The chat surface used to derive this on the client, from the agent's version
// LIST: a withdrawal reverts the published clone to a draft and renames it with
// a `-withdrawn-` marker, so `status != 'published' && name contains the
// marker` is a correct reading of what the server writes — and it is
// unreachable on the chat page, because the version list arrives only once a
// participant has been SELECTED, and opening a conversation selects nothing.
// For a participant bound into the public project it never arrives at all.
//
// So the conversation READ answers it, on the participant itself. These tests
// are against a REAL migrated database because the whole claim is about a join
// the client cannot perform: `meta.version_withdrawn` has to agree with what
// `api/v2/eliteacore.Unpublish` actually wrote to `application_versions`.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ensureApplicationVersionPublicationColumns completes the package's minimal
// `p_1.application_versions` fixture with the three columns this enrichment
// reads.
//
// STATED RATHER THAN SKIPPED. The table is declared minimally
// (`configuration_validation_postgres_integration_test.go`: id + llm_settings)
// because the migration corpus does not create it at all — in a migrated
// deployment it is pylon-owned — and each suite that needs more of it adds what
// it needs (`seedCurrentAgentContinuationSchema` adds the full 001_initial
// shape for the continuation cases). A `t.Skip` on the missing columns would
// turn the three assertions below into silent no-ops, which is the failure mode
// this repository keeps paying for.
//
// `IF NOT EXISTS`, and no column this suite does not read: a test that also ran
// one of those fuller seeds must find the table already correct rather than
// collide with it.
func ensureApplicationVersionPublicationColumns(t *testing.T, repo *ConversationsRepo) {
	t.Helper()
	if _, err := repo.pool.Exec(context.Background(), `
ALTER TABLE p_1.application_versions
    ADD COLUMN IF NOT EXISTS name VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS status VARCHAR NOT NULL DEFAULT 'draft',
    ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}'::jsonb`); err != nil {
		t.Fatalf("prepare the application_versions publication columns: %v", err)
	}
}

// seedApplicationVersion inserts one `application_versions` row and answers its
// id, so a participant can be bound to it the way the attach path binds one.
func seedApplicationVersion(t *testing.T, repo *ConversationsRepo, name, status, meta string) int64 {
	t.Helper()
	ensureApplicationVersionPublicationColumns(t, repo)
	var versionID int64
	if err := repo.pool.QueryRow(context.Background(), `
INSERT INTO p_1.application_versions (name, status, meta)
VALUES ($1, $2, $3::jsonb)
RETURNING id`, name, status, meta).Scan(&versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(),
			`DELETE FROM p_1.application_versions WHERE id = $1`, versionID)
	})
	return versionID
}

// participantPublication reads back the two enrichment keys for the single
// agent participant of a conversation.
func participantPublication(t *testing.T, repo *ConversationsRepo, conversationID string) map[string]any {
	t.Helper()
	items, err := repo.ListParticipants(context.Background(), "1", conversationID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	for _, item := range items {
		if item.EntityName == participantEntityApplication {
			return item.Meta
		}
	}
	t.Fatalf("the conversation has no agent participant: %+v", items)
	return nil
}

func seedAgentParticipantAt(t *testing.T, repo *ConversationsRepo, versionID int64) string {
	t.Helper()
	conversationID := seedConversation(t, repo)
	if err := repo.AddParticipant(context.Background(), "1", conversationID, map[string]any{
		"entity_name":     "application",
		"entity_meta":     map[string]any{"id": 4242, "project_id": 1, "name": "Withdrawn fixture"},
		"entity_settings": map[string]any{"version_id": versionID},
	}); err != nil {
		t.Fatalf("attach agent participant: %v", err)
	}
	return conversationID
}

// A version the author withdrew: reverted to draft AND carrying the marker the
// rename writes. Both halves are required, which the next test pins.
func TestListParticipantsReportsAWithdrawnAgentVersion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)

	versionID := seedApplicationVersion(t, repo,
		"rel420001"+withdrawnVersionNameMarker+"1", "draft", `{"withdrawn_from_name":"rel420001"}`)
	meta := participantPublication(t, repo, seedAgentParticipantAt(t, repo, versionID))

	if meta["version_withdrawn"] != true {
		t.Fatalf("a withdrawn version must be reported as withdrawn: %+v", meta)
	}
	if meta["version_status"] != "draft" {
		t.Fatalf("the raw status must travel beside the flag: %+v", meta)
	}
}

// THE HALF THAT KEEPS THE NOTICE OFF EVERY CONVERSATION IN THE PRODUCT.
//
// An ordinary private agent's version is a draft too. A signal that read
// `status != 'published'` alone would put "this agent has been unpublished" on
// essentially every conversation with a private agent — which is worse than the
// silence #972 is about, because it is confidently wrong.
func TestListParticipantsDoesNotCallAnOrdinaryDraftWithdrawn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)

	versionID := seedApplicationVersion(t, repo, "base", "draft", `{}`)
	meta := participantPublication(t, repo, seedAgentParticipantAt(t, repo, versionID))

	if meta["version_withdrawn"] != false {
		t.Fatalf("an ordinary draft is not a withdrawal: %+v", meta)
	}
	if meta["version_status"] != "draft" {
		t.Fatalf("the raw status must travel beside the flag: %+v", meta)
	}
}

// A version still in the catalogue is resolved and NOT withdrawn — the key is
// present and false, which is what lets the client tell this apart from "not
// resolved".
func TestListParticipantsReportsAPublishedAgentVersionAsLive(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)

	versionID := seedApplicationVersion(t, repo, "rel420002", "published", `{}`)
	meta := participantPublication(t, repo, seedAgentParticipantAt(t, repo, versionID))

	if meta["version_withdrawn"] != false {
		t.Fatalf("a published version is not withdrawn: %+v", meta)
	}
	if meta["version_status"] != "published" {
		t.Fatalf("version_status = %v, want published", meta["version_status"])
	}
}

// A version that is GONE is a DIFFERENT state with its own reader
// (`isActiveParticipantVersionMissing`). Announcing `version_withdrawn: false`
// for it would say "resolved, still published" about a version that does not
// exist; announcing `true` would put the wrong sentence on screen. The honest
// answer is to say nothing, so the keys must be ABSENT.
func TestListParticipantsSaysNothingAboutAVersionThatIsGone(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)

	meta := participantPublication(t, repo, seedAgentParticipantAt(t, repo, 987_654_321))

	if _, present := meta["version_withdrawn"]; present {
		t.Fatalf("a version that does not exist must leave the key absent: %+v", meta)
	}
	if _, present := meta["version_status"]; present {
		t.Fatalf("a version that does not exist must leave the key absent: %+v", meta)
	}
}

// The marker this package matches on is the one the WITHDRAWAL writes.
//
// `withdrawnVersionNameMarker` is a copy: the writer's own constant
// (`api/v2/eliteacore.withdrawnNameMarker`) is unexported in a package this one
// must not import. A copy that silently drifts is how a correct-looking
// enrichment stops matching anything, so the two are pinned by reading the
// writer's source. This is not an integration test and needs no database.
func TestWithdrawnVersionNameMarkerMatchesTheWriter(t *testing.T) {
	const writer = "../../../api/v2/eliteacore/unpublish_name_release.go"
	source, err := os.ReadFile(writer)
	if err != nil {
		t.Fatalf("read %s: %v", writer, err)
	}
	want := `const withdrawnNameMarker = "` + withdrawnVersionNameMarker + `"`
	if !strings.Contains(string(source), want) {
		t.Fatalf("%s no longer declares %s — this package's copy of the marker has drifted", writer, want)
	}
}
