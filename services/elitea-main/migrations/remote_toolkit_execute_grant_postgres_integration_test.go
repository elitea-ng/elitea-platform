package migrations_test

// shared/0156_remote_toolkit_execute_permission.sql against a clean database,
// in both directions, and its per-project override block.
//
// The permission gates executeRemoteToolkitTool (ADR-0029 decision 5b). It is
// read from the production constant, so a rename cannot leave this measuring a
// string nothing uses.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
)

const remoteToolkitExecutePermission = desktopops.RemoteToolkitPermission

// The holders are the roles that may chat: the holders of
// `models.chat.messages.create` (0070), and no machine role.
func TestCleanDatabaseGrantsRemoteToolkitExecuteToTheChatRoles(t *testing.T) {
	pool := newMigratedPool(t)
	if holders := defaultModeGrantHolders(t, pool, remoteToolkitExecutePermission); !slices.Equal(holders, defaultModeRoles) {
		t.Fatalf("default-mode holders of %s = %v, want %v", remoteToolkitExecutePermission, holders, defaultModeRoles)
	}
	if chat := defaultModeGrantHolders(t, pool, "models.chat.messages.create"); !slices.Equal(chat, defaultModeRoles) {
		t.Fatalf("the chat send's holders moved (%v); re-derive this grant's role split from them", chat)
	}
}

// A viewer of a Go-provisioned project resolves it (the entitled direction).
func TestAViewerResolvesRemoteToolkitExecuteOnACleanDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 15601, "viewer", 1)
	resolution := resolveDefaultModeFor(t, pool, "1", "1")
	if !slices.Contains(resolution.Permissions, remoteToolkitExecutePermission) {
		t.Fatalf("a viewer does not resolve %s (resolved %v)", remoteToolkitExecutePermission, resolution.Permissions)
	}
}

// The override block: a project role that already carries per-project rows
// (the shape the admin console writes) gets the string by name, a role outside
// the split does not, and a project role with no rows is left on the central
// fallback.
func TestTheOverrideBlockReachesProjectsWithSavedMatrices(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	viewerRole := seedRoleMembership(t, pool, 15602, "viewer", 1)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'auditor')
ON CONFLICT (project_id, name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var auditorRole int
	if err := pool.QueryRow(ctx, `SELECT id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'auditor'`).Scan(&auditorRole); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.chat.messages.create'), (1, $2, 'models.chat.messages.create')`, viewerRole, auditorRole); err != nil {
		t.Fatal(err)
	}
	// The suppression this block exists for: the saved row hides the central
	// grant from the viewer until the migration delivers it.
	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); slices.Contains(resolution.Permissions, remoteToolkitExecutePermission) {
		t.Fatalf("the fixture does not suppress the central fallback: %v", resolution.Permissions)
	}

	body, err := os.ReadFile(filepath.Join("shared", "0156_remote_toolkit_execute_permission.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-run 0156 over the saved matrix: %v", err)
	}
	// Idempotent.
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-run 0156 a second time: %v", err)
	}

	if resolution := resolveDefaultModeFor(t, pool, "1", "1"); !slices.Contains(resolution.Permissions, remoteToolkitExecutePermission) {
		t.Fatalf("a viewer with a saved matrix does not resolve %s after 0156 (resolved %v)",
			remoteToolkitExecutePermission, resolution.Permissions)
	}
	var auditorRows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM public.auth_core__project_role_permission
WHERE role_id = $1 AND permission = $2`, auditorRole, remoteToolkitExecutePermission).Scan(&auditorRows); err != nil {
		t.Fatal(err)
	}
	if auditorRows != 0 {
		t.Fatalf("a role outside admin/editor/viewer received %s", remoteToolkitExecutePermission)
	}
}

// The grant follows the chat send, not the role name: a role that does not
// hold `models.chat.messages.create` (centrally, or in a project's saved
// matrix) is not handed the tools a chat turn would run.
func TestTheGrantFollowsTheChatSendNotTheRoleName(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := os.ReadFile(filepath.Join("shared", "0156_remote_toolkit_execute_permission.sql"))
	if err != nil {
		t.Fatal(err)
	}

	// Central: an operator took the chat send away from the default viewer.
	if _, err := pool.Exec(ctx, `
DELETE FROM public.auth_core__role_permission AS grant_row
USING public.auth_core__role AS role
WHERE grant_row.role_id = role.id AND role.mode = 'default' AND role.name = 'viewer'
  AND grant_row.permission IN ('models.chat.messages.create', $1)`, remoteToolkitExecutePermission); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-run 0156: %v", err)
	}
	if holders := defaultModeGrantHolders(t, pool, remoteToolkitExecutePermission); slices.Contains(holders, "viewer") {
		t.Fatalf("a viewer without the chat send was granted %s centrally (holders %v)", remoteToolkitExecutePermission, holders)
	}

	// Per project: a saved matrix that gives the viewer something, but not
	// the chat send.
	viewerRole := seedRoleMembership(t, pool, 15604, "viewer", 1)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.applications.version.details')`, viewerRole); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-run 0156 over the saved matrix: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM public.auth_core__project_role_permission
WHERE project_id = 1 AND role_id = $1 AND permission = $2`, viewerRole, remoteToolkitExecutePermission).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a project viewer whose matrix withholds the chat send received %s", remoteToolkitExecutePermission)
	}
}
