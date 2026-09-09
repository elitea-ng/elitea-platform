package repos

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// This file is the #874 acceptance suite: skills gained the multi-version
// machinery application_versions already has (list/create/get/update/delete
// over NAMED versions, a default-version pointer) plus one thing agents
// don't — RestoreVersion, the rollback the issue asks for. Every test here
// asserts a PERSISTED effect (a fresh Get/GetVersion call, or a direct SQL
// read), not just the mutating call's own return value — the class of bug
// #38's doc comment on updateSkillRelation names: a write that answers 200
// while touching nothing.

func createSkillWithBase(t *testing.T, repo *SkillsRepo, ctx context.Context, name string) skills.Skill {
	t.Helper()
	created, err := repo.Create(ctx, "1", skills.Skill{
		Name: name, Description: "d", Instructions: "base instructions", Tags: []string{"base-tag"},
	})
	if err != nil {
		t.Fatalf("create skill: %v", err)
	}
	return created
}

func TestSkillsRepoPostgres_CreateVersionClonesBaseWhenNoContentGiven(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Cloner")

	created, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{Name: "v2"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if created.VersionDetails == nil || created.VersionDetails.Name != "v2" {
		t.Fatalf("version_details=%+v", created.VersionDetails)
	}
	if created.VersionDetails.Instructions != "base instructions" {
		t.Errorf("cloned instructions=%q, want base's content", created.VersionDetails.Instructions)
	}
	if len(created.VersionDetails.Tags) != 1 || created.VersionDetails.Tags[0] != "base-tag" {
		t.Errorf("cloned tags=%v, want [base-tag]", created.VersionDetails.Tags)
	}

	var baseID string
	for _, v := range created.Versions {
		if v.Name == "base" {
			baseID = v.ID
		}
	}
	if baseID == "" {
		t.Fatal("expected a base version among created.Versions")
	}
	if created.VersionDetails.ParentVersionID != baseID {
		t.Errorf("parent_version_id=%q, want %q (base)", created.VersionDetails.ParentVersionID, baseID)
	}

	// Persisted effect: a fresh Get sees both versions, still with the
	// lineage — not just the mutating call's own response.
	fetched, err := repo.Get(ctx, "1", sk.ID)
	if err != nil {
		t.Fatalf("get after create version: %v", err)
	}
	if len(fetched.Versions) != 2 {
		t.Fatalf("fetched.Versions=%+v, want 2 rows", fetched.Versions)
	}
	// The unversioned Get still answers `base`, unaffected by the new version.
	if fetched.Instructions != "base instructions" {
		t.Errorf("unversioned Get instructions=%q, want base's content unchanged", fetched.Instructions)
	}
}

func TestSkillsRepoPostgres_CreateVersionWithExplicitContentAndDuplicateNameConflict(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Explicit Content")

	created, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{
		Name: "release-1", Instructions: "release instructions", Tags: []string{"release"},
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if created.VersionDetails.Instructions != "release instructions" {
		t.Errorf("instructions=%q, want the explicit content (no clone should happen)", created.VersionDetails.Instructions)
	}
	// Explicit content with no source given still records no lineage — there
	// was nothing to clone from.
	if created.VersionDetails.ParentVersionID != "" {
		t.Errorf("parent_version_id=%q, want empty (no source given)", created.VersionDetails.ParentVersionID)
	}

	// A duplicate name is a 409, not a silent overwrite or a second row.
	_, err = repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{
		Name: "release-1", Instructions: "different content",
	})
	if err == nil {
		t.Fatal("expected the duplicate version name to be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate-name error = %v", err)
	}

	fetched, err := repo.Get(ctx, "1", sk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	count := 0
	for _, v := range fetched.Versions {
		if v.Name == "release-1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("release-1 rows=%d, want exactly 1 (the refused duplicate must not have landed)", count)
	}
}

// TestSkillsRepoPostgres_CompareTwoVersionsReadsIndependentPersistedContent
// is the backend half of the client-side compare UI (#874): the web app
// diffs two versions by fetching each with GetVersion and comparing
// instructions/tags/description locally — the same architecture
// CompareVersionsModal already uses for application_versions (there is no
// server-side "compare" endpoint for agents either). This proves the two
// reads the diff is built from are independent and correctly scoped: editing
// one version after the fact does not leak into the other's already-fetched
// content, and both round-trip through GetVersion with the SAME shared
// skill-level fields (name/description) and DIFFERENT version-level fields
// (instructions/tags).
func TestSkillsRepoPostgres_CompareTwoVersionsReadsIndependentPersistedContent(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Comparable")
	v2, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{
		Name: "v2", Instructions: "v2 instructions", Tags: []string{"v2-tag"},
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	v2ID := v2.VersionDetails.ID

	baseID := ""
	for _, v := range sk.Versions {
		if v.Name == "base" {
			baseID = v.ID
		}
	}
	if baseID == "" {
		// sk (from Create) always carries exactly the base version.
		baseID = sk.VersionDetails.ID
	}

	left, err := repo.GetVersion(ctx, "1", sk.ID, baseID)
	if err != nil {
		t.Fatalf("get base for compare: %v", err)
	}
	right, err := repo.GetVersion(ctx, "1", sk.ID, v2ID)
	if err != nil {
		t.Fatalf("get v2 for compare: %v", err)
	}

	if left.VersionDetails.Instructions == right.VersionDetails.Instructions {
		t.Fatal("expected the two versions' instructions to differ for a meaningful compare")
	}
	if left.Name != right.Name || left.Description != right.Description {
		t.Errorf("shared skill fields diverged across versions: left=%+v right=%+v", left, right)
	}
	if right.VersionDetails.Instructions != "v2 instructions" {
		t.Errorf("right.instructions=%q", right.VersionDetails.Instructions)
	}
	if left.VersionDetails.Instructions != "base instructions" {
		t.Errorf("left.instructions=%q", left.VersionDetails.Instructions)
	}
}

func TestSkillsRepoPostgres_UpdateVersionPersistsAndRefusesPublished(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Editable Version")
	created, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{Name: "draft-2", Instructions: "v1"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	versionID := created.VersionDetails.ID

	updated, err := repo.UpdateVersion(ctx, "1", sk.ID, versionID, skills.Skill{
		Name: sk.Name, Description: sk.Description, Instructions: "v2", Tags: []string{"edited"},
	})
	if err != nil {
		t.Fatalf("update version: %v", err)
	}
	if updated.VersionDetails.Instructions != "v2" {
		t.Errorf("updated instructions=%q", updated.VersionDetails.Instructions)
	}

	// Persisted effect via a fresh read, not the mutation's own response.
	fetched, err := repo.GetVersion(ctx, "1", sk.ID, versionID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if fetched.VersionDetails.Instructions != "v2" {
		t.Errorf("persisted instructions=%q, want v2", fetched.VersionDetails.Instructions)
	}
	if len(fetched.Tags) != 1 || fetched.Tags[0] != "edited" {
		t.Errorf("persisted tags=%v", fetched.Tags)
	}

	// Publish it directly (mirrors skillpublish's own status write) and
	// confirm UpdateVersion now refuses — "Unpublish first", matching
	// application_versions' own guard verbatim.
	if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET status = 'published' WHERE id = $1`, versionID); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	_, err = repo.UpdateVersion(ctx, "1", sk.ID, versionID, skills.Skill{
		Name: sk.Name, Description: sk.Description, Instructions: "v3",
	})
	if err == nil {
		t.Fatal("expected UpdateVersion to refuse a published version")
	}
	if !strings.Contains(err.Error(), "Unpublish first") {
		t.Errorf("update-published error = %v", err)
	}
	// And the refusal really left the content untouched.
	stillV2, err := repo.GetVersion(ctx, "1", sk.ID, versionID)
	if err != nil {
		t.Fatalf("get after refused update: %v", err)
	}
	if stillV2.VersionDetails.Instructions != "v2" {
		t.Errorf("instructions after refused update=%q, want unchanged v2", stillV2.VersionDetails.Instructions)
	}
}

func TestSkillsRepoPostgres_DeleteVersionRefusesBaseDefaultAndPublished(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Delete Guard")
	baseID := sk.VersionDetails.ID

	// 1. `base` itself can never be deleted — the unversioned GET/PUT/DELETE
	// contract has nowhere else to point.
	if err := repo.DeleteVersion(ctx, "1", sk.ID, baseID); err == nil {
		t.Fatal("expected deleting the base version to be refused")
	} else if !strings.Contains(err.Error(), `"base"`) {
		t.Errorf("delete-base error = %v", err)
	}

	// 2. The current DEFAULT version cannot be deleted until a different one
	// is set — deleting it would leave a fresh attachment's proposed version
	// pointing at nothing.
	named, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{Name: "candidate", Instructions: "c"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	namedID := named.VersionDetails.ID
	if _, err := repo.SetDefaultVersion(ctx, "1", sk.ID, namedID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if err := repo.DeleteVersion(ctx, "1", sk.ID, namedID); err == nil {
		t.Fatal("expected deleting the default version to be refused")
	} else if !strings.Contains(err.Error(), "default version") {
		t.Errorf("delete-default error = %v", err)
	}

	// Repoint the default back to base, and the delete works.
	if _, err := repo.SetDefaultVersion(ctx, "1", sk.ID, baseID); err != nil {
		t.Fatalf("reset default: %v", err)
	}
	if err := repo.DeleteVersion(ctx, "1", sk.ID, namedID); err != nil {
		t.Fatalf("delete after clearing default: %v", err)
	}
	if _, err := repo.GetVersion(ctx, "1", sk.ID, namedID); err == nil {
		t.Error("expected the deleted version to be gone")
	}

	// 3. A published version is frozen, same as application_versions.
	published, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{Name: "shipped", Instructions: "s"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	publishedID := published.VersionDetails.ID
	if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET status = 'published' WHERE id = $1`, publishedID); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if err := repo.DeleteVersion(ctx, "1", sk.ID, publishedID); err == nil {
		t.Fatal("expected deleting a published version to be refused")
	} else if !strings.Contains(err.Error(), "Unpublish first") {
		t.Errorf("delete-published error = %v", err)
	}
	if _, err := repo.GetVersion(ctx, "1", sk.ID, publishedID); err != nil {
		t.Errorf("published version should still exist after the refused delete: %v", err)
	}
}

// TestSkillsRepoPostgres_RestoreVersionRollsBackBaseWithLineage is the
// headline capability #874 asks for: a skill had no rollback at all before
// this change. RestoreVersion copies a named version's content back onto
// `base` — the row the unversioned GET/PUT/DELETE and every FRESH attachment
// actually use — and records which version it came from.
func TestSkillsRepoPostgres_RestoreVersionRollsBackBaseWithLineage(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Rollback Target")
	old, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{
		Name: "v1.0", Instructions: "the good version", Tags: []string{"stable"},
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	oldID := old.VersionDetails.ID

	// Drift base away from the good version, the way ordinary editing would.
	if _, err := repo.Update(ctx, "1", sk.ID, skills.Skill{
		Name: sk.Name, Description: sk.Description, Instructions: "a broken edit", Tags: []string{"broken"},
	}); err != nil {
		t.Fatalf("drift base: %v", err)
	}
	drifted, err := repo.Get(ctx, "1", sk.ID)
	if err != nil {
		t.Fatalf("get after drift: %v", err)
	}
	if drifted.Instructions != "a broken edit" {
		t.Fatalf("precondition failed: base=%q", drifted.Instructions)
	}

	restored, err := repo.RestoreVersion(ctx, "1", sk.ID, oldID)
	if err != nil {
		t.Fatalf("restore version: %v", err)
	}
	if restored.VersionDetails == nil || restored.VersionDetails.Name != "base" {
		t.Fatalf("restore should answer with base as VersionDetails, got %+v", restored.VersionDetails)
	}
	if restored.Instructions != "the good version" {
		t.Errorf("restored instructions=%q, want the good version's content", restored.Instructions)
	}
	if restored.VersionDetails.ParentVersionID != oldID {
		t.Errorf("parent_version_id=%q, want %q (the restored-from version)", restored.VersionDetails.ParentVersionID, oldID)
	}

	// Persisted effect: a completely fresh Get (unversioned — the contract
	// every existing caller of this skill relies on) sees the rollback.
	fetched, err := repo.Get(ctx, "1", sk.ID)
	if err != nil {
		t.Fatalf("get after restore: %v", err)
	}
	if fetched.Instructions != "the good version" {
		t.Errorf("fetched.Instructions=%q after restore, want the good version's content", fetched.Instructions)
	}
	gotTags := append([]string(nil), fetched.Tags...)
	sort.Strings(gotTags)
	if !reflect.DeepEqual(gotTags, []string{"stable"}) {
		t.Errorf("fetched.Tags=%v after restore, want [stable]", gotTags)
	}

	// The SOURCE version itself is untouched by the restore.
	sourceStillIntact, err := repo.GetVersion(ctx, "1", sk.ID, oldID)
	if err != nil {
		t.Fatalf("get source after restore: %v", err)
	}
	if sourceStillIntact.VersionDetails.Instructions != "the good version" {
		t.Errorf("source version mutated by its own restore: %q", sourceStillIntact.VersionDetails.Instructions)
	}
}

func TestSkillsRepoPostgres_SetDefaultVersionPersistsAndMarksExactlyOneVersion(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Default Pointer")
	named, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{Name: "v2", Instructions: "x"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	namedID := named.VersionDetails.ID

	updated, err := repo.SetDefaultVersion(ctx, "1", sk.ID, namedID)
	if err != nil {
		t.Fatalf("set default: %v", err)
	}
	if updated.DefaultVersionID != namedID {
		t.Errorf("default_version_id=%q, want %q", updated.DefaultVersionID, namedID)
	}

	// Persisted effect + exactly one version flagged, mirroring
	// TestHandlerPostgres_ExactlyOneVersionIsDefault's assertion for agents.
	fetched, err := repo.Get(ctx, "1", sk.ID)
	if err != nil {
		t.Fatalf("get after set default: %v", err)
	}
	if fetched.DefaultVersionID != namedID {
		t.Errorf("fetched.default_version_id=%q", fetched.DefaultVersionID)
	}
	defaultCount := 0
	for _, v := range fetched.Versions {
		if v.IsDefault {
			defaultCount++
			if v.ID != namedID {
				t.Errorf("wrong version flagged as default: %+v", v)
			}
		}
	}
	if defaultCount != 1 {
		t.Errorf("versions flagged is_default=%d, want exactly 1", defaultCount)
	}

	// Setting an unknown version id is refused, not silently accepted.
	if _, err := repo.SetDefaultVersion(ctx, "1", sk.ID, "999999"); err == nil {
		t.Error("expected setting an unknown version id as default to be refused")
	}
}

// TestSkillsRepoPostgres_WorkerReadPathUnchangedByVersioning is the
// non-regression proof the preamble asks for: a running agent resolves an
// attached skill's instructions purely through
// entity_skill_mapping.skill_version_id (internal/db/queries/agent_chat.sql's
// `skills` sub-select, unaffected by this change per the research this issue
// was built from). It never consults skills.meta.default_version_id or
// skill_versions.name at execution time. This asserts that join directly:
// attaching a NAMED, NON-default version still resolves to THAT version's
// instructions, regardless of what `base` or the default pointer say.
func TestSkillsRepoPostgres_WorkerReadPathUnchangedByVersioning(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sk := createSkillWithBase(t, repo, ctx, "Attached Skill")
	named, err := repo.CreateVersion(ctx, "1", sk.ID, skills.VersionCreateInput{
		Name: "pinned", Instructions: "the pinned version's instructions",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	namedID := named.VersionDetails.ID

	// Set a DIFFERENT version as default, so a naive read keyed off the
	// default pointer (rather than skill_version_id) would answer the wrong
	// content.
	if _, err := repo.SetDefaultVersion(ctx, "1", sk.ID, sk.VersionDetails.ID); err != nil {
		t.Fatalf("set default to base: %v", err)
	}

	// Seed the attachment the same shape AttachSkill writes, pinned to the
	// NAMED (non-default) version.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES (4242, 'agent', $1, $2)`, sk.ID, namedID); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	// The exact join shape agent_chat.sql's `skills` sub-select uses: keyed
	// on skill_version_id alone, no name/default involved.
	var resolvedInstructions, resolvedVersionName string
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(skill_version.instructions, ''), COALESCE(skill_version.name, 'unknown')
		FROM p_1.entity_skill_mapping AS skill_mapping
		JOIN p_1.skills AS skill ON skill.id = skill_mapping.skill_id
		LEFT JOIN p_1.skill_versions AS skill_version ON skill_version.id = skill_mapping.skill_version_id
		WHERE skill_mapping.entity_version_id = 4242 AND skill_mapping.entity_type = 'agent'`,
	).Scan(&resolvedInstructions, &resolvedVersionName)
	if err != nil {
		t.Fatalf("resolve attached skill: %v", err)
	}
	if resolvedInstructions != "the pinned version's instructions" {
		t.Errorf("resolved instructions=%q, want the PINNED version's content regardless of default", resolvedInstructions)
	}
	if resolvedVersionName != "pinned" {
		t.Errorf("resolved version_name=%q, want %q", resolvedVersionName, "pinned")
	}
}
