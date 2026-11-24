package admin_test

// The admin.access.changed events of the estate-wide permission pushes and
// the role rename (ADR-0059 R1.5 follow-up, issue 2994): one summary
// project_permissions_pushed event per role a push changed, one role_renamed
// event per rename, each in the write's transaction, and a failed append
// fails the write.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// eventsOfKind keeps the events of one kind.
func eventsOfKind(events []accessEventRow, kind string) []accessEventRow {
	var out []accessEventRow
	for _, event := range events {
		if event.Access.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func TestARoleRenameAppendsOneRoleRenamedEvent(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)

	expectStatus(t, adminDo(t, router, http.MethodPost, "/r/admin/roles/administration/administration",
		map[string]any{"name": "auditors"}), http.StatusCreated)
	expectStatus(t, adminDo(t, router, http.MethodPut, "/r/admin/roles/administration/administration",
		map[string]any{"name": "auditors", "new_name": "reviewers"}), http.StatusOK)
	renamed := eventsOfKind(accessEvents(t, env.pool), "role_renamed")
	if len(renamed) != 1 {
		t.Fatalf("role_renamed events = %+v, want one", renamed)
	}
	event := renamed[0]
	if event.Action != "admin.access.role_renamed" || event.Subject != "role/administration:reviewers" ||
		event.Partition != "0" || event.Principal != fmt.Sprintf("user:%d", env.ids[escalationSuper]) ||
		setOf(event.Access.Before...) != "auditors" || setOf(event.Access.After...) != "reviewers" ||
		event.Access.Mode != "administration" || len(event.Access.Fields) != 0 {
		t.Fatalf("event = %+v", event)
	}

	// A rename to the same name changes nothing and appends nothing.
	expectStatus(t, adminDo(t, router, http.MethodPut, "/r/admin/roles/administration/administration",
		map[string]any{"name": "reviewers", "new_name": "reviewers"}), http.StatusOK)
	if got := eventsOfKind(accessEvents(t, env.pool), "role_renamed"); len(got) != 1 {
		t.Fatalf("role_renamed events = %+v after a same-name rename, want still one", got)
	}
	requireChainsVerify(t, env.pool, 0)
}

// A central `default` role is renamed with its project copies; the event
// says so.
func TestADefaultRoleRenameRecordsTheProjectCopies(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)
	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, $2)`, sharedProjectID, newRole); err != nil {
		t.Fatal(err)
	}
	if recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "default"),
		map[string]any{"name": newRole, "new_name": "risk_reviewer"}); recorder.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", recorder.Code, recorder.Body.String())
	}
	renamed := eventsOfKind(accessEvents(t, pool), "role_renamed")
	if len(renamed) != 1 || renamed[0].Subject != "role/default:risk_reviewer" ||
		strings.Join(renamed[0].Access.Fields, ",") != "project_role_copies" {
		t.Fatalf("role_renamed events = %+v, want one naming the project copies", renamed)
	}
	requireChainsVerify(t, pool, 0)
}

// Apply to Projects appends one summary event per role whose project grants
// it changed, not one per project; a second sync that changes nothing appends
// none.
func TestApplyToProjectsAppendsOneSummaryEventPerRole(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)
	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d", recorder.Code)
	}
	before := readMatrix(t, matrix, "administration", "default")
	if recorder := adminDo(t, matrix, http.MethodPut, "/admin/permissions/administration/default",
		setCell(t, before, "models.alpha.view", newRole, true)); recorder.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := adminDo(t, matrix, http.MethodPost, "/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("sync = %d: %s", recorder.Code, recorder.Body.String())
	}
	pushed := eventsOfKind(accessEvents(t, pool), "project_permissions_pushed")
	var ours []accessEventRow
	for _, event := range pushed {
		if event.Partition != "0" || event.Access.Mode != "default" || !strings.HasPrefix(event.Subject, "project_roles/team:") {
			t.Fatalf("push event = %+v, want the platform partition, mode default and a team subject", event)
		}
		if event.Subject == "project_roles/team:"+newRole {
			ours = append(ours, event)
		}
	}
	// The role reached three shared projects; one event names it.
	if len(ours) != 1 || setOf(ours[0].Access.After...) != "models.alpha.view" || len(ours[0].Access.Before) != 0 {
		t.Fatalf("events of %s = %+v, want one adding models.alpha.view", newRole, ours)
	}

	count := len(pushed)
	if recorder := adminDo(t, matrix, http.MethodPost, "/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("second sync = %d", recorder.Code)
	}
	if got := eventsOfKind(accessEvents(t, pool), "project_permissions_pushed"); len(got) != count {
		t.Fatalf("a sync that changed nothing appended %d events", len(got)-count)
	}
	requireChainsVerify(t, pool, 0)
}

// The personal-project matrix save appends one summary event per role it
// changed, with what it removed and what it added.
func TestAPersonalProjectPushAppendsOneSummaryEventPerRole(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)
	recorder := adminDo(t, router, http.MethodPut, "/admin/user_project_permissions/administration?append_user_role",
		map[string][]string{"editor": {"models.beta.view"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	pushed := eventsOfKind(accessEvents(t, pool), "project_permissions_pushed")
	if len(pushed) != 1 {
		t.Fatalf("push events = %+v, want one for the one role", pushed)
	}
	event := pushed[0]
	// The second personal project's editor held models.alpha.view, which the
	// role-map body replaces.
	if event.Subject != "project_roles/personal:editor" || event.Partition != "0" || event.Access.Mode != "" ||
		setOf(event.Access.Before...) != "models.alpha.view" || setOf(event.Access.After...) != "models.beta.view" ||
		strings.Join(event.Access.Fields, ",") != "roles_assigned" {
		t.Fatalf("event = %+v", event)
	}
	requireChainsVerify(t, pool, 0)
}

// A failed append fails the push and the rename: nothing moves.
func TestAFailedAppendRollsThePushAndTheRenameBack(t *testing.T) {
	pool, _ := newUserProjectPermissionsEnvironment(t)
	failing := admin.NewHandler(pool, admin.WithPermissionResolver(superAdminResolver()),
		admin.WithAccessAuditAppender(securityaudit.FailingAppender{Err: errors.New("injected append failure")}))
	router := userProjectPermissionsRouter(failing)
	beforeGrants := storedProjectGrants(t, pool, secondPersonalProjectID, "editor")
	if recorder := adminDo(t, router, http.MethodPut, "/admin/user_project_permissions/administration",
		map[string][]string{"editor": {"models.beta.view"}}); recorder.Code == http.StatusOK {
		t.Fatalf("a push answered 200 with a failed append: %s", recorder.Body.String())
	}
	if got := storedProjectGrants(t, pool, secondPersonalProjectID, "editor"); setOf(got...) != setOf(beforeGrants...) {
		t.Fatalf("the push moved after a failed append: %v became %v", beforeGrants, got)
	}

	writes := roleWriteRouter(failing, nil, nil)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('auditors', 'administration')`); err != nil {
		t.Fatal(err)
	}
	if recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "administration"),
		map[string]any{"name": "auditors", "new_name": "reviewers"}); recorder.Code == http.StatusOK {
		t.Fatalf("a rename answered 200 with a failed append: %s", recorder.Body.String())
	}
	if !centralRoleExists(t, pool, "administration", "auditors") {
		t.Fatal("the rename survived a failed append")
	}
	if events := accessEvents(t, pool); len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}
