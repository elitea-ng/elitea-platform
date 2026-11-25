package migrations_test

// Shared 0279 (issue 2596, ADR-0059 R2.2b): the request columns of the
// elevation ledger. The duration is frozen at insert, the denial reason code
// belongs to a denial and is frozen with the decision, and both are bounded
// by CHECK constraints.

import (
	"context"
	"strings"
	"testing"
)

func TestElevationRequestColumnsAreBoundedAndFrozen(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()

	insert := func(t *testing.T, duration any) (string, error) {
		t.Helper()
		var id string
		err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, path, reason_code, reason, requester_user_id, requested_at, duration_minutes)
VALUES (7, 41, 'approved', 'incident', 'r', 7, now(), $1)
RETURNING id::text`, duration).Scan(&id)
		return id, err
	}

	for _, minutes := range []int{0, 481} {
		if _, err := insert(t, minutes); err == nil || !strings.Contains(err.Error(), "admin_elevations_duration_bounded") {
			t.Fatalf("a duration of %d minutes was not refused by the bound: %v", minutes, err)
		}
	}
	id, err := insert(t, 60)
	if err != nil {
		t.Fatal(err)
	}

	// The duration is frozen at insert.
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET duration_minutes = 120 WHERE id = $1::uuid`, id); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("an UPDATE of the duration was not refused: %v", err)
	}

	// A denial reason code needs a denial, and one of the list.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'approved', decided_at = now(), level = 'admin', approver_user_id = 8, denial_reason_code = 'other'
WHERE id = $1::uuid`, id); err == nil || !strings.Contains(err.Error(), "admin_elevations_denial_reason_known") {
		t.Fatalf("an approval with a denial reason code was not refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'denied', decided_at = now(), approver_user_id = 8, denial_reason_code = 'free text'
WHERE id = $1::uuid`, id); err == nil || !strings.Contains(err.Error(), "admin_elevations_denial_reason_known") {
		t.Fatalf("an unknown denial reason code was not refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'denied', decided_at = now(), approver_user_id = 8, denial_reason_code = 'not_justified'
WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("a denial with a known reason code was refused: %v", err)
	}

	// The reason code is in the decision group: frozen once decided.
	if _, err := pool.Exec(ctx, `UPDATE elitea_auth.admin_elevations SET denial_reason_code = 'other' WHERE id = $1::uuid`, id); err == nil ||
		!strings.Contains(err.Error(), "the decision is recorded") {
		t.Fatalf("an UPDATE of a recorded denial reason was not refused by the decision group: %v", err)
	}
}
