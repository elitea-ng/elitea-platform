package repos

// The trust store behind the skill trust routes (ADR-0051 amendment 1, A1.6)
// against a REAL migrated database. It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func TestTrustingAForeignSkillByRevision(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewSkillTrustRepo(pool)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 60*time.Second)
	defer cancel()
	const text = "Forked steps"
	seedTrustSkill(t, pool, 960, "Forked", text, `{"parent_project_id": 2}`)
	seedTrustSkill(t, pool, 961, "Own", "Own steps", `{}`)

	status, err := repo.Get(ctx, "1", 960)
	if err != nil || !status.Foreign || status.State != "untrusted" || status.Revision != skilltrust.Revision(text) ||
		status.PublisherProjectID == nil || *status.PublisherProjectID != 2 || len(status.History) != 0 {
		t.Fatalf("a fresh fork: %+v err=%v", status, err)
	}

	// A review of other text must not trust this text.
	var apiErr *apierr.APIError
	if _, _, err := repo.Trust(ctx, "1", 960, skilltrust.Revision("something else")); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("a stale revision: error = %v, want 409", err)
	}
	if got, _ := repo.Get(ctx, "1", 960); got.State != "untrusted" {
		t.Fatal("a refused trust must not trust")
	}

	trusted, changed, err := repo.Trust(ctx, "1", 960, status.Revision)
	if err != nil || !changed || trusted.State != "trusted" || len(trusted.History) != 1 ||
		trusted.History[0].TrustedBy == nil || *trusted.History[0].TrustedBy != 7 || trusted.History[0].Source != "user" {
		t.Fatalf("trust: %+v changed=%v err=%v", trusted, changed, err)
	}
	// Trusting what is trusted already changes nothing and adds no row.
	if _, changed, err := repo.Trust(ctx, "1", 960, status.Revision); err != nil || changed {
		t.Fatalf("trusting twice: changed=%v err=%v", changed, err)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.skill_trust WHERE skill_id = 960`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d err=%v", rows, err)
	}

	// The text changes: a new revision, untrusted again, and the old decision stays in the history.
	if _, err := pool.Exec(context.Background(), `UPDATE p_1.skill_versions SET instructions = $1 WHERE skill_id = 960`, text+" Then merge it."); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Get(ctx, "1", 960); err != nil || got.State != "untrusted" || got.Revision == status.Revision || len(got.History) != 1 {
		t.Fatalf("after an edit: %+v err=%v", got, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE p_1.skill_versions SET instructions = $1 WHERE skill_id = 960`, text); err != nil {
		t.Fatal(err)
	}

	// Revoke: untrusted again, the row kept with who and when.
	revoked, count, err := repo.Revoke(ctx, "1", 960)
	if err != nil || count != 1 || revoked.State != "untrusted" || len(revoked.History) != 1 ||
		revoked.History[0].RevokedBy == nil || *revoked.History[0].RevokedBy != 7 || revoked.History[0].RevokedAt == nil {
		t.Fatalf("revoke: %+v count=%d err=%v", revoked, count, err)
	}
	if _, count, err := repo.Revoke(ctx, "1", 960); err != nil || count != 0 {
		t.Fatalf("revoking twice: count=%d err=%v", count, err)
	}
	// And it can be trusted again: a new row, two in the history.
	if again, changed, err := repo.Trust(ctx, "1", 960, status.Revision); err != nil || !changed || again.State != "trusted" || len(again.History) != 2 {
		t.Fatalf("trusting after a revoke: %+v changed=%v err=%v", again, changed, err)
	}
}

func TestASkillMadeInTheProjectNeedsNoTrustAndAnUnreadableOneIsNotFound(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewSkillTrustRepo(pool)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 60*time.Second)
	defer cancel()
	seedTrustSkill(t, pool, 970, "Own", "Own steps", `{}`)

	status, err := repo.Get(ctx, "1", 970)
	if err != nil || status.Foreign || status.State != "not_required" {
		t.Fatalf("an own skill: %+v err=%v", status, err)
	}
	var apiErr *apierr.APIError
	if _, _, err := repo.Trust(ctx, "1", 970, status.Revision); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("trusting an own skill: error = %v, want 400", err)
	}
	if _, err := repo.Get(ctx, "1", 99999); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("a skill that does not exist: error = %v, want 404", err)
	}
	// Trust is recorded for a person: a token without an owning user records none.
	if _, _, err := repo.Trust(context.Background(), "1", 970, status.Revision); err == nil {
		t.Fatal("trust without a caller must fail")
	}
}
