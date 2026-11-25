package api

// The file of the write-path separation matrix (ADR-0059 R0.9b, #2587) and
// the assertions the issue makes over it. The file is written by
// TestSeparationMatrixWritesMatchTheDatabase against a migrated database;
// these tests read it without one, so they run in every shard.

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const separationMatrixWritesFile = "testdata/separation_matrix_writes.txt"

const separationMatrixWritesHeader = `# The separation matrix of the write paths (ADR-0059 R0.9b, #2587).
#
# Generated; do not edit by hand. Regenerate against a database with
#   UPDATE_GOLDEN=1 ELITEA_TEST_DATABASE_URL=... go test ./internal/api/ -run TestSeparationMatrixWritesMatchTheDatabase
# and review every changed line: it is a write whose answer for a role changed.
#
# testdata/separation_matrix.txt pins the GATE of each route. This file pins
# what the write itself answers, through the production router and the real
# permission resolver, against a migrated database: the rules that decide
# after the gate (no escalation, not yourself, the bound role's permissions in
# the project, the configuration type of a central settings write).
#
# A block per write: the method, the route and the action, then one line per
# actor. The action names what the write does and its target: "self" is the
# actor itself, "target" a second account with no role, "member" a viewer of
# project 2. No actor is a member of project 2 unless the action says so
# ("self_viewer"). Each actor holds exactly one administration role: the six
# built-in roles, and sso_settings_only, a custom role holding
# identity.sso.settings.manage alone.
#
# The answers:
#   allowed N   no authorization refusal; N is the status. The write may still
#               be refused for another reason (400, 404, 409, 503).
#   refused X   a 401 or 403; X is the refusal code, or the status when the
#               refusal carries none (a route gate).
`

// writesRow is one block of the file.
type writesRow struct {
	Method, Route, Action string
	Answers               map[string]string // actor name → answer
}

func (r writesRow) key() string { return r.Method + " " + r.Route + " " + r.Action }

func renderWritesMatrix(rows []writesRow) string {
	var out strings.Builder
	out.WriteString(separationMatrixWritesHeader)
	for _, row := range rows {
		fmt.Fprintf(&out, "\n%s\n", row.key())
		for _, a := range writeActors {
			fmt.Fprintf(&out, "  %-22s %s\n", a.name, row.Answers[a.name])
		}
	}
	return out.String()
}

func parseWritesMatrix(text string) ([]writesRow, error) {
	var rows []writesRow
	for n, line := range strings.Split(text, "\n") {
		switch {
		case strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "  "):
			if len(rows) == 0 {
				return nil, fmt.Errorf("line %d: an answer before any write: %q", n+1, line)
			}
			fields := strings.Fields(line)
			if len(fields) != 3 || (fields[1] != "allowed" && fields[1] != "refused") {
				return nil, fmt.Errorf("line %d: want an actor, allowed or refused, and a status or code: %q", n+1, line)
			}
			rows[len(rows)-1].Answers[fields[0]] = fields[1] + " " + fields[2]
		default:
			fields := strings.Fields(line)
			if len(fields) != 3 {
				return nil, fmt.Errorf("line %d: want a method, a route and an action: %q", n+1, line)
			}
			rows = append(rows, writesRow{Method: fields[0], Route: fields[1], Action: fields[2], Answers: map[string]string{}})
		}
	}
	return rows, nil
}

func diffWritesMatrix(want, got []writesRow) []string {
	index := map[string]writesRow{}
	for _, row := range want {
		index[row.key()] = row
	}
	var diffs []string
	for _, row := range got {
		pinned, ok := index[row.key()]
		if !ok {
			diffs = append(diffs, row.key()+": not in the file")
			continue
		}
		delete(index, row.key())
		for _, a := range writeActors {
			if pinned.Answers[a.name] != row.Answers[a.name] {
				diffs = append(diffs, fmt.Sprintf("%s: %s answers %q, the file says %q",
					row.key(), a.name, row.Answers[a.name], pinned.Answers[a.name]))
			}
		}
	}
	for key := range index {
		diffs = append(diffs, key+": in the file, not driven")
	}
	return diffs
}

func readWritesMatrix(t *testing.T) []writesRow {
	t.Helper()
	raw, err := os.ReadFile(separationMatrixWritesFile)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseWritesMatrix(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), separationMatrixWritesHeader) {
		t.Errorf("%s does not start with the header; regenerate it", separationMatrixWritesFile)
	}
	return rows
}

// The file has a block for every write the test drives, and a line for every
// actor of each block.
func TestSeparationMatrixWritesFileCoversEveryWrite(t *testing.T) {
	rows := readWritesMatrix(t)
	keys := map[string]bool{}
	for _, row := range rows {
		if keys[row.key()] {
			t.Errorf("%s: twice in the file", row.key())
		}
		keys[row.key()] = true
		for _, a := range writeActors {
			if row.Answers[a.name] == "" {
				t.Errorf("%s: no answer for %s", row.key(), a.name)
			}
		}
	}
	for _, c := range writeCases() {
		if strings.ContainsAny(c.action, " \t") {
			t.Errorf("%s %s: the action %q has a space", c.method, c.route, c.action)
		}
		if !keys[c.method+" "+c.route+" "+c.action] {
			t.Errorf("%s %s %s: driven, not in the file", c.method, c.route, c.action)
		}
	}
	if raw, _ := os.ReadFile(separationMatrixWritesFile); strings.Contains(string(raw), "pending") {
		t.Errorf("%s has a pending marker", separationMatrixWritesFile)
	}
}

// selectWrites answers the rows whose method and route match and whose action
// passes keep.
func selectWrites(t *testing.T, rows []writesRow, method, route string, keep func(action string) bool) []writesRow {
	t.Helper()
	var out []writesRow
	for _, row := range rows {
		if (method == "" || row.Method == method) && row.Route == route && keep(row.Action) {
			out = append(out, row)
		}
	}
	if len(out) == 0 {
		t.Fatalf("the file has no row for %s %s", method, route)
	}
	return out
}

func anyAction(string) bool { return true }

func expectRefused(t *testing.T, row writesRow, actor string, codes ...string) {
	t.Helper()
	answer := row.Answers[actor]
	if !strings.HasPrefix(answer, "refused ") {
		t.Errorf("%s: %s is %q, want refused", row.key(), actor, answer)
		return
	}
	if len(codes) > 0 && !slices.Contains(codes, strings.TrimPrefix(answer, "refused ")) {
		t.Errorf("%s: %s is %q, want refused with one of %v", row.key(), actor, answer, codes)
	}
}

// project_administrator is refused on the project-role matrix edit and on the
// SCIM group bindings.
func TestSeparationMatrixWritesProjectAdministratorStaysOffTheRoleSets(t *testing.T) {
	rows := readWritesMatrix(t)
	pa := adminaccess.RoleProjectAdministrator
	for _, row := range selectWrites(t, rows, "", adminAPI+"/permissions/{scope}/{mode}", anyAction) {
		expectRefused(t, row, pa)
	}
	for _, row := range selectWrites(t, rows, "", adminAPI+"/user_project_permissions/administration", anyAction) {
		expectRefused(t, row, pa)
	}
	for _, route := range []string{
		adminAPI + "/scim_group_bindings/administration",
		adminAPI + "/scim_group_bindings/administration/{id}",
	} {
		for _, row := range selectWrites(t, rows, "", route, anyAction) {
			expectRefused(t, row, pa)
		}
	}
}

// admin and the holder of identity.sso.settings.manage alone are refused on
// every identity trust action and on the SCIM client create and rotate, and
// the settings action stays open to the settings holder.
func TestSeparationMatrixWritesTrustActionsNeedATrustName(t *testing.T) {
	rows := readWritesMatrix(t)
	trust := func(action string) bool { return strings.HasPrefix(action, "trust:") }
	var trustRows []writesRow
	for _, route := range []string{
		adminAPI + "/identity_providers/administration/{key}",
		adminAPI + "/identity_providers/administration/{key}/rebind",
		adminAPI + "/scim_clients/administration",
		adminAPI + "/scim_clients/administration/{id}/rotate",
	} {
		trustRows = append(trustRows, selectWrites(t, rows, "", route, trust)...)
	}
	if len(trustRows) < 8 {
		t.Fatalf("the file has %d identity trust rows, want the create, disable, issuer, secret, delete, "+
			"rebind and the SCIM client create and rotate", len(trustRows))
	}
	for _, row := range trustRows {
		expectRefused(t, row, adminaccess.RoleAdmin)
		expectRefused(t, row, writesSSOSettingsOnly)
		if answer := row.Answers[adminaccess.RoleSuperAdmin]; strings.HasPrefix(answer, "refused ") {
			t.Errorf("%s: super_admin is %q; the rows would measure nothing", row.key(), answer)
		}
	}
	for _, row := range selectWrites(t, rows, "", adminAPI+"/identity_providers/administration/{key}",
		func(action string) bool { return strings.HasPrefix(action, "settings:") }) {
		if answer := row.Answers[writesSSOSettingsOnly]; !strings.HasPrefix(answer, "allowed ") {
			t.Errorf("%s: %s is %q, want allowed", row.key(), writesSSOSettingsOnly, answer)
		}
	}
}

// A group binding write is refused with role_permission_not_held for a caller
// who does not hold the bound role's permissions in the project, whatever
// central permission opened the route. No actor is a member of the project.
func TestSeparationMatrixWritesBindingNeedsTheBoundRole(t *testing.T) {
	rows := readWritesMatrix(t)
	for _, route := range []string{
		adminAPI + "/scim_group_bindings/administration",
		adminAPI + "/scim_group_bindings/administration/{id}",
	} {
		for _, row := range selectWrites(t, rows, "", route, anyAction) {
			for _, actor := range []string{adminaccess.RoleAdmin, writesSSOSettingsOnly} {
				expectRefused(t, row, actor, adminaccess.CodePermissionNotHeld)
			}
			if answer := row.Answers[adminaccess.RoleSuperAdmin]; !strings.HasPrefix(answer, "allowed ") {
				t.Errorf("%s: super_admin is %q, want allowed", row.key(), answer)
			}
		}
	}
}

// project_administrator adding itself is role_self_escalation on the member
// create, the member update and the bulk invite. On the project matrix its
// route gate refuses it first (it holds no
// configuration.roles.user_project_permissions.edit), so the row is the
// gate's refusal; the rule behind the gate is pinned by
// internal/api/v2/admin/project_administrator_self_postgres_integration_test.go.
// The central removal route has a row, and removing a member is allowed.
func TestSeparationMatrixWritesProjectAdministratorCannotAddItself(t *testing.T) {
	rows := readWritesMatrix(t)
	pa := adminaccess.RoleProjectAdministrator
	self := func(action string) bool { return strings.Contains(action, ":self") }
	for _, route := range []string{
		adminAPI + "/users/administration/{projectID}",
		adminAPI + "/invites_bulk/administration",
	} {
		for _, row := range selectWrites(t, rows, "", route, self) {
			expectRefused(t, row, pa, adminaccess.CodeSelfEscalation)
		}
	}
	if got := len(selectWrites(t, rows, "", adminAPI+"/users/administration/{projectID}", self)); got != 2 {
		t.Errorf("the file has %d self rows on the central member routes, want the create and the update", got)
	}
	for _, row := range selectWrites(t, rows, "", adminAPI+"/user_project_permissions/administration",
		func(action string) bool { return strings.Contains(action, "self") }) {
		expectRefused(t, row, pa)
	}
	for _, row := range selectWrites(t, rows, "DELETE", adminAPI+"/users/administration/{projectID}", anyAction) {
		if answer := row.Answers[pa]; !strings.HasPrefix(answer, "allowed ") {
			t.Errorf("%s: %s is %q, want allowed", row.key(), pa, answer)
		}
	}
}

// The central override reaches the guarded setting types only: a credential,
// model or toolkit type is refused to every central role, since no actor is a
// member of the project.
func TestSeparationMatrixWritesCentralOverrideStopsAtGuardedTypes(t *testing.T) {
	rows := readWritesMatrix(t)
	for _, row := range rows {
		if !strings.HasPrefix(row.Route, configAPI+"/") {
			continue
		}
		guarded := strings.HasSuffix(row.Action, "=cloud_executor")
		for _, a := range writeActors {
			if guarded {
				continue
			}
			expectRefused(t, row, a.name)
		}
		if guarded {
			for _, actor := range []string{adminaccess.RoleSuperAdmin, adminaccess.RoleAdmin} {
				if answer := row.Answers[actor]; !strings.HasPrefix(answer, "allowed ") {
					t.Errorf("%s: %s is %q, want allowed", row.key(), actor, answer)
				}
			}
			expectRefused(t, row, adminaccess.RoleLLMManager)
		}
	}
}
