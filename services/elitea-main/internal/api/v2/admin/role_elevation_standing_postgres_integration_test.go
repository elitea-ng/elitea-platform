package admin_test

// The exemption is read from standing roles only (ADR-0059 decision 3; story
// R2.1b, issue 2595). A person whose standing role is `admin`, under an
// ACTIVE super_admin-level elevation bound to the session of the request,
// resolves admin.auth.users.super_admin through the real resolver (the
// premise), and still gets 403 when it assigns a role to itself, edits a role
// it holds, or gives the super_admin role: the role rules read the caller from
// the standing-set call, which no elevation reaches.

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// elevatedSessionRef is the browser session reference the elevation is
// bound to. The ledger has no foreign key to browser_sessions.
const elevatedSessionRef int64 = 920001

// seedActiveElevation applies the elevation ledger (shared 0269) to a
// projection-built pool and writes one active elevation of userID at level,
// bound to sessionRef, approved by approverID. A test writes the row directly:
// the ledger function also checks the browser session row, which this
// fixture's schema does not have.
func seedActiveElevation(t *testing.T, pool *pgxpool.Pool, userID, approverID int, sessionRef int64, level string) {
	t.Helper()
	ctx := context.Background()
	ledger, err := migrations.Files.ReadFile("shared/0269_admin_elevations.sql")
	if err != nil {
		t.Fatalf("read the elevation ledger migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(ledger)); err != nil {
		t.Fatalf("apply the elevation ledger migration: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id, approver_user_id,
     requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, $3, 'approved', 'incident', 'R2.1b: the standing set', $1, $4, $5, 'approved', $5, $5, $6)`,
		userID, sessionRef, level, approverID, now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatalf("seed an active %s elevation: %v", level, err)
	}
}

func elevatedAdminEnvironment(t *testing.T) escalationEnvironment {
	t.Helper()
	env := newEscalationEnvironment(t)
	seedActiveElevation(t, env.pool, env.ids[escalationAdmin], env.ids[escalationSuper],
		elevatedSessionRef, adminaccess.ElevationLevelSuperAdmin)
	// The premise: the route gates' resolver gives the elevated session the
	// super_admin permission, and says it came from the elevation.
	id := env.ids[escalationAdmin]
	principal := auth.User{ID: strconv.Itoa(id), UserID: strconv.Itoa(id), CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: elevatedSessionRef}
	resolution, err := legacyrbac.NewPostgresResolver(env.pool).ResolvePermissions(
		context.Background(), principal, adminaccess.ModeAdministration, "")
	if err != nil || !slices.Contains(resolution.Permissions, adminaccess.SuperAdminPermission) ||
		!slices.Contains(resolution.ElevatedPermissions, adminaccess.SuperAdminPermission) {
		t.Fatalf("premise: the elevated session resolves %+v (%v), want %s from the elevation",
			resolution, err, adminaccess.SuperAdminPermission)
	}
	return env
}

func TestASuperAdminLevelElevationDoesNotExemptItsHolderFromNotYourself(t *testing.T) {
	env := elevatedAdminEnvironment(t)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	expectRefusal(t, adminDo(t, router, http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": env.ids[escalationAdmin], "role_name": "editor"}),
		adminaccess.CodeSelfAssignment)
	expectRoles(t, env, escalationAdmin, "admin")
}

func TestASuperAdminLevelElevationDoesNotUnlockTheRoleItsHolderHolds(t *testing.T) {
	env := elevatedAdminEnvironment(t)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	expectRefusal(t, saveCell(t, router, centralMatrix, "admin.auth.users", "admin", false),
		adminaccess.CodeOwnRoleLocked)
	if !contains(grantsSQL(t, env.pool, "administration", "admin"), "admin.auth.users") {
		t.Fatal("the refused revoke was written")
	}
}

func TestASuperAdminLevelElevationDoesNotGiveTheSuperAdminRole(t *testing.T) {
	env := elevatedAdminEnvironment(t)
	router := env.asSession(escalationAdmin, elevatedSessionRef)
	plain := env.ids[escalationPlainTarget]
	expectRefusal(t, adminDo(t, router, http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "super_admin"}),
		adminaccess.CodeSuperAdminRequired)
	if names := adminRoleNamesSQL(t, env.pool, plain); len(names) != 0 {
		t.Fatalf("the plain target holds %v after the refusal", names)
	}
}
