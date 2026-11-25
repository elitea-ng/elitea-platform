package repos

// The `inherit` token notice on the ledgered corpus (ADR-0059 R3.15b, issue
// 2619): shared 0278's delivery column, the candidate query, the queue into
// the access notice outbox of 0277, and the end date the token routes read.
// The database is 001_initial.sql and the shared corpus (newEntitlementPool):
// the candidate query joins auth_core.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

func seedInheritAccount(t *testing.T, pool *pgxpool.Pool, suspended bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__user (email, name, suspended) VALUES ($1, 'Token owner', $2) RETURNING id`,
		"inherit-"+uuid.NewString()+"@autotest.local", suspended).Scan(&id); err != nil {
		t.Fatalf("seed an account: %v", err)
	}
	return id
}

func seedInheritToken(t *testing.T, pool *pgxpool.Pool, owner int64, name, access string, expires *time.Time) int64 {
	t.Helper()
	scopes := []string{}
	if access == "scoped" {
		scopes = []string{"admin.auth.users"}
	}
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access, admin_scopes, expires)
VALUES ($1, $2, $3, $4, $5::text[], $6::timestamp) RETURNING id`,
		uuid.NewString(), owner, name, access, scopes, expires).Scan(&id); err != nil {
		t.Fatalf("seed a %s token: %v", access, err)
	}
	return id
}

func TestInheritTokenNoticeCandidates(t *testing.T) {
	pool := newEntitlementPool(t)
	ctx := t.Context()
	now := time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)

	// The migration: the column is there, nullable and timestamptz.
	var dataType, nullable string
	if err := pool.QueryRow(ctx, `
SELECT data_type, is_nullable FROM information_schema.columns
WHERE table_schema = 'elitea_identity' AND table_name = 'token_lifecycle'
  AND column_name = 'inherit_notice_delivered_at'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("0278 added no inherit_notice_delivered_at: %v", err)
	}
	if dataType != "timestamp with time zone" || nullable != "YES" {
		t.Fatalf("inherit_notice_delivered_at is %s, nullable %s", dataType, nullable)
	}

	owner := seedInheritAccount(t, pool, false)
	ci := seedInheritToken(t, pool, owner, "ci", "inherit", nil)
	future := now.Add(30 * 24 * time.Hour)
	backup := seedInheritToken(t, pool, owner, "backup", "inherit", &future)
	past := now.Add(-time.Hour)
	seedInheritToken(t, pool, owner, "expired", "inherit", &past)
	seedInheritToken(t, pool, owner, "none", "none", nil)
	seedInheritToken(t, pool, owner, "scoped", "scoped", nil)
	// An owner whose only tokens are none and scoped is no candidate.
	plain := seedInheritAccount(t, pool, false)
	seedInheritToken(t, pool, plain, "none", "none", nil)
	seedInheritToken(t, pool, plain, "scoped", "scoped", nil)
	// A suspended owner is no candidate.
	suspended := seedInheritAccount(t, pool, true)
	seedInheritToken(t, pool, suspended, "inherit", "inherit", nil)
	// An owner already told is no candidate.
	told := seedInheritAccount(t, pool, false)
	toldToken := seedInheritToken(t, pool, told, "told", "inherit", nil)
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_lifecycle (token_id, inherit_notice_delivered_at) VALUES ($1, $2)`,
		toldToken, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// An owner with no address is listed, with "" (the notifier counts them).
	unreachable := seedInheritAccount(t, pool, false)
	seedInheritToken(t, pool, unreachable, "no-mail", "inherit", nil)
	if _, err := pool.Exec(ctx, `UPDATE public.auth_core__user SET email = '' WHERE id = $1`, unreachable); err != nil {
		t.Fatal(err)
	}

	repo, err := NewPATExpiryNotificationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := repo.ListInheritTokenOwners(ctx, now, 500)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var mine []patexpiry.InheritOwner
	sawUnreachable := false
	for _, candidate := range owners {
		switch candidate.UserID {
		case owner:
			mine = append(mine, candidate)
		case unreachable:
			// Listed with no address, for the notifier to count.
			sawUnreachable = candidate.Email == ""
		case plain, suspended, told:
			t.Errorf("user %d is a candidate: %+v", candidate.UserID, candidate)
		}
	}
	if !sawUnreachable {
		t.Errorf("the owner with no address is not listed with an empty address: %+v", owners)
	}
	if len(mine) != 1 {
		t.Fatalf("the owner is listed %d times, want once: %+v", len(mine), owners)
	}
	want := []patexpiry.InheritToken{{ID: ci, Name: "ci"}, {ID: backup, Name: "backup"}}
	if ci > backup {
		want[0], want[1] = want[1], want[0]
	}
	if got := mine[0].Tokens; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tokens = %+v, want the two live inherit tokens %+v", got, want)
	}

	// Queued once per owner, date and token list.
	for i, wantQueued := range []bool{true, false} {
		queued, err := repo.QueueInheritTokenNotice(ctx, mine[0], "2027-01-31", now)
		if err != nil || queued != wantQueued {
			t.Fatalf("queue %d = %v, %v; want %v", i, queued, err, wantQueued)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_auth.admin_access_notices WHERE kind = 'inherit_token' AND recipient_user_id = $1`,
		owner).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("outbox rows = %d (%v), want 1", rows, err)
	}
	// While that row waits, the owner takes no slot of the limit again.
	waiting, err := repo.ListInheritTokenOwners(ctx, now, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range waiting {
		if candidate.UserID == owner {
			t.Fatalf("an owner whose notice waits is listed again: %+v", candidate)
		}
	}

	// The end date: the sunset date before the delivery, the later of the
	// two after it; nothing while the date is not set.
	ends, err := adminaccess.InheritTokenEndDatesAt(ctx, pool, "2027-01-31", []int64{ci, toldToken})
	if err != nil {
		t.Fatal(err)
	}
	if ends[ci] != "2027-01-31" || ends[toldToken] != "2027-02-08" {
		t.Fatalf("end dates = %v", ends)
	}
	if ends, err := adminaccess.InheritTokenEndDatesAt(ctx, pool, "", []int64{ci}); err != nil || len(ends) != 0 {
		t.Fatalf("with no sunset date: %v, %v", ends, err)
	}
}
