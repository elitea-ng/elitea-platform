package repos

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// Shared 0198 (ADR-0040 decision 2, issue #1492): publish records an
// immutable release, unpublish withdraws it, every draft save appends a
// revision, a run resolves its release, and rollback re-points the
// published release. CI-only: skipped without the integration database.

// releaseTestCatalogueProject is the catalogue project the start resolver
// is built with; the test database has no such schema.
const releaseTestCatalogueProject = 99

// seedAgentReleaseFixture is the continuation fixture (project 1, agent 31,
// draft version 41, conversation 1 with participant 21 on version 41) plus
// what a release pins: toolkit 51 attached to version 41 with a narrowed
// selection, and a non-private configuration its settings reference.
func seedAgentReleaseFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedCurrentAgentContinuationSchema(t, pool)
	if _, err := pool.Exec(t.Context(), `
ALTER TABLE p_1.applications
    ADD COLUMN updated_at TIMESTAMP,
    ADD COLUMN meta JSONB NOT NULL DEFAULT '{}'::jsonb;
UPDATE p_1.elitea_tools
SET settings = '{"selected_tools":["list_products","list_features"],"aha_configuration":{"elitea_title":"team-aha","private":false},"personal":{"elitea_title":"mine","private":true}}'::jsonb
WHERE id = 51;
INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (51, 31, 41, 'agent', '["list_products"]'::jsonb);
INSERT INTO p_1.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, author_id, created_at)
VALUES ('6b0f9e4c-2a1d-4c3b-9e8f-7a6b5c4d3e2f', 1, 'Aha', 'team-aha', 'aha', 'credentials',
        '{"token":"plain-secret"}'::jsonb, '{}'::jsonb, false, true, 'user', 11, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}
}

// releaseTestPublish copies a draft into a new published row with its
// toolkit and skill attachments, as Handler.Publish does, and records the
// release.
func releaseTestPublish(t *testing.T, pool *pgxpool.Pool, store *AgentReleases, sourceID, cloneID int64, name string) agentrelease.Release {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
SELECT $2, application_id, $3, 'published', author_id, gen_random_uuid(), llm_settings, instructions,
       conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at
FROM p_1.application_versions WHERE id = $1`, sourceID, cloneID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
SELECT tool_id, entity_id, $2, entity_type, selected_tools FROM p_1.entity_tool_mapping
WHERE entity_version_id = $1 ORDER BY id`, sourceID, cloneID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
SELECT $2, entity_type, skill_id, skill_version_id FROM p_1.entity_skill_mapping
WHERE entity_version_id = $1 ORDER BY id`, sourceID, cloneID); err != nil {
		t.Fatal(err)
	}
	release, err := store.RecordRelease(t.Context(), RecordReleaseRequest{
		ProjectID: 1, ApplicationID: 31, VersionID: cloneID, SourceVersionID: sourceID, Name: name, ActorID: 11,
	})
	if err != nil {
		t.Fatalf("record the release of version %d: %v", cloneID, err)
	}
	return release
}

// releaseTestUnpublish reverts a published row to draft and withdraws its
// release, as Handler.Unpublish does.
func releaseTestUnpublish(t *testing.T, pool *pgxpool.Pool, store *AgentReleases, versionID int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.application_versions SET status = 'draft', name = name || '-withdrawn' WHERE id = $1`, versionID); err != nil {
		t.Fatal(err)
	}
	if err := store.WithdrawVersions(t.Context(), 1, []int64{versionID}, 11); err != nil {
		t.Fatal(err)
	}
}

func releaseTestSave(t *testing.T, repo *ApplicationsRepo, versionID string, instructions string) {
	t.Helper()
	v := applications.Version{Instructions: instructions, EditorID: 11}
	v.Present.Instructions = true
	if _, err := repo.UpdateVersion(t.Context(), "1", "31", versionID, v); err != nil {
		t.Fatalf("save version %s: %v", versionID, err)
	}
}

func releaseTestPoint(t *testing.T, pool *pgxpool.Pool, versionID int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_participant_mapping
SET entity_settings = jsonb_set(entity_settings, '{version_id}', to_jsonb($1::int))
WHERE conversation_id = 1 AND participant_id = 21`, versionID); err != nil {
		t.Fatal(err)
	}
}

func releaseTestResolve(t *testing.T, resolver *CurrentAgentStartRepository, releaseID int64) (agentapp.CurrentApplicationTarget, error) {
	t.Helper()
	return resolver.ResolveCurrentApplication(t.Context(), agentapp.CurrentApplicationStartRequest{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ConversationUUID: "10000000-0000-4000-8000-000000000031",
		QuestionID:       uuid.NewString(), UserInput: "release run", ReleaseID: releaseID,
	})
}

func releaseTestInstructions(t *testing.T, details json.RawMessage) string {
	t.Helper()
	var document struct {
		Instructions string `json:"instructions"`
		Tools        []struct {
			Settings map[string]any `json:"settings"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(details, &document); err != nil {
		t.Fatalf("decode the run definition: %v", err)
	}
	if len(document.Tools) != 1 || document.Tools[0].Settings["available_tools"] == nil {
		t.Fatalf("the run definition lost the toolkit settings: %s", details)
	}
	return document.Instructions
}

func newReleaseTestStores(t *testing.T, pool *pgxpool.Pool) (*AgentReleases, *CurrentAgentStartRepository) {
	t.Helper()
	store, err := NewAgentReleases(pool)
	if err != nil {
		t.Fatal(err)
	}
	store.publicProjectID = func() int64 { return releaseTestCatalogueProject }
	resolver, err := NewCurrentAgentStartRepository(pool, releaseTestCatalogueProject)
	if err != nil {
		t.Fatal(err)
	}
	return store, resolver
}

// TestAgentReleases_UnpublishEditRepublishKeepsTheFirstReleaseRunnable is the
// acceptance of issue #1492: unpublish, edit and republish leave two
// releases and one revision per save, and the first release still resolves
// and runs its own definition by id.
func TestAgentReleases_UnpublishEditRepublishKeepsTheFirstReleaseRunnable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	store, resolver := newReleaseTestStores(t, pool)
	repo := NewApplicationsRepo(pool)

	// Two draft saves: a baseline plus two saves.
	releaseTestSave(t, repo, "41", "Be concise. v1")
	releaseTestSave(t, repo, "41", "Be concise. v1") // no change: no revision
	first := releaseTestPublish(t, pool, store, 41, 141, "v1")
	if !first.IsHead || first.State != agentrelease.StatePublished {
		t.Fatalf("the first release is not the published release: %+v", first)
	}
	if len(first.ToolkitPins) != 1 || first.ToolkitPins[0].ToolkitID != 51 {
		t.Fatalf("pins = %+v", first.ToolkitPins)
	}
	pin := first.ToolkitPins[0]
	if len(pin.Configurations) != 2 {
		t.Fatalf("configuration pins = %+v", pin.Configurations)
	}
	for _, configuration := range pin.Configurations {
		if configuration.Title == "team-aha" && (configuration.ConfigurationUUID == "" || configuration.DataSHA256 == "") {
			t.Fatalf("the shared configuration is not pinned: %+v", configuration)
		}
		if configuration.Private && configuration.ConfigurationUUID != "" {
			t.Fatalf("a private configuration was pinned to one row: %+v", configuration)
		}
	}
	// No secret is stored: neither the settings nor the configuration data.
	var stored string
	if err := pool.QueryRow(t.Context(), `
SELECT definition::text || toolkit_pins::text FROM elitea_lifecycle.application_releases WHERE id = $1`, first.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "plain-secret") || strings.Contains(stored, `"settings":`) || strings.Contains(stored, "list_features") {
		t.Fatalf("the release stored toolkit settings: %s", stored)
	}
	// Recording the same publish twice answers the same release.
	again, err := store.RecordRelease(t.Context(), RecordReleaseRequest{ProjectID: 1, ApplicationID: 31, VersionID: 141, Name: "v1"})
	if err != nil || again.ID != first.ID {
		t.Fatalf("a repeated record made release %d (%v), want %d", again.ID, err, first.ID)
	}
	// The release row is immutable.
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_lifecycle.application_releases SET definition = '{}'::jsonb WHERE id = $1`, first.ID); err == nil {
		t.Fatal("a release definition was edited in place")
	}
	// A published row is never saved in place.
	if _, err := repo.UpdateVersion(t.Context(), "1", "31", "141", applications.Version{Instructions: "edited"}); err == nil {
		t.Fatal("a published version was saved in place")
	}

	// Unpublish: the row goes back to draft and the release is withdrawn.
	releaseTestUnpublish(t, pool, store, 141)
	releaseTestSave(t, repo, "141", "Edited after the unpublish")
	releaseTestSave(t, repo, "41", "Be concise. v2")
	second := releaseTestPublish(t, pool, store, 41, 142, "v2")

	releases, err := store.ListReleases(t.Context(), 1, 31)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 2 || releases[0].ID != second.ID || releases[1].ID != first.ID {
		t.Fatalf("releases = %+v", releases)
	}
	if !releases[0].IsHead || releases[1].State != agentrelease.StateWithdrawn || releases[1].WithdrawnAt == nil || releases[1].IsHead {
		t.Fatalf("release states = %+v", releases)
	}
	revisions, err := store.ListRevisions(t.Context(), 1, 31, 41)
	if err != nil {
		t.Fatal(err)
	}
	// baseline + "v1" + "v2"; the unchanged save appended nothing.
	if len(revisions) != 3 || revisions[2].Kind != "baseline" || revisions[0].Revision != 3 || revisions[0].ActorID != 11 {
		t.Fatalf("revisions = %+v", revisions)
	}
	var diff []agentrelease.Change
	if err := json.Unmarshal(revisions[0].Diff, &diff); err != nil || len(diff) != 1 || diff[0].Path != "/instructions" {
		t.Fatalf("revision diff = %s (%v)", revisions[0].Diff, err)
	}
	detail, err := store.GetRelease(t.Context(), 1, 31, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.PreviousID != first.ID || len(detail.Changes) == 0 {
		t.Fatalf("release diff = %+v", detail)
	}

	// The first release runs by id: participant 21 is on the draft 41, the
	// row of release 1 (141) was edited, and the run executes release 1.
	target, err := releaseTestResolve(t, resolver, first.ID)
	if err != nil {
		t.Fatalf("run release %d by id: %v", first.ID, err)
	}
	if target.Release == nil || target.Release.ID != first.ID || target.Release.DefinitionDigest != first.DefinitionDigest {
		t.Fatalf("run release = %+v", target.Release)
	}
	if got := releaseTestInstructions(t, target.SourceVersionDetails); got != "Be concise. v1" {
		t.Fatalf("release 1 ran instructions %q", got)
	}
	digest, err := agentidentity.VersionDefinitionDigest(1, 31, 141, target.SourceVersionDetails)
	if err != nil || digest != first.DefinitionDigest {
		t.Fatal("the run definition is not the definition release 1 addresses")
	}
	// A draft runs no release.
	if target, err := releaseTestResolve(t, resolver, 0); err != nil || target.Release != nil {
		t.Fatalf("a draft run resolved release %+v (%v)", target.Release, err)
	}
	// A run of the published version records its release.
	releaseTestPoint(t, pool, 142)
	target, err = releaseTestResolve(t, resolver, 0)
	if err != nil || target.Release == nil || target.Release.ID != second.ID {
		t.Fatalf("the published version ran release %+v (%v)", target.Release, err)
	}
	if got := releaseTestInstructions(t, target.SourceVersionDetails); got != "Be concise. v2" {
		t.Fatalf("release 2 ran instructions %q", got)
	}
	// A release of another agent is not found.
	if _, err := releaseTestResolve(t, resolver, first.ID+1000); !errors.Is(err, agentrelease.ErrNotFound) {
		t.Fatalf("an unknown release: %v", err)
	}
}

// TestAgentReleases_RollbackAndDrift: a rollback re-points the published
// release, a run of a later version executes the earlier release, and a
// toolkit or configuration edited after a release refuses an explicit run
// of it with the typed drift error.
func TestAgentReleases_RollbackAndDrift(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	store, resolver := newReleaseTestStores(t, pool)
	repo := NewApplicationsRepo(pool)

	releaseTestSave(t, repo, "41", "Release A")
	a := releaseTestPublish(t, pool, store, 41, 141, "a")
	releaseTestSave(t, repo, "41", "Release B")
	b := releaseTestPublish(t, pool, store, 41, 142, "b")

	if _, err := store.Rollback(t.Context(), 1, 31, b.ID, 11, ""); !errors.Is(err, agentrelease.ErrAlreadyHead) {
		t.Fatalf("rollback to the published release: %v", err)
	}
	if _, err := store.Rollback(t.Context(), 1, 32, a.ID, 11, ""); !errors.Is(err, agentrelease.ErrNotPublished) {
		t.Fatalf("rollback of an agent with no release: %v", err)
	}
	outcome, err := store.Rollback(t.Context(), 1, 31, a.ID, 11, "B answers wrong")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.From.ID != b.ID || outcome.To.ID != a.ID || !outcome.To.IsHead {
		t.Fatalf("rollback outcome = %+v", outcome)
	}
	var cause, reason string
	if err := pool.QueryRow(t.Context(), `
SELECT cause, reason FROM elitea_lifecycle.application_release_heads WHERE tenant_project_id = 1 AND application_id = 31`).Scan(&cause, &reason); err != nil {
		t.Fatal(err)
	}
	if cause != "rollback" || reason != "B answers wrong" {
		t.Fatalf("head = %s %q", cause, reason)
	}
	// A participant on B's version now runs A.
	releaseTestPoint(t, pool, 142)
	target, err := releaseTestResolve(t, resolver, 0)
	if err != nil || target.Release == nil || target.Release.ID != a.ID {
		t.Fatalf("after the rollback version 142 ran %+v (%v)", target.Release, err)
	}
	if got := releaseTestInstructions(t, target.SourceVersionDetails); got != "Release A" {
		t.Fatalf("after the rollback the run executed %q", got)
	}

	// The shared configuration changes: every release pinned it.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.configuration SET data = '{"token":"rotated"}'::jsonb WHERE elitea_title = 'team-aha'`); err != nil {
		t.Fatal(err)
	}
	_, err = releaseTestResolve(t, resolver, b.ID)
	var drift *agentrelease.DriftError
	if !errors.As(err, &drift) || drift.Reason != agentrelease.DriftConfigurationChanged || drift.ToolkitID != 51 {
		t.Fatalf("a run of a release whose configuration changed: %v", err)
	}
	// The rolled-back published release is drifted too: a run on 142 is
	// refused rather than run with what A did not pin.
	if _, err := releaseTestResolve(t, resolver, 0); !errors.As(err, &drift) {
		t.Fatalf("a run of the drifted published release: %v", err)
	}
	// A rollback to a drifted release is refused, and the head stays.
	if _, err := store.Rollback(t.Context(), 1, 31, b.ID, 11, ""); !errors.As(err, &drift) {
		t.Fatalf("rollback to a drifted release: %v", err)
	}

	// A toolkit settings edit is a drift of its own reason.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.configuration SET data = '{"token":"plain-secret"}'::jsonb WHERE elitea_title = 'team-aha';
UPDATE p_1.elitea_tools SET settings = settings || '{"base_url":"https://other.example"}'::jsonb WHERE id = 51;`); err != nil {
		t.Fatal(err)
	}
	if _, err := releaseTestResolve(t, resolver, a.ID); !errors.As(err, &drift) || drift.Reason != agentrelease.DriftToolkitSettingsChanged {
		t.Fatalf("a run of a release whose toolkit changed: %v", err)
	}
	// The current published version whose toolkit was edited in place runs
	// as it stands, with no release: head is A, a new publish puts B's
	// successor at the head.
	releaseTestSave(t, repo, "41", "Release C")
	c := releaseTestPublish(t, pool, store, 41, 143, "c")
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.elitea_tools SET settings = settings || '{"base_url":"https://third.example"}'::jsonb WHERE id = 51`); err != nil {
		t.Fatal(err)
	}
	releaseTestPoint(t, pool, 143)
	target, err = releaseTestResolve(t, resolver, 0)
	if err != nil || target.Release != nil {
		t.Fatalf("the edited published version ran release %+v (%v), want none (release %d drifted)", target.Release, err, c.ID)
	}

	// Unpublishing the head's version moves the head to the newest
	// published release; a withdrawn release cannot be rolled back to.
	releaseTestUnpublish(t, pool, store, 143)
	releases, err := store.ListReleases(t.Context(), 1, 31)
	if err != nil {
		t.Fatal(err)
	}
	if releases[0].ID != c.ID || releases[0].State != agentrelease.StateWithdrawn || !releases[1].IsHead || releases[1].ID != b.ID {
		t.Fatalf("after the unpublish: %+v", releases)
	}
	if _, err := store.Rollback(t.Context(), 1, 31, c.ID, 11, ""); !errors.Is(err, agentrelease.ErrWithdrawn) {
		t.Fatalf("rollback to a withdrawn release: %v", err)
	}
}

// TestAgentReleases_AdmissionRecordsTheRelease: the admission writes the
// release id of a release run on its job row.
func TestAgentReleases_AdmissionRecordsTheRelease(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 8
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	for index, releaseID := range []int64{0, 77} {
		admission := postgresAgentCapacityAdmission(890 + index)
		admission.ReleaseID = releaseID
		outcome, err := repository.AdmitAgentExecution(ctx, admission)
		if err != nil || !outcome.Created {
			t.Fatalf("admit with release %d: outcome=%+v err=%v", releaseID, outcome, err)
		}
		var stored *int64
		if err := pool.QueryRow(ctx,
			`SELECT release_id FROM elitea_runtime.agent_execution_jobs WHERE execution_id = $1`,
			outcome.ExecutionID).Scan(&stored); err != nil {
			t.Fatalf("read release_id: %v", err)
		}
		switch {
		case releaseID == 0 && stored != nil:
			t.Fatalf("a run of no release recorded release %d", *stored)
		case releaseID > 0 && (stored == nil || *stored != releaseID):
			t.Fatalf("release run recorded %v, want %d", stored, releaseID)
		}
	}
}
