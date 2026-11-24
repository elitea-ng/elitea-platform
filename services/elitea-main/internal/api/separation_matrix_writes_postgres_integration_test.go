package api

// The separation matrix of the write paths (ADR-0059 R0.9b, #2587).
//
// testdata/separation_matrix.txt pins who passes the GATE of every
// administration route, against a router without a database. The writes that
// change who holds what decide after their gate, from the database: no
// escalation, not yourself, the bound role's permissions in the project, the
// configuration type a central settings write reaches. This test drives those
// writes through the production router, against a migrated database, with
// one seeded holder of each built-in administration role plus a synthetic
// holder of identity.sso.settings.manage alone, and pins every answer, with
// the refusal code, in testdata/separation_matrix_writes.txt.
//
// Every request starts from the same state: a case's `before` resets what the
// write touches, and the actors' own assignments are restored after each
// request. The cases run in file order; Apply to Projects is last because it
// rewrites every project's role grants.
//
// Regenerate the file with
//
//	UPDATE_GOLDEN=1 ELITEA_TEST_DATABASE_URL=... go test ./internal/api/ -run TestSeparationMatrixWritesMatchTheDatabase
//
// The assertions of the issue run over the file, without a database, in
// separation_matrix_writes_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// writesProject is the project of the project plane: newCredentialJourneyPool
// creates it, and no actor is a member of it unless a case makes it one.
const writesProject = 2

// writeActor is one column of the file: an account holding exactly one
// administration role.
type writeActor struct {
	name string // the column name
	role string // the administration role it holds
	id   int
}

func (a writeActor) email() string { return "sm-" + a.name + "@autotest.local" }

// writeActors are the six built-in roles and the synthetic column of the
// issue: a custom role holding identity.sso.settings.manage and nothing else.
var writeActors = []writeActor{
	{name: adminaccess.RoleSuperAdmin, role: adminaccess.RoleSuperAdmin, id: 9301},
	{name: adminaccess.RoleAdmin, role: adminaccess.RoleAdmin, id: 9302},
	{name: adminaccess.RoleLLMManager, role: adminaccess.RoleLLMManager, id: 9303},
	{name: adminaccess.RoleProjectAdministrator, role: adminaccess.RoleProjectAdministrator, id: 9304},
	{name: adminaccess.RoleSecurityAuditor, role: adminaccess.RoleSecurityAuditor, id: 9305},
	{name: adminaccess.RoleEmergencyAccess, role: adminaccess.RoleEmergencyAccess, id: 9306},
	{name: writesSSOSettingsOnly, role: writesSSOSettingsOnly, id: 9307},
}

const (
	writesSSOSettingsOnly = "sso_settings_only"
	// The second user of a row whose target is not the actor: no role
	// anywhere until a case gives it one.
	writesTargetID    = 9310
	writesTargetEmail = "sm-target@autotest.local"
	// A viewer member of writesProject, for the update and the removal.
	writesMemberID = 9311
)

type writesEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	routers map[string]http.Handler
}

func (e writesEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e writesEnv) id(sql string, args ...any) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		e.t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

// asSuper sends a setup request as super_admin, fails on a non-2xx and
// answers the body.
func (e writesEnv) asSuper(method, path, body string) []byte {
	e.t.Helper()
	recorder := serveWrite(e.routers[adminaccess.RoleSuperAdmin], method, path, body)
	if recorder.Code/100 != 2 {
		e.t.Fatalf("setup %s %s answered %d: %s", method, path, recorder.Code, recorder.Body.String())
	}
	return recorder.Body.Bytes()
}

// writeCase is one (method, route, action) row group: the same request for
// every actor.
type writeCase struct {
	method string
	route  string // the chi pattern, as the gate matrix names it
	action string // what the request does, naming its target; no spaces
	// path is the concrete request path; it may read state `before` left.
	path func(e writesEnv, a writeActor, state map[string]string) string
	// body is the request body; nil sends the state["body"] `before` left.
	body func(a writeActor) string
	// before resets what the write touches, before each actor's request.
	before func(e writesEnv, a writeActor, state map[string]string)
}

// matrixCell leaves in state["body"] the matrix save that sets one cell: the
// permission's whole row as the matrix serves it, with the role's column
// turned on, so no other column is revoked. An empty permission picks the
// first row whose column is off.
func matrixCell(path, permission string, role func(writeActor) string) func(writesEnv, writeActor, map[string]string) {
	return func(e writesEnv, a writeActor, state map[string]string) {
		e.t.Helper()
		var matrix struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(e.asSuper(http.MethodGet, path, ""), &matrix); err != nil {
			e.t.Fatalf("decode the matrix %s: %v", path, err)
		}
		column := role(a)
		pick := -1
		for i, row := range matrix.Rows {
			on, _ := row[column].(bool)
			name, _ := row["name"].(string)
			grantable := !adminaccess.SeededOnly(name) && name != adminaccess.SuperAdminPermission
			if (permission == "" && !on && grantable) || name == permission {
				pick = i
				break
			}
		}
		if pick < 0 && permission == "" && len(matrix.Rows) > 0 {
			pick = 0 // the column holds every row: the save turns one on again
		}
		if pick < 0 {
			e.t.Fatalf("the matrix %s has no row for %q", path, permission)
		}
		matrix.Rows[pick][column] = true
		encoded, _ := json.Marshal(matrix.Rows[pick : pick+1])
		state["body"] = string(encoded)
	}
}

const (
	centralMatrixPath  = adminAPI + "/permissions/administration/administration"
	templateMatrixPath = adminAPI + "/permissions/administration/default"
)

func fixedRole(name string) func(writeActor) string { return func(writeActor) string { return name } }

func ownRole(a writeActor) string { return a.role }

func staticPath(path string) func(writesEnv, writeActor, map[string]string) string {
	return func(writesEnv, writeActor, map[string]string) string { return path }
}

func staticBody(body string) func(writeActor) string {
	return func(writeActor) string { return body }
}

const (
	adminAPI  = "/api/v2/admin"
	configAPI = "/api/v2/configurations"
)

// resetTargetAdministration removes every administration role of the target.
func resetTargetAdministration(e writesEnv, _ writeActor, _ map[string]string) {
	e.exec(`DELETE FROM public.auth_core__user_role WHERE user_id = $1`, writesTargetID)
}

// resetProjectPlane gives writesProject its role grants back and leaves the
// target out of it and the member in it as a viewer.
func resetProjectPlane(e writesEnv, _ writeActor, _ map[string]string) {
	e.exec(`DELETE FROM public.auth_core__project_role_permission WHERE project_id = $1`, writesProject)
	e.exec(`
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
SELECT role.project_id, role.id, grant_row.permission
FROM public.auth_core__project_role role
JOIN (VALUES ('admin', 'configuration.users.users.create'), ('admin', 'configuration.users.users.edit'),
             ('admin', 'configuration.users.users.delete'), ('admin', 'models.secrets.view'),
             ('editor', 'models.secrets.view'), ('viewer', 'models.prompts.view'))
  AS grant_row(role_name, permission) ON grant_row.role_name = role.name
WHERE role.project_id = $1`, writesProject)
	e.exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = $1`, writesProject)
	e.exec(`
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT project_id, $2, id FROM public.auth_core__project_role WHERE project_id = $1 AND name = 'viewer'`,
		writesProject, writesMemberID)
}

func resetBindings(e writesEnv, _ writeActor, state map[string]string) {
	e.exec(`DELETE FROM elitea_auth.scim_group_bindings WHERE display_name LIKE 'sm-%'`)
	state["binding"] = fmt.Sprint(e.id(`
INSERT INTO elitea_auth.scim_group_bindings (display_name, project_id, role_name)
VALUES ('sm-existing', $1, 'viewer') RETURNING id`, writesProject))
}

func resetSCIMClients(e writesEnv, _ writeActor, state map[string]string) {
	e.exec(`DELETE FROM elitea_auth.scim_clients WHERE name LIKE 'sm-%'`)
	state["client"] = fmt.Sprint(e.id(`
INSERT INTO elitea_auth.scim_clients (name, auth_method, secret_hash, secret_hint)
VALUES ('sm-existing', 'bearer', repeat('a', 64), 'aaaa') RETURNING id`))
}

func identityProviderBody(displayName, issuer, extra string) string {
	return fmt.Sprintf(`{"kind":"oidc","display_name":%q%s,"oidc":{"issuer":%q,"client_id":"elitea",`+
		`"redirect_uri":"https://elitea.example.com/auth/oidc/callback"}}`, displayName, extra, issuer)
}

const writesProviderPath = adminAPI + "/identity_providers/administration/sm_oidc"

// resetIdentityProviders leaves one enabled OIDC provider, sm_oidc, written
// through the API as super_admin, and no sm_new.
func resetIdentityProviders(e writesEnv, _ writeActor, _ map[string]string) {
	e.exec(`DELETE FROM elitea_auth.identity_providers WHERE provider_key LIKE 'sm\_%'`)
	e.asSuper(http.MethodPut, writesProviderPath, writesProviderBody)
}

// writesProviderBody is sm_oidc as resetIdentityProviders leaves it.
var writesProviderBody = identityProviderBody("SM SSO", "https://idp.example.com", `,"enabled":true`)

// resetConfigurations leaves one cloud_executor row and one github row in
// writesProject, written by SQL, and no row a create left.
func resetConfigurations(e writesEnv, _ writeActor, state map[string]string) {
	e.exec(fmt.Sprintf(`DELETE FROM p_%d.configuration`, writesProject))
	for _, row := range []struct{ key, typeName, section, data string }{
		{"guarded", "cloud_executor", "cloud_executor", `{"enabled": false}`},
		{"credential", "github", "credentials", `{"base_url": "https://api.github.com"}`},
	} {
		state[row.key] = fmt.Sprint(e.id(fmt.Sprintf(`
INSERT INTO p_%d.configuration (project_id, label, elitea_title, type, section, data, shared, created_at)
VALUES ($1, $2, $2, $3, $4, $5::jsonb, false, now() AT TIME ZONE 'UTC') RETURNING id`, writesProject),
			writesProject, "sm-"+row.key, row.typeName, row.section, row.data))
	}
}

func selfOr(a writeActor, self bool) int {
	if self {
		return a.id
	}
	return writesTargetID
}

func setAdminRole(self bool, roles ...string) func(writeActor) string {
	return func(a writeActor) string {
		names := roles
		if self {
			names = append([]string{a.role}, roles...)
			slices.Sort(names)
			names = slices.Compact(names)
		}
		encoded, _ := json.Marshal(map[string]any{
			"action": "set_admin_role", "user_id": selfOr(a, self), "role_names": names,
		})
		return string(encoded)
	}
}

func memberCreate(self bool) func(writeActor) string {
	return func(a writeActor) string {
		email := writesTargetEmail
		if self {
			email = a.email()
		}
		return fmt.Sprintf(`{"emails":[%q],"roles":["admin"]}`, email)
	}
}

func memberUpdate(self bool) func(writeActor) string {
	return func(a writeActor) string {
		id := writesMemberID
		if self {
			id = a.id
		}
		return fmt.Sprintf(`{"userId":"%d","roles":["admin"]}`, id)
	}
}

func bulkInvite(self bool) func(writeActor) string {
	return func(a writeActor) string {
		return fmt.Sprintf(`{"users":[%d],"projects":[%d],"role":"admin"}`, selfOr(a, self), writesProject)
	}
}

func configurationCreate(typeName, section string) func(writeActor) string {
	return func(a writeActor) string {
		return fmt.Sprintf(`{"elitea_title":"sm-new-%s","label":"sm-new-%s","type":%q,"section":%q,"data":{}}`,
			a.name, a.name, typeName, section)
	}
}

func storedConfigurationPath(mode, key string) func(writesEnv, writeActor, map[string]string) string {
	return func(_ writesEnv, _ writeActor, state map[string]string) string {
		return fmt.Sprintf("%s/configuration/%s%d/%s", configAPI, mode, writesProject, state[key])
	}
}

func writeCases() []writeCase {
	project := fmt.Sprint(writesProject)
	membersCentral := adminAPI + "/users/administration/" + project
	membersMode := adminAPI + "/users/default/" + project
	cases := []writeCase{
		// The central assignment routes: the set form (R0.8b) and the mode role writes.
		{http.MethodPost, adminAPI + "/auth_users/{mode}", "set_admin_role:target:editor",
			staticPath(adminAPI + "/auth_users/administration"), setAdminRole(false, "editor"), resetTargetAdministration},
		{http.MethodPost, adminAPI + "/auth_users/{mode}", "set_admin_role:target:super_admin",
			staticPath(adminAPI + "/auth_users/administration"), setAdminRole(false, "super_admin"), resetTargetAdministration},
		{http.MethodPost, adminAPI + "/auth_users/{mode}", "set_admin_role:self:+super_admin",
			staticPath(adminAPI + "/auth_users/administration"), setAdminRole(true, "super_admin"), nil},
		{http.MethodPost, adminAPI + "/auth_users/{mode}", "set_admin_role:self:none",
			staticPath(adminAPI + "/auth_users/administration"),
			func(a writeActor) string {
				return fmt.Sprintf(`{"action":"set_admin_role","user_id":%d,"role_names":[]}`, a.id)
			}, nil},
		{http.MethodPost, adminAPI + "/modes/administration", "assign:target:editor",
			staticPath(adminAPI + "/modes/administration"),
			staticBody(fmt.Sprintf(`{"user_id":%d,"mode":"administration","role":"editor"}`, writesTargetID)),
			resetTargetAdministration},
		{http.MethodPost, adminAPI + "/modes/administration", "assign:self:super_admin",
			staticPath(adminAPI + "/modes/administration"),
			func(a writeActor) string {
				return fmt.Sprintf(`{"user_id":%d,"mode":"administration","role":"super_admin"}`, a.id)
			}, nil},
		{http.MethodDelete, adminAPI + "/modes/administration", "remove:target:editor",
			staticPath(fmt.Sprintf("%s/modes/administration?id=%d:administration:editor", adminAPI, writesTargetID)),
			staticBody(""),
			func(e writesEnv, a writeActor, state map[string]string) {
				resetTargetAdministration(e, a, state)
				e.exec(`INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = 'editor'`, writesTargetID)
			}},
		{http.MethodDelete, adminAPI + "/modes/administration", "remove:self:own_role",
			func(_ writesEnv, a writeActor, _ map[string]string) string {
				return fmt.Sprintf("%s/modes/administration?id=%d:administration:%s", adminAPI, a.id, a.role)
			}, staticBody(""), nil},

		// The Roles matrix: the central plane (the administration roles) and
		// the project plane (the default-mode template, Apply to Projects last).
		{http.MethodPut, adminAPI + "/permissions/{scope}/{mode}", "central_matrix:grant:sm_custom",
			staticPath(centralMatrixPath), nil,
			matrixCell(centralMatrixPath, adminaccess.ConfigurationRolesPermissionsView, fixedRole("sm_custom"))},
		{http.MethodPut, adminAPI + "/permissions/{scope}/{mode}", "central_matrix:grant:sm_custom:sso_trust_name",
			staticPath(centralMatrixPath), nil,
			matrixCell(centralMatrixPath, adminaccess.IdentitySSOTrustManage, fixedRole("sm_custom"))},
		{http.MethodPut, adminAPI + "/permissions/{scope}/{mode}", "central_matrix:grant:sm_custom:super_admin_name",
			staticPath(centralMatrixPath), nil,
			matrixCell(centralMatrixPath, adminaccess.SuperAdminPermission, fixedRole("sm_custom"))},
		{http.MethodPut, adminAPI + "/permissions/{scope}/{mode}", "central_matrix:grant:own_role",
			staticPath(centralMatrixPath), nil,
			matrixCell(centralMatrixPath, "", ownRole)},
		{http.MethodPut, adminAPI + "/permissions/{scope}/{mode}", "template:grant:viewer",
			staticPath(templateMatrixPath), nil,
			matrixCell(templateMatrixPath, "", fixedRole("viewer"))},

		// Role create, rename and delete.
		{http.MethodPost, adminAPI + "/roles/{scope}/{mode}", "create:administration",
			staticPath(adminAPI + "/roles/administration/administration"),
			func(a writeActor) string { return fmt.Sprintf(`{"name":"sm_new_%s"}`, a.id2()) }, nil},
		{http.MethodPost, adminAPI + "/roles/{scope}/{mode}", "create:template",
			staticPath(adminAPI + "/roles/administration/default"),
			func(a writeActor) string { return fmt.Sprintf(`{"name":"sm_new_%s"}`, a.id2()) }, nil},
		{http.MethodPut, adminAPI + "/roles/{scope}/{mode}", "rename:administration:sm_from",
			staticPath(adminAPI + "/roles/administration/administration"),
			staticBody(`{"name":"sm_from","new_name":"sm_to"}`),
			func(e writesEnv, _ writeActor, _ map[string]string) {
				e.exec(`DELETE FROM public.auth_core__role WHERE mode = 'administration' AND name IN ('sm_from', 'sm_to')`)
				e.exec(`INSERT INTO public.auth_core__role (name, mode) VALUES ('sm_from', 'administration')`)
			}},
		{http.MethodDelete, adminAPI + "/roles/{scope}/{mode}", "delete:administration:sm_gone",
			staticPath(adminAPI + "/roles/administration/administration"),
			staticBody(`{"name":"sm_gone"}`),
			func(e writesEnv, _ writeActor, _ map[string]string) {
				e.exec(`DELETE FROM public.auth_core__role WHERE mode = 'administration' AND name = 'sm_gone'`)
				e.exec(`INSERT INTO public.auth_core__role (name, mode) VALUES ('sm_gone', 'administration')`)
			}},

		// The project plane: the members (the central routes, the {mode}
		// routes and the central removal), the bulk invite, the project matrix.
		{http.MethodPost, adminAPI + "/users/administration/{projectID}", "add:target:admin",
			staticPath(membersCentral), memberCreate(false), resetProjectPlane},
		{http.MethodPost, adminAPI + "/users/administration/{projectID}", "add:self:admin",
			staticPath(membersCentral), memberCreate(true), resetProjectPlane},
		{http.MethodPut, adminAPI + "/users/administration/{projectID}", "update:member:admin",
			staticPath(membersCentral), memberUpdate(false), resetProjectPlane},
		{http.MethodPut, adminAPI + "/users/administration/{projectID}", "update:self:admin",
			staticPath(membersCentral), memberUpdate(true), resetProjectPlane},
		{http.MethodDelete, adminAPI + "/users/administration/{projectID}", "remove:member",
			staticPath(fmt.Sprintf("%s?id=%d", membersCentral, writesMemberID)), staticBody(""), resetProjectPlane},
		{http.MethodPost, adminAPI + "/users/{mode}/{projectID}", "add:target:admin",
			staticPath(membersMode), memberCreate(false), resetProjectPlane},
		{http.MethodPut, adminAPI + "/users/{mode}/{projectID}", "update:member:admin",
			staticPath(membersMode), memberUpdate(false), resetProjectPlane},
		{http.MethodDelete, adminAPI + "/users/{mode}/{projectID}", "remove:member",
			staticPath(fmt.Sprintf("%s?id=%d", membersMode, writesMemberID)), staticBody(""), resetProjectPlane},
		{http.MethodPost, adminAPI + "/invites_bulk/administration", "invite:target:admin",
			staticPath(adminAPI + "/invites_bulk/administration"), bulkInvite(false), resetProjectPlane},
		{http.MethodPost, adminAPI + "/invites_bulk/administration", "invite:self:admin",
			staticPath(adminAPI + "/invites_bulk/administration"), bulkInvite(true), resetProjectPlane},
		{http.MethodPut, adminAPI + "/user_project_permissions/administration", "project_matrix:team:viewer",
			staticPath(adminAPI + "/user_project_permissions/administration?team_projects=true"),
			staticBody(`{"viewer":["models.prompts.view","models.secrets.view"]}`), resetProjectPlane},
		{http.MethodPut, adminAPI + "/user_project_permissions/administration", "project_matrix:self_viewer:viewer",
			staticPath(adminAPI + "/user_project_permissions/administration?team_projects=true"),
			staticBody(`{"viewer":["models.prompts.view","models.secrets.view"]}`),
			func(e writesEnv, a writeActor, state map[string]string) {
				resetProjectPlane(e, a, state)
				e.exec(`
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT project_id, $2, id FROM public.auth_core__project_role WHERE project_id = $1 AND name = 'viewer'`,
					writesProject, a.id)
			}},

		// The SCIM group bindings and the SCIM clients.
		{http.MethodPost, adminAPI + "/scim_group_bindings/administration", "bind:project:admin",
			staticPath(adminAPI + "/scim_group_bindings/administration"),
			func(a writeActor) string {
				return fmt.Sprintf(`{"display_name":"sm-%s","project_id":%d,"role_name":"admin"}`, a.name, writesProject)
			},
			func(e writesEnv, a writeActor, state map[string]string) {
				resetProjectPlane(e, a, state)
				resetBindings(e, a, state)
			}},
		{http.MethodPut, adminAPI + "/scim_group_bindings/administration/{id}", "rebind:project:viewer->admin",
			func(_ writesEnv, _ writeActor, state map[string]string) string {
				return adminAPI + "/scim_group_bindings/administration/" + state["binding"]
			},
			staticBody(fmt.Sprintf(`{"display_name":"sm-existing","project_id":%d,"role_name":"admin"}`, writesProject)),
			func(e writesEnv, a writeActor, state map[string]string) {
				resetProjectPlane(e, a, state)
				resetBindings(e, a, state)
			}},
		{http.MethodDelete, adminAPI + "/scim_group_bindings/administration/{id}", "unbind:project:viewer",
			func(_ writesEnv, _ writeActor, state map[string]string) string {
				return adminAPI + "/scim_group_bindings/administration/" + state["binding"]
			}, staticBody(""),
			func(e writesEnv, a writeActor, state map[string]string) {
				resetProjectPlane(e, a, state)
				resetBindings(e, a, state)
			}},
		{http.MethodPost, adminAPI + "/scim_clients/administration", "trust:create",
			staticPath(adminAPI + "/scim_clients/administration"),
			func(a writeActor) string { return fmt.Sprintf(`{"name":"sm-%s","auth_method":"bearer"}`, a.name) },
			resetSCIMClients},
		{http.MethodPost, adminAPI + "/scim_clients/administration/{id}/rotate", "trust:rotate",
			func(_ writesEnv, _ writeActor, state map[string]string) string {
				return adminAPI + "/scim_clients/administration/" + state["client"] + "/rotate"
			}, staticBody(""), resetSCIMClients},

		// The identity provider routes, one row per action (R0.4c).
		{http.MethodPut, adminAPI + "/identity_providers/administration/{key}", "trust:create",
			staticPath(adminAPI + "/identity_providers/administration/sm_new"),
			staticBody(identityProviderBody("SM new", "https://new.example.com", "")), resetIdentityProviders},
		{http.MethodPut, adminAPI + "/identity_providers/administration/{key}", "settings:rename",
			staticPath(writesProviderPath),
			staticBody(identityProviderBody("SM SSO renamed", "https://idp.example.com", "")), resetIdentityProviders},
		{http.MethodPut, adminAPI + "/identity_providers/administration/{key}", "trust:disable",
			staticPath(writesProviderPath),
			staticBody(identityProviderBody("SM SSO", "https://idp.example.com", `,"enabled":false`)), resetIdentityProviders},
		{http.MethodPut, adminAPI + "/identity_providers/administration/{key}", "trust:issuer",
			staticPath(writesProviderPath),
			staticBody(identityProviderBody("SM SSO", "https://other.example.com", "")), resetIdentityProviders},
		{http.MethodPut, adminAPI + "/identity_providers/administration/{key}", "trust:secret",
			staticPath(writesProviderPath),
			staticBody(identityProviderBody("SM SSO", "https://idp.example.com", `,"secret":"sm-secret"`)), resetIdentityProviders},
		{http.MethodDelete, adminAPI + "/identity_providers/administration/{key}", "trust:delete",
			staticPath(writesProviderPath), staticBody(""), resetIdentityProviders},
		{http.MethodPost, adminAPI + "/identity_providers/administration/{key}/rebind", "trust:rebind",
			staticPath(writesProviderPath + "/rebind"), staticBody(`{"from_unbound":true}`), resetIdentityProviders},
	}

	// The central-override pairs (R0.6b) of a project the actor is not a
	// member of: a guarded setting type, and a credential, a model and a
	// toolkit type the override never reaches.
	for _, mode := range []string{"", "default/"} {
		pattern := func(path string) string {
			if mode == "" {
				return configAPI + path
			}
			return configAPI + strings.Replace(path, "{projectID}", "{mode}/{projectID}", 1)
		}
		list := fmt.Sprintf("%s/configurations/%s%d", configAPI, mode, writesProject)
		for _, typeName := range []string{"cloud_executor", "github"} {
			cases = append(cases, writeCase{http.MethodGet, pattern("/configurations/{projectID}"), "list:type=" + typeName,
				staticPath(list + "?type=" + typeName), staticBody(""), resetConfigurations})
		}
		for _, created := range []struct{ typeName, section string }{
			{"cloud_executor", "cloud_executor"}, {"github", "credentials"}, {"open_ai", "ai_credentials"},
			{"llm_model", "llm"}, {"jira", "credentials"},
		} {
			cases = append(cases, writeCase{http.MethodPost, pattern("/configurations/{projectID}"),
				"create:type=" + created.typeName, staticPath(list),
				configurationCreate(created.typeName, created.section), resetConfigurations})
		}
		for _, stored := range []struct{ key, typeName string }{{"guarded", "cloud_executor"}, {"credential", "github"}} {
			cases = append(cases,
				writeCase{http.MethodGet, pattern("/configuration/{projectID}/{configID}"), "read:stored=" + stored.typeName,
					storedConfigurationPath(mode, stored.key), staticBody(""), resetConfigurations},
				writeCase{http.MethodPut, pattern("/configuration/{projectID}/{configID}"), "update:stored=" + stored.typeName,
					storedConfigurationPath(mode, stored.key),
					staticBody(`{"label":"sm-updated","data":{}}`), resetConfigurations},
				writeCase{http.MethodDelete, pattern("/configuration/{projectID}/{configID}"), "delete:stored=" + stored.typeName,
					storedConfigurationPath(mode, stored.key), staticBody(""), resetConfigurations})
		}
	}

	// Apply to Projects rewrites every project's grants from the template,
	// so it runs last.
	return append(cases, writeCase{http.MethodPost, adminAPI + "/permissions/{scope}/{mode}", "template:apply_to_projects",
		staticPath(adminAPI + "/permissions/administration/default"), staticBody(""), resetProjectPlane})
}

// id2 is a name-safe suffix for a role an actor creates.
func (a writeActor) id2() string { return fmt.Sprint(a.id) }

// seedWritesFixture writes the actors, the target and the member, the custom
// administration roles and writesProject's roles.
func seedWritesFixture(e writesEnv) {
	e.t.Helper()
	e.exec(`INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration'), ('sm_custom', 'administration')`,
		writesSSOSettingsOnly)
	e.exec(`INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, $2 FROM public.auth_core__role WHERE mode = 'administration' AND name = $1`,
		writesSSOSettingsOnly, adminaccess.IdentitySSOSettingsManage)
	for _, a := range writeActors {
		e.exec(`INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $3)`, a.id, a.email(), a.name)
	}
	e.exec(`INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, 'Target'), ($3, 'sm-member@autotest.local', 'Member')`,
		writesTargetID, writesTargetEmail, writesMemberID)
	e.exec(`
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT $1, role_name FROM (VALUES ('admin'), ('editor'), ('viewer'), ('system')) AS roles(role_name)
ON CONFLICT (project_id, name) DO NOTHING`, writesProject)
	e.exec(`CREATE TABLE public.sm_role_permission_snapshot AS
SELECT role_id, permission FROM public.auth_core__role_permission`)
	restoreActors(e)
	// The premise: every built-in role resolves its registry set, or the
	// file would measure the fixture.
	for _, a := range writeActors[:6] {
		role, _ := adminaccess.LookupBuiltInRole(a.role)
		var held int
		if err := e.pool.QueryRow(context.Background(), `
SELECT count(*) FROM public.auth_core__role_permission grant_row
JOIN public.auth_core__role role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND role.name = $1 AND grant_row.permission = ANY($2)`,
			a.role, role.Set()).Scan(&held); err != nil || held != len(role.Set()) {
			e.t.Fatalf("premise: %s holds %d of its %d registry names (%v)", a.role, held, len(role.Set()), err)
		}
	}
}

// restoreActors gives every actor exactly its one administration role, and
// takes away any project role a case gave it.
func restoreActors(e writesEnv) {
	ids := make([]int, 0, len(writeActors))
	for _, a := range writeActors {
		ids = append(ids, a.id)
	}
	e.exec(`DELETE FROM public.auth_core__user_role WHERE user_id = ANY($1)`, ids)
	e.exec(`DELETE FROM public.auth_core__project_user_role WHERE user_id = ANY($1)`, ids)
	for _, a := range writeActors {
		e.exec(`INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = $2`, a.id, a.role)
	}
	// A matrix case may have changed a role's grants: put back the grants
	// the fixture started from, in every mode.
	e.exec(`DELETE FROM public.auth_core__role_permission grant_row WHERE NOT EXISTS (
SELECT 1 FROM public.sm_role_permission_snapshot snapshot
WHERE snapshot.role_id = grant_row.role_id AND snapshot.permission = grant_row.permission)`)
	e.exec(`INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT snapshot.role_id, snapshot.permission FROM public.sm_role_permission_snapshot snapshot
WHERE EXISTS (SELECT 1 FROM public.auth_core__role role WHERE role.id = snapshot.role_id)
AND NOT EXISTS (SELECT 1 FROM public.auth_core__role_permission grant_row
WHERE grant_row.role_id = snapshot.role_id AND grant_row.permission = snapshot.permission)`)
}

func serveWrite(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// writeAnswer is the file's answer for one response: "allowed <status>", or
// "refused <code>" (the status when the refusal carries no code). 401, 403
// and the maintenance window's 503 are refusals.
func writeAnswer(recorder *httptest.ResponseRecorder) string {
	switch recorder.Code {
	case http.StatusUnauthorized, http.StatusForbidden:
	default:
		return fmt.Sprintf("allowed %d", recorder.Code)
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &body) == nil && body.Code != "" {
		return "refused " + body.Code
	}
	return fmt.Sprintf("refused %d", recorder.Code)
}

func TestSeparationMatrixWritesMatchTheDatabase(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	env := writesEnv{t: t, pool: pool, routers: map[string]http.Handler{}}
	seedWritesFixture(env)
	for _, a := range writeActors {
		user := auth.User{ID: fmt.Sprint(a.id), UserID: fmt.Sprint(a.id), Email: a.email()}
		env.routers[a.name] = asConsoleSession(NewRouter(RouterConfig{
			Pool:               pool,
			SkillsRepo:         struct{ v2skills.Repository }{},
			SessionSecret:      consoleTestSessionSecret,
			AuthValidator:      testTokenValidator{user: user},
			PrincipalValidator: testPrincipalValidator{},
		}), user)
	}

	var got []writesRow
	for _, c := range writeCases() {
		row := writesRow{Method: c.method, Route: c.route, Action: c.action, Answers: map[string]string{}}
		for _, a := range writeActors {
			state := map[string]string{}
			if c.before != nil {
				c.before(env, a, state)
			}
			body := state["body"]
			if c.body != nil {
				body = c.body(a)
			}
			recorder := serveWrite(env.routers[a.name], c.method, c.path(env, a, state), body)
			row.Answers[a.name] = writeAnswer(recorder)
			if recorder.Code >= 400 {
				t.Logf("%s as %s: %d %s", row.key(), a.name, recorder.Code, strings.TrimSpace(recorder.Body.String()))
			}
			restoreActors(env)
		}
		got = append(got, row)
	}

	rendered := renderWritesMatrix(got)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(separationMatrixWritesFile, []byte(rendered), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(separationMatrixWritesFile)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", separationMatrixWritesFile, err)
	}
	if string(want) != rendered {
		wantRows, _ := parseWritesMatrix(string(want))
		for _, diff := range diffWritesMatrix(wantRows, got) {
			t.Error(diff)
		}
		t.Errorf("%s differs from the database answers; regenerate with UPDATE_GOLDEN=1 and review every changed line",
			separationMatrixWritesFile)
	}
}
