package admin_test

// Worker unit M9: deleting a user owes their PERSONAL memories (in every
// project) a deletion. The obligation is journaled IN the delete's transaction,
// with project 0 (every project): no membership lookup, which the cascade would
// have emptied. A probe journal writes a row in the transaction it is handed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
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

func TestDeletingAUserJournalsTheirMemoryCleanupForEveryProject(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE memory_cleanup_probe (project_id bigint, user_id bigint)`); err != nil {
		t.Fatal(err)
	}
	router := adminUsersRouter(admin.NewHandler(pool, admin.WithMemoryUserJournal(probeJournal{})), &auth.User{ID: "1", UserID: "1"})
	const victim = "a14-editor@autotest.local"
	id := userID(t, pool, victim)

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "delete", "users": []map[string]any{{"id": id}}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST delete status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var got string
	if err := pool.QueryRow(ctx, `SELECT project_id || ':' || user_id FROM memory_cleanup_probe`).Scan(&got); err != nil {
		t.Fatalf("no journal row: %v", err)
	}
	if want := fmt.Sprintf("0:%d", id); got != want {
		t.Fatalf("journaled %s, want %s (project 0: every project)", got, want)
	}
}

func TestAUserIsNotDeletedWhenTheirMemoryCleanupCannotBeJournaled(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	router := adminUsersRouter(admin.NewHandler(pool, admin.WithMemoryUserJournal(probeJournal{fail: true})), &auth.User{ID: "1", UserID: "1"})
	const victim = "a14-editor@autotest.local"
	id := userID(t, pool, victim)

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "delete", "users": []map[string]any{{"id": id}}})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("POST delete status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM public.auth_core__user WHERE email = $1`, victim).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("the user was deleted although the cleanup could not be journaled: %d (%v)", remaining, err)
	}
}
