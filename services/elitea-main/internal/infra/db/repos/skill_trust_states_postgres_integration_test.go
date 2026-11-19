package repos

// The trust state a skill list row and a skill read carry (ADR-0051 amendment 1,
// A1.6; skills epic 2169, #2245) against a REAL migrated database. It SKIPS
// without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
)

func TestSkillListRowsAndReadsCarryTheTrustState(t *testing.T) {
	pool := newSkillsTestPool(t)
	applySkillTrustMigration(t, pool)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 60*time.Second)
	defer cancel()
	seedListedSkill(t, pool, 1100, "Forked, untrusted", "Fork steps", `{"parent_project_id": 2}`)
	seedListedSkill(t, pool, 1101, "Forked, trusted", "Reviewed steps", `{"parent_project_id": 2}`)
	seedListedSkill(t, pool, 1102, "Own", "Own steps", `{}`)
	seedListedSkill(t, pool, 1103, "Forked, changed since", "Old steps", `{"parent_project_id": 2}`)
	for _, row := range []struct {
		id   int
		text string
	}{{1101, "Reviewed steps"}, {1103, "Old steps"}} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.skill_trust (skill_id, revision, publisher_project_id, trusted_by, source) VALUES ($1, $2, 2, 7, 'user')`,
			row.id, skilltrust.Revision(row.text)); err != nil {
			t.Fatal(err)
		}
	}
	// The text of 1103 changes after it was trusted: a new revision, untrusted again.
	if _, err := pool.Exec(context.Background(), `UPDATE p_1.skill_versions SET instructions = 'New steps' WHERE skill_id = 1103`); err != nil {
		t.Fatal(err)
	}
	// A revoked decision trusts nothing.
	seedListedSkill(t, pool, 1104, "Forked, revoked", "Revoked steps", `{"parent_project_id": 2}`)
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.skill_trust (skill_id, revision, publisher_project_id, trusted_by, source, revoked_at, revoked_by) VALUES (1104, $1, 2, 7, 'user', now(), 7)`,
		skilltrust.Revision("Revoked steps")); err != nil {
		t.Fatal(err)
	}

	want := map[string]skills.SkillTrust{
		"1100": {Foreign: true, State: "untrusted"},
		"1101": {Foreign: true, State: "trusted"},
		"1102": {Foreign: false, State: "not_required"},
		"1103": {Foreign: true, State: "untrusted"},
		"1104": {Foreign: true, State: "untrusted"},
	}
	repo := NewSkillsRepo(pool)
	list, err := repo.List(ctx, "1", skills.ListParams{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := 0
	for _, item := range list.Items {
		expected, ok := want[item.ID]
		if !ok {
			continue
		}
		seen++
		if item.Trust == nil || item.Trust.Foreign != expected.Foreign || item.Trust.State != expected.State {
			t.Errorf("list row %s: trust = %+v, want %+v", item.ID, item.Trust, expected)
		}
		if expected.Foreign && (item.Trust == nil || item.Trust.PublisherProjectID == nil || *item.Trust.PublisherProjectID != 2) {
			t.Errorf("list row %s: publisher = %+v, want 2", item.ID, item.Trust)
		}
	}
	if seen != len(want) {
		t.Fatalf("the list held %d of the %d seeded skills", seen, len(want))
	}
	// A read carries the same state as the row.
	for id, expected := range want {
		got, err := repo.Get(ctx, "1", id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Trust == nil || got.Trust.State != expected.State {
			t.Errorf("read %s: trust = %+v, want state %s", id, got.Trust, expected.State)
		}
	}
	// Agreement with the trust route: the row says what getSkillTrust says.
	route := NewSkillTrustRepo(pool)
	for id, expected := range want {
		n, _ := strconv.ParseInt(id, 10, 64)
		status, err := route.Get(ctx, "1", n)
		if err != nil || status.State != expected.State {
			t.Errorf("trust route %s: %+v err=%v, want %s", id, status, err, expected.State)
		}
	}
}

// seedListedSkill is seedTrustSkill for a pool the baseline migrations built, where a skill needs its owner.
func seedListedSkill(t *testing.T, pool *pgxpool.Pool, id int, name, instructions, meta string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skills (id, name, description, owner_id, author_id) VALUES ($1, $2, 'd', 1, 1)`, id, name); err != nil {
		t.Fatalf("seed skill %d: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.skill_versions (skill_id, name, instructions, meta, author_id) VALUES ($1, 'base', $2, $3::jsonb, 1)`, id, instructions, meta); err != nil {
		t.Fatalf("seed skill version %d: %v", id, err)
	}
}
