package repos

// The entitlement export (ADR-0044 decision 5, #1501) against a FRESH
// INSTALL: 001_initial.sql through migrate.Bootstrap, then the whole shared
// history (so 0204's agent registry and 0209's grant are the real ones).

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func newEntitlementPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	applied, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial)
	if err != nil || !applied {
		t.Fatalf("bootstrap: applied=%v err=%v", applied, err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared: %v", err)
	}
	return pool
}

// seedEntitlements writes the fixture every test below reads:
//
//	alice 101  administration role `governor` (safety.suspend,
//	           entitlements.export, a chat permission); admin on Alpha
//	           (own rows: publish, governance, chat); technical owner of
//	           agent 31 in Alpha.
//	bob   102  viewer on Alpha (own row: chat only); business owner of 31.
//	carol 103  SUSPENDED; auditor on Beta (SUSPENDED project), no own rows,
//	           so the central default `auditor` role applies.
//	dave  104  display name that a spreadsheet would evaluate; viewer on Alpha.
//	erin  105  auditor AND scribe on Beta; scribe has an own row there, so
//	           the central fallback is off for BOTH of erin's roles.
func seedEntitlements(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO auth_core__user (id, email, name, suspended) VALUES
    (101, 'alice@example.com', 'Alice, "The" Admin', false),
    (102, 'bob@example.com', E'Bob\nSmith', false),
    (103, 'carol@example.com', 'Carol', true),
    (104, 'dave@example.com', '=SUM(1)', false),
    (105, 'erin@example.com', 'Erin', false);
INSERT INTO centry.project (id, name, owner_id, create_success, suspended) VALUES
    (201, 'Alpha, Inc', 101, true, false),
    (202, 'Beta', 101, true, true);

INSERT INTO auth_core__role (id, name, mode) VALUES
    (901, 'governor', 'administration'),
    (902, 'auditor', 'default');
INSERT INTO auth_core__role_permission (role_id, permission) VALUES
    (901, 'safety.suspend'), (901, 'entitlements.export'), (901, 'models.chat.messages.list'),
    (902, 'runs.evidence.read'), (902, 'models.chat.messages.list');
INSERT INTO auth_core__user_role (user_id, role_id) VALUES (101, 901);

INSERT INTO auth_core__project_role (id, project_id, name) VALUES
    (301, 201, 'admin'), (302, 201, 'viewer'), (303, 202, 'auditor'), (304, 202, 'scribe');
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (201, 301, 'models.applications.publish.post'),
    (201, 301, 'models.applications.governance'),
    (201, 301, 'models.chat.messages.list'),
    (201, 302, 'models.chat.messages.list'),
    (202, 304, 'models.chat.messages.list');
INSERT INTO auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (201, 101, 301), (201, 102, 302), (202, 103, 303), (201, 104, 302),
    (202, 105, 303), (202, 105, 304);

CREATE SCHEMA p_201;
CREATE TABLE p_201.applications (id integer PRIMARY KEY, name text);
INSERT INTO p_201.applications (id, name) VALUES (31, 'Support Bot');
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, business_owner_id, technical_owner_id)
VALUES (201, 31, 102, 101);`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// fixtureCSV is the exact export of the fixture's five accounts.
const fixtureCSV = "user_id,user_name,display_name,user_active,scope_type,scope_id,scope_name,project_id,scope_active,role,capability,permissions,granted_via\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,platform,administration,Platform (administration),,true,governor,export_entitlements,entitlements.export,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,platform,administration,Platform (administration),,true,governor,suspend,safety.suspend,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,project,201,"Alpha, Inc",201,true,admin,approve_change_requests,models.applications.governance,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,project,201,"Alpha, Inc",201,true,admin,edit_agent_registry,models.applications.governance,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,project,201,"Alpha, Inc",201,true,admin,override_release_gates,models.applications.governance,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,project,201,"Alpha, Inc",201,true,admin,publish_agents,models.applications.publish.post,role` + "\r\n" +
	`101,alice@example.com,"Alice, ""The"" Admin",true,agent,31,Support Bot,201,true,technical_owner,edit_agent_registry,,owner` + "\r\n" +
	"102,bob@example.com,\"Bob\r\nSmith\",true,project,201,\"Alpha, Inc\",201,true,viewer,,,role\r\n" +
	"102,bob@example.com,\"Bob\r\nSmith\",true,agent,31,Support Bot,201,true,business_owner,edit_agent_registry,,owner\r\n" +
	"103,carol@example.com,Carol,false,project,202,Beta,202,false,auditor,read_run_evidence,runs.evidence.read,role\r\n" +
	`104,dave@example.com,'=SUM(1),true,project,201,"Alpha, Inc",201,true,viewer,,,role` + "\r\n" +
	"105,erin@example.com,Erin,true,project,202,Beta,202,false,auditor,,,role\r\n" +
	"105,erin@example.com,Erin,true,project,202,Beta,202,false,scribe,,,role\r\n"

func streamCSV(t *testing.T, store *Entitlements, filter EntitlementFilter) (string, int) {
	t.Helper()
	var (
		buffer   bytes.Buffer
		writer   *entitlements.CSVWriter
		reported = -1
	)
	err := store.Stream(context.Background(), filter, func(rows int) error {
		reported = rows
		var err error
		writer, err = entitlements.NewCSVWriter(&buffer)
		return err
	}, func(grant entitlements.Grant) error {
		// Only the fixture's accounts: the fresh install seeds its own.
		if grant.UserID < 101 {
			return nil
		}
		return writer.Write(grant)
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String(), reported
}

func TestEntitlements_CSVRowsAreExact(t *testing.T) {
	pool := newEntitlementPool(t)
	seedEntitlements(t, pool)
	store := NewEntitlements(pool)

	got, reported := streamCSV(t, store, EntitlementFilter{})
	if got != fixtureCSV {
		t.Fatalf("csv mismatch\n got:\n%s\nwant:\n%s", got, fixtureCSV)
	}
	// The count `before` receives is the whole export, the fresh install's
	// own dev user included, and equals what was streamed.
	var all int
	if err := store.Stream(context.Background(), EntitlementFilter{}, func(int) error { return nil },
		func(entitlements.Grant) error { all++; return nil }); err != nil {
		t.Fatal(err)
	}
	if reported != all {
		t.Fatalf("before reported %d rows, %d were streamed", reported, all)
	}

	// The filters narrow to one account and to one project.
	alice, _ := streamCSV(t, store, EntitlementFilter{UserID: 101})
	if strings.Count(alice, "\r\n") != 1+7 {
		t.Fatalf("user filter: %q", alice)
	}
	beta, _ := streamCSV(t, store, EntitlementFilter{ProjectID: 202})
	if strings.Count(beta, "\r\n") != 1+3 || strings.Contains(beta, "platform") {
		t.Fatalf("project filter: %q", beta)
	}

	// JSON pages are the same rows.
	page, total, err := store.Page(context.Background(), EntitlementFilter{UserID: 101}, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 || len(page) != 3 || page[0].Capability != "approve_change_requests" || page[0].RoleID != 301 {
		t.Fatalf("page: total=%d rows=%+v", total, page)
	}
}

// The export says what the resolver allows: every permission exported for an
// active account on an active project is one legacyrbac resolves for it, and
// the per-user fallback rule (erin) matches.
func TestEntitlements_AgreeWithTheResolver(t *testing.T) {
	pool := newEntitlementPool(t)
	seedEntitlements(t, pool)
	store := NewEntitlements(pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	ctx := context.Background()

	_, _ = pool.Exec(ctx, `UPDATE centry.project SET suspended = false WHERE id = 202`)
	for _, check := range []struct {
		user, project int64
		mode          string
	}{{101, 201, auth.PermissionModeDefault}, {105, 202, auth.PermissionModeDefault}, {101, 0, auth.PermissionModeAdministration}} {
		id := entitlementID(check.user)
		resolution, err := resolver.ResolvePermissions(ctx, auth.User{ID: id, UserID: id}, check.mode, entitlementID(check.project))
		if err != nil {
			t.Fatalf("resolve %+v: %v", check, err)
		}
		filter := EntitlementFilter{UserID: check.user, ProjectID: check.project}
		var exported []string
		if err := store.Stream(ctx, filter, func(int) error { return nil }, func(g entitlements.Grant) error {
			wanted := entitlements.ScopeProject
			if check.mode == auth.PermissionModeAdministration {
				wanted = entitlements.ScopePlatform
			}
			if g.ScopeType == wanted && (wanted == entitlements.ScopeProject || g.ScopeID == check.mode) {
				exported = append(exported, g.Permissions...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, permission := range exported {
			if !slices.Contains(resolution.Permissions, permission) {
				t.Errorf("%+v: exported %s, which the resolver does not grant (%v)", check, permission, resolution.Permissions)
			}
		}
		// And every governance permission the resolver grants is exported.
		for _, permission := range resolution.Permissions {
			if len(entitlements.Capabilities[permission]) > 0 && !slices.Contains(exported, permission) {
				t.Errorf("%+v: the resolver grants %s, which the export omits", check, permission)
			}
		}
	}
}

func entitlementID(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func TestEntitlements_SCIMUsersPagingAndFilter(t *testing.T) {
	pool := newEntitlementPool(t)
	seedEntitlements(t, pool)
	store := NewEntitlements(pool)
	ctx := context.Background()

	filter, err := scimdirectory.ParseEntitlementUserFilter(`userName eq "ALICE@example.com"`)
	if err != nil {
		t.Fatal(err)
	}
	users, total, err := store.ListUsers(ctx, filter, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(users) != 1 || users[0].UserID != 101 || len(users[0].Grants) != 7 {
		t.Fatalf("filter: total=%d users=%+v", total, users)
	}
	if users[0].Grants[6].ScopeName != "Support Bot" || users[0].Grants[6].GrantedVia != entitlements.ViaOwner {
		t.Fatalf("owner grant: %+v", users[0].Grants[6])
	}

	// Paging: the fixture's five accounts, two per page, from the second.
	filter, err = scimdirectory.ParseEntitlementUserFilter(`userName ew "@example.com"`)
	if err != nil {
		t.Fatal(err)
	}
	users, total, err = store.ListUsers(ctx, filter, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || len(users) != 2 || users[0].UserID != 102 || users[1].UserID != 103 {
		t.Fatalf("paging: total=%d users=%+v", total, users)
	}
	if !slices.Equal([]string{users[1].Grants[0].Capability}, []string{"read_run_evidence"}) || users[1].Active {
		t.Fatalf("carol: %+v", users[1])
	}
	// count=0 answers the total alone.
	users, total, err = store.ListUsers(ctx, filter, 1, 0)
	if err != nil || total != 5 || len(users) != 0 {
		t.Fatalf("count=0: total=%d users=%d err=%v", total, len(users), err)
	}
	// An account with no grant is listed with none.
	if _, err := pool.Exec(ctx, `INSERT INTO auth_core__user (id, email, name) VALUES (106, 'frank@example.com', 'Frank')`); err != nil {
		t.Fatal(err)
	}
	frank, err := store.GetUser(ctx, 106)
	if err != nil || len(frank.Grants) != 0 || frank.UserName != "frank@example.com" {
		t.Fatalf("frank: %+v %v", frank, err)
	}
	if _, err := store.GetUser(ctx, 999); err != ErrEntitlementNotFound {
		t.Fatalf("missing user: %v", err)
	}
}

func TestEntitlements_SCIMGroups(t *testing.T) {
	pool := newEntitlementPool(t)
	seedEntitlements(t, pool)
	store := NewEntitlements(pool)
	ctx := context.Background()

	filter, err := scimdirectory.ParseEntitlementGroupFilter(`displayName eq "Alpha, Inc / admin"`)
	if err != nil {
		t.Fatal(err)
	}
	groups, total, err := store.ListGroups(ctx, filter, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("groups: %d %+v", total, groups)
	}
	admin := groups[0]
	if admin.ID != "project-role-301" || admin.ProjectID != 201 || len(admin.Members) != 1 || admin.Members[0].UserID != 101 ||
		!slices.Equal(admin.Capabilities, []string{"approve_change_requests", "edit_agent_registry", "override_release_gates", "publish_agents"}) ||
		!slices.Equal(admin.Permissions, []string{"models.applications.governance", "models.applications.publish.post", "models.chat.messages.list"}) {
		t.Fatalf("admin group: %+v", admin)
	}

	// A project role with no own rows takes the central default role's.
	auditor, err := store.GetGroup(ctx, "project-role-303")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(auditor.Permissions, []string{"models.chat.messages.list", "runs.evidence.read"}) ||
		auditor.ScopeActive || len(auditor.Members) != 2 {
		t.Fatalf("auditor group: %+v", auditor)
	}
	governor, err := store.GetGroup(ctx, "platform-role-901")
	if err != nil || governor.ScopeID != "administration" || !slices.Equal(governor.Capabilities, []string{"export_entitlements", "suspend"}) {
		t.Fatalf("governor group: %+v %v", governor, err)
	}
	for _, missing := range []string{"project-role-999999", "role-1", "project-role-01"} {
		if _, err := store.GetGroup(ctx, missing); err != ErrEntitlementNotFound {
			t.Fatalf("%s: %v", missing, err)
		}
	}
	// Paging over every group: platform roles first, then project roles.
	all, total, err := store.ListGroups(ctx, scimdirectory.Filter{}, 1, 500)
	if err != nil || len(all) != total || all[0].ScopeType != entitlements.ScopePlatform || all[len(all)-1].ID != "project-role-304" {
		t.Fatalf("all groups: %d/%d %v", len(all), total, err)
	}
	second, _, err := store.ListGroups(ctx, scimdirectory.Filter{}, 2, 1)
	if err != nil || len(second) != 1 || second[0].ID != all[1].ID {
		t.Fatalf("second page: %+v %v", second, err)
	}
}

func TestEntitlements_ExportIsAudited(t *testing.T) {
	pool := newEntitlementPool(t)
	store := NewEntitlements(pool)
	ctx := context.Background()
	for _, record := range []EntitlementExportAudit{
		{UserID: 1, Actor: "dev@elitea.ai", Format: "csv", Rows: 13, Route: "/api/v2/admin/entitlements"},
		{Actor: "scim:sailpoint", Format: "scim", Rows: 2, Route: "/api/v2/scim/v2/entitlements/Users"},
	} {
		if err := store.RecordExport(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `
SELECT COALESCE(user_id, 0), COALESCE(user_email, ''), action, entity_type, entity_name, http_route
FROM centry.audit_events WHERE entity_type = 'entitlements' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var (
			user                                 int64
			email, action, entity, format, route string
		)
		if err := rows.Scan(&user, &email, &action, &entity, &format, &route); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{entitlementID(user), email, action, entity, format, route}, "|"))
	}
	want := []string{
		"1|dev@elitea.ai|export entitlements csv: 13 rows|entitlements|csv|/api/v2/admin/entitlements",
		"|scim:sailpoint|export entitlements scim: 2 rows|entitlements|scim|/api/v2/scim/v2/entitlements/Users",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit rows:\n got %q\nwant %q", got, want)
	}
}

// 001_initial.sql seeds entitlements.export on a fresh install, and shared
// 0209 grants it on an existing database: to super_admin and admin in the
// administration mode, and to nobody else.
func TestEntitlements_ExportPermissionGrant(t *testing.T) {
	pool := newEntitlementPool(t)
	ctx := context.Background()
	holders := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `
SELECT r.name || '/' || r.mode FROM auth_core__role_permission AS p JOIN auth_core__role AS r ON r.id = p.role_id
WHERE p.permission = 'entitlements.export' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var holder string
			if err := rows.Scan(&holder); err != nil {
				t.Fatal(err)
			}
			out = append(out, holder)
		}
		return out
	}
	want := []string{"admin/administration", "super_admin/administration"}
	if got := holders(); !slices.Equal(got, want) {
		t.Fatalf("fresh install: %v", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM auth_core__role_permission WHERE permission = 'entitlements.export'`); err != nil {
		t.Fatal(err)
	}
	migration, err := platformmigrations.Files.ReadFile("shared/0209_entitlement_export.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if got := holders(); !slices.Equal(got, want) {
		t.Fatalf("0209: %v", got)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__user (id, email, name) VALUES (150, 'operator@example.com', 'Operator');
INSERT INTO auth_core__user_role (user_id, role_id)
SELECT 150, id FROM auth_core__role WHERE name = 'admin' AND mode = 'administration'`); err != nil {
		t.Fatal(err)
	}
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(ctx,
		auth.User{ID: "150", UserID: "150"}, auth.PermissionModeAdministration, "")
	if err != nil || !slices.Contains(resolution.Permissions, entitlements.ExportPermission) {
		t.Fatalf("an administrator does not resolve %s: %v %v", entitlements.ExportPermission, resolution.Permissions, err)
	}
}
