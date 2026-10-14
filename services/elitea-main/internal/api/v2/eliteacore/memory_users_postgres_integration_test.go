package eliteacore_test

// Worker unit M9: a user removed from a project owes their PERSONAL memories
// there a deletion, and the obligation is journaled IN the removal's
// transaction (not asked of the vector store after the fact, where a failure
// would be lost). A probe journal stands in for the real one: it writes a row in
// the transaction it is handed, so these tests prove WHICH transaction it was
// and WHEN it ran.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

type probeJournal struct{ fail bool }

func (p probeJournal) EnqueueMemoryUsers(ctx context.Context, tx pgx.Tx, projectID int64, userIDs ...int64) (string, error) {
	if p.fail {
		return "", errors.New("journal refused")
	}
	for _, id := range userIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO memory_cleanup_probe (project_id, user_id) VALUES ($1, $2)`, projectID, id); err != nil {
			return "", err
		}
	}
	return "memory_user", nil
}

func probeRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT project_id || ':' || user_id FROM memory_cleanup_probe ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		found = append(found, row)
	}
	return found
}

func memoryProbeFixture(t *testing.T, journal probeJournal) (*pgxpool.Pool, *eliteacore.Handler) {
	t.Helper()
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE memory_cleanup_probe (project_id bigint, user_id bigint)`); err != nil {
		t.Fatal(err)
	}
	return pool, eliteacore.NewHandler(pool, eliteacore.WithMemoryUserJournal(journal))
}

func TestRemovingAUserFromAProjectJournalsTheirMemoryCleanupInTheSameTransaction(t *testing.T) {
	pool, handler := memoryProbeFixture(t, probeJournal{})
	router := usersWriteRouter(handler)
	const leaving = "e2e-member@autotest.local"
	userID := memberUserID(t, readMembers(t, router), leaving)

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/users/default/%d?id%%5B%%5D=%s", usersWriteProjectID, userID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if _, still := memberRoles(readMembers(t, router), leaving); still {
		t.Fatalf("%s is still a member", leaving)
	}
	if got, want := probeRows(t, pool), []string{fmt.Sprintf("%d:%s", usersWriteProjectID, userID)}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("journaled %v, want %v", got, want)
	}
}

func TestAMembershipRemovalIsRefusedAndRolledBackWhenItsMemoryCleanupCannotBeJournaled(t *testing.T) {
	pool, handler := memoryProbeFixture(t, probeJournal{fail: true})
	router := usersWriteRouter(handler)
	const member = "e2e-member@autotest.local"
	userID := memberUserID(t, readMembers(t, router), member)

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/users/default/%d?id%%5B%%5D=%s", usersWriteProjectID, userID), nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
	if _, still := memberRoles(readMembers(t, router), member); !still {
		t.Fatalf("%s was removed although the cleanup could not be journaled: the memories would be orphaned", member)
	}
	if got := probeRows(t, pool); len(got) != 0 {
		t.Fatalf("journaled %v", got)
	}
}

func TestAChangeOfRolesIsNotALeaveAndJournalsNothing(t *testing.T) {
	pool, handler := memoryProbeFixture(t, probeJournal{})
	router := usersWriteRouter(handler)
	const edited = "e2e-member@autotest.local"
	userID := memberUserID(t, readMembers(t, router), edited)

	recorder := usersWriteDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
		map[string]any{"userId": userID, "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := probeRows(t, pool); len(got) != 0 {
		t.Fatalf("a role change journaled %v", got)
	}
}
