package admin_test

// The built-in roles are locked (ADR-0059 decision 3, story R0.7, issue 2496):
// the roles API refuses to rename, delete or edit the set of a role with a row
// in elitea_auth.admin_role_seeds, for a global administrator too. A role of
// the same kind of name without a row is an ordinary custom role.
//
// A rename or a delete answers the 409 the roles API gives a code-named role;
// the edit of a built-in set answers 403 with the stable code
// `role_set_locked` (story R0.8a), through the refusal writer of the role
// policy, so its audit row carries the action role.escalation_refused.

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// adminRoleSeedsDDL is the CREATE SCHEMA and CREATE TABLE of shared 0263, read
// from the migration so the fixture cannot drift from it.
func adminRoleSeedsDDL(t *testing.T) string {
	t.Helper()
	const file = "shared/0263_builtin_admin_roles.sql"
	body, err := fs.ReadFile(migrations.Files, file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	ddl := seedTableDDLPattern.FindString(string(body))
	if ddl == "" {
		t.Fatalf("%s has no seed-table DDL", file)
	}
	return ddl
}

// seedTableDDLPattern matches the CREATE SCHEMA and the CREATE TABLE up to the
// first line that closes the column list, whatever its indentation.
var seedTableDDLPattern = regexp.MustCompile(
	`(?s)CREATE SCHEMA IF NOT EXISTS elitea_auth;\s*CREATE TABLE IF NOT EXISTS elitea_auth\.admin_role_seeds \(.*?\n\s*\);`)

// seedBuiltIn creates the administration role if absent and gives it a seed row.
func seedBuiltIn(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration') ON CONFLICT DO NOTHING`,
		`INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
		 SELECT id, name, mode, 'declared', 'test' FROM public.auth_core__role
		 WHERE mode = 'administration' AND name = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, name); err != nil {
			t.Fatalf("seed the built-in %s: %v", name, err)
		}
	}
}

func createCustom(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration')`, name); err != nil {
		t.Fatalf("create the custom %s: %v", name, err)
	}
}

func roleExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.auth_core__role
		WHERE mode = 'administration' AND name = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("read role %s: %v", name, err)
	}
	return exists
}

const centralRoles = "/r/admin/roles/administration/administration"

func TestABuiltInRoleCannotBeRenamed(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "llm_manager")
	recorder := adminDo(t, env.as(escalationSuper), http.MethodPut, centralRoles,
		map[string]string{"name": "llm_manager", "new_name": "llm_ops"})
	expectStatus(t, recorder, http.StatusConflict)
	if !strings.Contains(recorder.Body.String(), "built-in") {
		t.Errorf("the refusal does not say why: %s", recorder.Body.String())
	}
	if !roleExists(t, env.pool, "llm_manager") || roleExists(t, env.pool, "llm_ops") {
		t.Fatal("the refused rename was written")
	}
}

func TestABuiltInRoleCannotBeDeleted(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "emergency_access")
	recorder := adminDo(t, env.as(escalationSuper), http.MethodDelete, centralRoles,
		map[string]string{"name": "emergency_access"})
	expectStatus(t, recorder, http.StatusConflict)
	if !roleExists(t, env.pool, "emergency_access") {
		t.Fatal("the refused delete was written")
	}
}

// A global administrator cannot edit a built-in set either, in either
// direction, and the refusal names the role.
func TestTheSetOfABuiltInRoleCannotBeEdited(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "admin")
	seedBuiltIn(t, env.pool, "editor")
	router := env.as(escalationSuper)

	for _, edit := range []struct {
		role, permission string
		value            bool
	}{
		{"editor", "models.alpha.view", true},
		{"admin", "models.beta.edit", false},
	} {
		before := grantsSQL(t, env.pool, "administration", edit.role)
		recorder := saveCell(t, router, centralMatrix, edit.permission, edit.role, edit.value)
		expectRefusal(t, recorder, adminaccess.CodeSetLocked)
		if !strings.Contains(recorder.Body.String(), edit.role) {
			t.Errorf("the refusal does not name %s: %s", edit.role, recorder.Body.String())
		}
		if after := grantsSQL(t, env.pool, "administration", edit.role); strings.Join(after, ",") != strings.Join(before, ",") {
			t.Errorf("%s changed from %v to %v after the refusal", edit.role, before, after)
		}
	}
}

// eventRecorder keeps every audit event the request middleware records.
type eventRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *eventRecorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// The locked-set refusal goes through the refusal writer of the role policy:
// behind the real audit middleware it writes exactly one row, and that row
// carries the action role.escalation_refused.
func TestALockedSetRefusalWritesOneAuditRow(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "editor")
	recorder := &eventRecorder{}
	// The middleware audits the /api/v2/admin/ surface, so the matrix routes
	// are mounted under /api/v2 behind it, with the real resolver.
	caller := fmt.Sprint(env.ids[escalationSuper])
	router := chi.NewRouter()
	router.Use(middleware.Audit(recorder))
	router.Mount("/api/v2", rolesRouter(
		admin.NewHandler(env.pool, admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(env.pool))),
		nil, &auth.User{ID: caller, UserID: caller}))

	const path = "/api/v2/admin/permissions/administration/administration"
	refused := saveCell(t, router, path, "models.alpha.view", "editor", true)
	expectRefusal(t, refused, adminaccess.CodeSetLocked)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.events) != 1 {
		t.Fatalf("the refused save wrote %d audit rows, want 1: %+v", len(recorder.events), recorder.events)
	}
	event := recorder.events[0]
	if event.Action != adminaccess.RefusalAuditAction || event.EntityType != "role" || event.EntityName != "editor" {
		t.Fatalf("audit row = action %q entity %s/%s, want %s on role editor",
			event.Action, event.EntityType, event.EntityName, adminaccess.RefusalAuditAction)
	}
	if event.StatusCode == nil || *event.StatusCode != http.StatusForbidden {
		t.Fatalf("audit row status = %v, want 403", event.StatusCode)
	}
}

// A role that uses a built-in name but has no seed row is a custom role: it
// can be edited, renamed and deleted.
func TestACustomRoleWithABuiltInNameIsNotLocked(t *testing.T) {
	env := newEscalationEnvironment(t)
	createCustom(t, env.pool, "security_auditor")
	router := env.as(escalationSuper)

	expectStatus(t, saveCell(t, router, centralMatrix, "models.alpha.view", "security_auditor", true), http.StatusOK)
	expectStatus(t, adminDo(t, router, http.MethodPut, centralRoles,
		map[string]string{"name": "security_auditor", "new_name": "auditors"}), http.StatusOK)
	// A grant is not a member: the renamed role still holds it and nobody
	// holds the role, so it can be deleted.
	expectStatus(t, adminDo(t, router, http.MethodDelete, centralRoles,
		map[string]string{"name": "auditors"}), http.StatusOK)
	if roleExists(t, env.pool, "auditors") || roleExists(t, env.pool, "security_auditor") {
		t.Fatal("the custom role is still there")
	}
}

// The escalation rules answer first: a caller they refuse keeps the stable
// 403 code, and the lock refuses only a caller who could otherwise edit.
func TestTheEscalationCodeComesBeforeTheLock(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "admin")
	expectRefusal(t, saveCell(t, env.as(escalationAdmin), centralMatrix, "admin.auth.users", "admin", false),
		adminaccess.CodeOwnRoleLocked)
}

// A database whose seed table is absent (0263 ran before the auth_core
// tables existed) records no built-in role: the writes are not refused for
// it, and do not fail.
func TestWithoutTheSeedTableNothingIsLocked(t *testing.T) {
	env := newEscalationEnvironment(t)
	createCustom(t, env.pool, "llm_manager")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := env.pool.Exec(ctx, `DROP TABLE elitea_auth.admin_role_seeds`); err != nil {
		t.Fatalf("drop the seed table: %v", err)
	}
	router := env.as(escalationSuper)
	expectStatus(t, saveCell(t, router, centralMatrix, "models.alpha.view", "llm_manager", true), http.StatusOK)
	expectStatus(t, adminDo(t, router, http.MethodDelete, centralRoles,
		map[string]string{"name": "llm_manager"}), http.StatusOK)
	// A code-named role is still refused by name.
	expectStatus(t, adminDo(t, router, http.MethodDelete, centralRoles,
		map[string]string{"name": "editor"}), http.StatusConflict)
}
