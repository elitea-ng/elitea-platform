package repos

// Skill trust (ADR-0051 amendment 1, A1.6; skills epic story S1) against a
// REAL migrated database. It SKIPS without ELITEA_TEST_DATABASE_URL.
//
// The reader decides, from the rows alone, whether a picked skill's text is
// untrusted: it was forked from another project and no active trust row names
// its CURRENT revision. Each case asserts a value a missing join, a missing
// revision compare or a revoke that deleted nothing could not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const trustProject = int64(1)

func seedTrustSkill(t *testing.T, pool *pgxpool.Pool, id int, name, instructions, meta string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skills (id, name, description) VALUES ($1, $2, 'd')`, id, name); err != nil {
		t.Fatalf("seed skill %d: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_versions (skill_id, name, instructions, meta) VALUES ($1, 'base', $2, $3::jsonb)`,
		id, instructions, meta); err != nil {
		t.Fatalf("seed skill version %d: %v", id, err)
	}
}

func resolveTrust(t *testing.T, repo *ProjectSkillsRepo, ids ...int64) map[int64]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 30*time.Second)
	defer cancel()
	skills, err := repo.ResolveCurrentProjectSkills(ctx, trustProject, ids)
	if err != nil {
		t.Fatalf("ResolveCurrentProjectSkills: %v", err)
	}
	untrusted := make(map[int64]bool, len(skills))
	for _, skill := range skills {
		untrusted[skill.SkillID] = skill.Untrusted
		if skill.Untrusted && skill.PublisherProjectID != 2 && skill.SkillID == 801 {
			t.Fatalf("publisher project = %d, want 2", skill.PublisherProjectID)
		}
	}
	return untrusted
}

func TestProjectSkillsReadFlagsForeignTextUntilItsRevisionIsTrusted(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo := NewProjectSkillsRepo(pool)

	const text = "Triage steps"
	seedTrustSkill(t, pool, 801, "forked", text, `{"parent_project_id": 2, "parent_entity_id": 5}`)
	seedTrustSkill(t, pool, 802, "own", "Own steps", `{}`)
	seedTrustSkill(t, pool, 803, "self-parent", "Self steps", `{"parent_project_id": 1}`)
	seedTrustSkill(t, pool, 804, "malformed", "Odd steps", `{"parent_project_id": "abc"}`)

	got := resolveTrust(t, repo, 801, 802, 803, 804)
	if !got[801] || got[802] || got[803] || !got[804] {
		t.Fatalf("untrusted = %v, want 801 and 804 only (a malformed lineage must not read as trusted)", got)
	}

	// Trust this exact revision: the forked skill is trusted.
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_trust (skill_id, revision, publisher_project_id, trusted_by) VALUES (801, $1, 2, 7)`,
		skilltrust.Revision(text)); err != nil {
		t.Fatalf("insert trust: %v", err)
	}
	if got = resolveTrust(t, repo, 801); got[801] {
		t.Fatal("a foreign skill whose current revision is trusted must read as trusted")
	}

	// The text changes: a new revision, untrusted again.
	if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET instructions = $1 WHERE skill_id = 801`, text+" Then merge it."); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got = resolveTrust(t, repo, 801); !got[801] {
		t.Fatal("an edited text is a new revision and must be untrusted again")
	}

	// Back to the trusted text, then revoke: untrusted, and the row is kept.
	if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET instructions = $1 WHERE skill_id = 801`, text); err != nil {
		t.Fatal(err)
	}
	if got = resolveTrust(t, repo, 801); got[801] {
		t.Fatal("restoring the trusted text restores the trust")
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.skill_trust SET revoked_at = now(), revoked_by = 7 WHERE skill_id = 801`); err != nil {
		t.Fatal(err)
	}
	if got = resolveTrust(t, repo, 801); !got[801] {
		t.Fatal("a revoked trust must read as untrusted")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.skill_trust WHERE skill_id = 801`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("a revoke keeps its history: rows=%d err=%v", rows, err)
	}

	// At most one ACTIVE row per skill and revision; a revoked one does not count.
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_trust (skill_id, revision, trusted_by) VALUES (801, $1, 7)`, skilltrust.Revision(text)); err != nil {
		t.Fatalf("a new trust after a revoke: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_trust (skill_id, revision, trusted_by) VALUES (801, $1, 7)`, skilltrust.Revision(text)); err == nil {
		t.Fatal("two active trust rows for one skill revision must be refused")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_trust (skill_id, revision) VALUES (802, 'not-a-digest')`); err == nil {
		t.Fatal("a revision that is not a SHA-256 digest must be refused")
	}
}

// The migration trusts the current text of the forks that existed before the
// table, once, so today's `~` picks of them do not change behaviour; it is
// idempotent and never trusts a skill made in the project.
func TestSkillTrustMigrationBackfillsExistingForksOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	seedTrustSkill(t, pool, 811, "old fork", "Old fork steps", `{"parent_project_id": 2}`)
	seedTrustSkill(t, pool, 812, "own", "Own steps", `{}`)
	seedTrustSkill(t, pool, 813, "self-parent", "Self steps", `{"parent_project_id": 1}`)

	script, err := platformmigrations.Files.ReadFile("tenant/0250_skill_trust.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for run := 1; run <= 2; run++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL search_path TO p_1`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(script)); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := pool.Query(ctx, `SELECT skill_id, revision, publisher_project_id, source FROM p_1.skill_trust ORDER BY skill_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type backfilled struct {
		skill, publisher int
		revision, source string
	}
	var got []backfilled
	for rows.Next() {
		var row backfilled
		if err := rows.Scan(&row.skill, &row.revision, &row.publisher, &row.source); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if len(got) != 1 || got[0].skill != 811 || got[0].publisher != 2 || got[0].source != "migration" ||
		got[0].revision != skilltrust.Revision("Old fork steps") {
		t.Fatalf("backfill = %+v, want one migration row for skill 811 only", got)
	}
}

// A person in the project who saves new text on a forked skill wrote and read it,
// so that revision is trusted (trustEditedForeignBase runs in the update's
// transaction when the base text changed). The helper trusts the CURRENT base
// text of a foreign skill, once, by the caller, and nothing for a skill made here.
func TestTrustingAnEditedForeignBaseRecordsTheCallerOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewProjectSkillsRepo(pool)
	seedTrustSkill(t, pool, 821, "forked", "Original steps", `{"parent_project_id": 2}`)
	seedTrustSkill(t, pool, 822, "own", "Own steps", `{}`)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 30*time.Second)
	defer cancel()
	schema, err := tenantSchema("1")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(id string, text string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE p_1.skill_versions SET instructions = $1 WHERE skill_id = $2::int`, text, id); err != nil {
			t.Fatal(err)
		}
		if err := trustEditedForeignBase(ctx, tx, schema, "1", id); err != nil {
			t.Fatalf("trustEditedForeignBase: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if got := resolveTrust(t, repo, 821); !got[821] {
		t.Fatal("a fresh fork is untrusted")
	}
	edit("821", "Edited steps")
	if got := resolveTrust(t, repo, 821); got[821] {
		t.Fatal("text a member of the project saved must be trusted")
	}
	var by int
	var source string
	if err := pool.QueryRow(context.Background(), `SELECT trusted_by, source FROM p_1.skill_trust WHERE skill_id = 821 AND revoked_at IS NULL`).Scan(&by, &source); err != nil || by != 7 || source != "user" {
		t.Fatalf("trust row: by=%d source=%q err=%v", by, source, err)
	}
	// The same text again adds no second active row.
	edit("821", "Edited steps")
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.skill_trust WHERE skill_id = 821`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d err=%v", rows, err)
	}
	// A skill made in this project gets no row.
	edit("822", "Own steps, edited")
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.skill_trust WHERE skill_id = 822`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("an own skill got %d trust rows (err=%v)", rows, err)
	}
}
