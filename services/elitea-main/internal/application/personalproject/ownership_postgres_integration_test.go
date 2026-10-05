package personalproject_test

// The ownership check behind the project settings gate (#6789). The gate
// admits an editor to the settings writes in ONE project only: their own
// personal project. Each case below is a project that must NOT count.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
)

func seedProjectRow(t *testing.T, pool *pgxpool.Pool, name string, ownerID int64) string {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO centry.project (name, owner_id, plugins, create_success)
		 VALUES ($1, $2, '{}', true) RETURNING id`, name, ownerID).Scan(&id); err != nil {
		t.Fatalf("seed project %q: %v", name, err)
	}
	return strconv.FormatInt(id, 10)
}

func TestIsOwnPersonalProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	check := personalproject.NewOwnershipCheck(pool)

	owner := seedUser(t, pool, "owner-6789@autotest.local", "Owner")
	other := seedUser(t, pool, "other-6789@autotest.local", "Other")

	personal := seedProjectRow(t, pool, personalproject.Name(owner), owner)
	team := seedProjectRow(t, pool, "Team 6789", owner)
	// A row that carries the owner's personal NAME but another owner: pylon-era
	// data and the free-text project name can both produce it.
	squatted := seedProjectRow(t, pool, personalproject.Name(other)+"0", other)
	renamed := seedProjectRow(t, pool, personalproject.Name(owner), other)

	for _, tc := range []struct {
		name      string
		projectID string
		userID    int64
		want      bool
	}{
		{"own personal project", personal, owner, true},
		{"someone else's personal project", personal, other, false},
		{"own team project", team, owner, false},
		{"personal name, other owner", renamed, owner, false},
		{"personal name, owner is not the name's user", renamed, other, false},
		{"name of another user id", squatted, other, false},
		{"unknown project", "999999", owner, false},
		{"malformed project id", "x7", owner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := check.IsOwnPersonalProject(ctx, tc.projectID, tc.userID)
			if err != nil {
				t.Fatalf("IsOwnPersonalProject: %v", err)
			}
			if got != tc.want {
				t.Fatalf("IsOwnPersonalProject(%s, %d) = %v, want %v", tc.projectID, tc.userID, got, tc.want)
			}
		})
	}

	// A check built over no pool answers false and does not panic.
	if got, err := personalproject.NewOwnershipCheck(nil).IsOwnPersonalProject(ctx, personal, owner); got || err != nil {
		t.Fatalf("nil-pool check = (%v, %v), want (false, nil)", got, err)
	}
}
