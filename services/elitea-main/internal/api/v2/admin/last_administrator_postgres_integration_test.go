package admin_test

// The last-administrator rule (ADR-0059 R0.11, issue 2500) on databases built
// from the real corpus: 001_initial.sql plus every ledgered shared migration
// (newSystemInfoPool's template).

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// corpusAccount creates an account and gives it the administration roles.
func corpusAccount(t *testing.T, pool *pgxpool.Pool, email string, roles ...string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create %s: %v", email, err)
	}
	for _, role := range roles {
		tag, err := pool.Exec(t.Context(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = $2`, id, role)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("give %s the %s role: %v (rows %d)", email, role, err, tag.RowsAffected())
		}
	}
	return id
}

func countAdministrators(t *testing.T, pool *pgxpool.Pool) adminaccess.Administrators {
	t.Helper()
	counts, err := adminaccess.CountAdministrators(t.Context(), pool)
	if err != nil {
		t.Fatalf("count administrators: %v", err)
	}
	return counts
}

// corpusUsersRouter mounts the Users routes for a caller who is a global
// administrator by its resolved set; the rule binds that caller too.
func corpusUsersRouter(pool *pgxpool.Pool, callerID int) chi.Router {
	id := strconv.Itoa(callerID)
	principal := &auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession}
	router := chi.NewRouter()
	router.Mount("/u", adminUsersRouter(admin.NewHandler(pool), principal))
	return router
}

// Acceptance (2), first half: a database that came from legacy data, where
// one person holds `admin` and nobody holds `super_admin`. That person is the
// last administrator, and suspending them answers 409.
func TestTheAdminHalfOfTheRuleProtectsALegacyBackedDatabase(t *testing.T) {
	pool := newSystemInfoPool(t)
	legacy := corpusAccount(t, pool, "r011-legacy-admin@autotest.local", adminaccess.RoleAdmin)
	operator := corpusAccount(t, pool, "r011-operator@autotest.local")
	if counts := countAdministrators(t, pool); counts.SuperAdmins != 0 || counts.Admins != 1 {
		t.Fatalf("fixture: %+v, want no super_admin holder and one admin", counts)
	}

	recorder := adminDo(t, corpusUsersRouter(pool, operator), http.MethodPut,
		userSuspend(legacy), map[string]any{"suspended": true})
	expectLastAdministrator(t, recorder)
	var suspended bool
	if err := pool.QueryRow(t.Context(),
		`SELECT suspended FROM public.auth_core__user WHERE id = $1`, legacy).Scan(&suspended); err != nil || suspended {
		t.Fatalf("the last admin was suspended (%v, err %v)", suspended, err)
	}

	// With a second unsuspended holder of `admin`, the same suspension goes
	// through: the rule protects only the last one.
	corpusAccount(t, pool, "r011-second-admin@autotest.local", adminaccess.RoleAdmin)
	expectStatus(t, adminDo(t, corpusUsersRouter(pool, operator), http.MethodPut,
		userSuspend(legacy), map[string]any{"suspended": true}), http.StatusOK)
}

// Acceptance (2), second half: on a fresh database nobody holds either role,
// so there is nothing to protect and the rule refuses nothing.
func TestTheRuleRefusesNothingOnAFreshDatabase(t *testing.T) {
	pool := newSystemInfoPool(t)
	if counts := countAdministrators(t, pool); counts.Protected() {
		t.Fatalf("a fresh database has an administrator: %+v", counts)
	}
	someone := corpusAccount(t, pool, "r011-fresh@autotest.local")
	operator := corpusAccount(t, pool, "r011-fresh-operator@autotest.local")
	router := corpusUsersRouter(pool, operator)

	expectStatus(t, adminDo(t, router, http.MethodPut, userSuspend(someone),
		map[string]any{"suspended": true}), http.StatusOK)
	// The bootstrap account of 001_initial.sql (user 1) is no administrator
	// after shared 0111, so it can be suspended too.
	expectStatus(t, adminDo(t, router, http.MethodPut, userSuspend(1),
		map[string]any{"suspended": true}), http.StatusOK)
}

// Acceptance (4): two concurrent demotions of the last two holders, both
// under LockRoleWrites, leave one holder. The second waits on the lock, reads
// the first one's commit, and is refused.
func TestConcurrentDemotionsOfTheLastTwoHoldersLeaveOne(t *testing.T) {
	pool := newSystemInfoPool(t)
	first := corpusAccount(t, pool, "r011-first@autotest.local", adminaccess.RoleSuperAdmin)
	second := corpusAccount(t, pool, "r011-second@autotest.local", adminaccess.RoleSuperAdmin)
	if counts := countAdministrators(t, pool); counts.SuperAdmins != 2 {
		t.Fatalf("fixture: %+v, want two super_admin holders", counts)
	}
	ctx := t.Context()
	origin := rolegrants.Origin{Source: rolegrants.SourceConsole, ActorUserID: int64(first)}

	demote := func(tx pgx.Tx, userID int) (*adminaccess.Refusal, error) {
		guard, err := adminaccess.BeginLastAdministratorGuard(ctx, tx)
		if err != nil {
			return nil, err
		}
		if _, err := rolegrants.Revoke(ctx, tx, int64(userID), adminaccess.ModeAdministration,
			adminaccess.RoleSuperAdmin, origin); err != nil {
			return nil, err
		}
		return guard.Check(ctx, tx)
	}

	one, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = one.Rollback(context.WithoutCancel(ctx)) }()
	if refusal, err := demote(one, first); err != nil || refusal != nil {
		t.Fatalf("the first demotion: refusal %v, err %v", refusal, err)
	}

	type outcome struct {
		refusal *adminaccess.Refusal
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		two, err := pool.Begin(ctx)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer func() { _ = two.Rollback(context.WithoutCancel(ctx)) }()
		refusal, err := demote(two, second)
		if err == nil && refusal == nil {
			err = two.Commit(ctx)
		}
		done <- outcome{refusal: refusal, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("the second demotion did not wait for the lock: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := one.Commit(ctx); err != nil {
		t.Fatalf("commit the first demotion: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("the second demotion failed: %v", got.err)
	}
	if got.refusal == nil || got.refusal.Code != adminaccess.CodeLastAdministrator {
		t.Fatalf("the second demotion was not refused: %+v", got.refusal)
	}
	if counts := countAdministrators(t, pool); counts.SuperAdmins != 1 {
		t.Fatalf("after both demotions: %+v, want one holder", counts)
	}
}
