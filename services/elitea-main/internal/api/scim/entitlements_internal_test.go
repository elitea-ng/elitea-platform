package scim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

type fakeEntitlements struct {
	users      []entitlements.UserEntitlements
	groups     []entitlements.Group
	audits     []repos.EntitlementExportAudit
	auditFails bool
	starts     []int
	counts     []int
}

func (f *fakeEntitlements) ListUsers(_ context.Context, _ scimdirectory.Filter, startIndex, count int) ([]entitlements.UserEntitlements, int, error) {
	f.starts, f.counts = append(f.starts, startIndex), append(f.counts, count)
	return f.users, len(f.users) + 10, nil
}

func (f *fakeEntitlements) GetUser(_ context.Context, id int64) (entitlements.UserEntitlements, error) {
	for _, user := range f.users {
		if user.UserID == id {
			return user, nil
		}
	}
	return entitlements.UserEntitlements{}, repos.ErrEntitlementNotFound
}

func (f *fakeEntitlements) ListGroups(context.Context, scimdirectory.Filter, int, int) ([]entitlements.Group, int, error) {
	return f.groups, len(f.groups), nil
}

func (f *fakeEntitlements) GetGroup(_ context.Context, id string) (entitlements.Group, error) {
	for _, group := range f.groups {
		if group.ID == id {
			return group, nil
		}
	}
	return entitlements.Group{}, repos.ErrEntitlementNotFound
}

func (f *fakeEntitlements) RecordExport(_ context.Context, record repos.EntitlementExportAudit) error {
	if f.auditFails {
		return errors.New("audit table unavailable")
	}
	f.audits = append(f.audits, record)
	return nil
}

func entitlementFixture() *fakeEntitlements {
	return &fakeEntitlements{
		users: []entitlements.UserEntitlements{{
			UserID: 101, UserName: "alice@example.com", DisplayName: "Alice", Active: true,
			Grants: []entitlements.Grant{
				{UserID: 101, ScopeType: "platform", ScopeID: "administration", ScopeName: "Platform (administration)",
					ScopeActive: true, Role: "admin", RoleID: 5, Capability: "suspend",
					Permissions: []string{"safety.suspend"}, GrantedVia: "role"},
				{UserID: 101, ScopeType: "project", ScopeID: "7", ScopeName: "Alpha", ProjectID: 7, ScopeActive: true,
					Role: "admin", RoleID: 15, Capability: "publish_agents",
					Permissions: []string{"models.applications.publish.post"}, GrantedVia: "role"},
				{UserID: 101, ScopeType: "project", ScopeID: "7", ScopeName: "Alpha", ProjectID: 7, ScopeActive: true,
					Role: "admin", RoleID: 15, Capability: "approve_change_requests",
					Permissions: []string{"models.applications.governance"}, GrantedVia: "role"},
				{UserID: 101, ScopeType: "agent", ScopeID: "12", ScopeName: "Support Bot", ProjectID: 7, ScopeActive: true,
					Role: "technical_owner", Capability: "edit_agent_registry", Permissions: []string{}, GrantedVia: "owner"},
			},
		}},
		groups: []entitlements.Group{{
			ID: "project-role-15", DisplayName: "Alpha / admin", ScopeType: "project", ScopeID: "7", ScopeName: "Alpha",
			ProjectID: 7, ScopeActive: true, Role: "admin", RoleID: 15,
			Permissions:  []string{"models.applications.publish.post"},
			Capabilities: []string{"publish_agents"},
			Members:      []entitlements.Member{{UserID: 101, UserName: "alice@example.com"}},
		}},
	}
}

func serveEntitlements(source EntitlementSource, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	recorder := httptest.NewRecorder()
	NewHandler(newRecordingDirectory()).WithEntitlements(source).Routes().ServeHTTP(recorder, request)
	return recorder
}

func TestEntitlementUsersCarryGroupsRolesAndEntitlements(t *testing.T) {
	source := entitlementFixture()
	recorder := serveEntitlements(source, http.MethodGet, "/entitlements/Users?startIndex=3&count=2")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, contentType, recorder.Header().Get("Content-Type"))
	body := decodeBody(t, recorder)
	require.EqualValues(t, 11, body["totalResults"])
	require.EqualValues(t, 3, body["startIndex"])
	require.Equal(t, []int{3}, source.starts)
	require.Equal(t, []int{2}, source.counts)

	user := body["Resources"].([]any)[0].(map[string]any)
	require.Equal(t, "101", user["id"])
	require.Equal(t, []any{schemaUser, schemaEntitlementUser}, user["schemas"])
	require.Equal(t, []any{
		map[string]any{"value": "platform-role-5", "display": "Platform (administration) / admin", "type": "direct",
			"$ref": "/api/v2/scim/v2/entitlements/Groups/platform-role-5"},
		map[string]any{"value": "project-role-15", "display": "project Alpha / admin", "type": "direct",
			"$ref": "/api/v2/scim/v2/entitlements/Groups/project-role-15"},
	}, user["groups"])
	require.Equal(t, []any{
		map[string]any{"value": "platform:administration:admin", "display": "admin on Platform (administration)", "type": "platform"},
		map[string]any{"value": "project:7:admin", "display": "admin on project Alpha", "type": "project"},
		map[string]any{"value": "agent:7/12:technical_owner", "display": "technical_owner on agent Support Bot (project 7)", "type": "agent"},
	}, user["roles"])
	values := []string{}
	for _, entitlement := range user["entitlements"].([]any) {
		values = append(values, entitlement.(map[string]any)["value"].(string))
	}
	require.Equal(t, []string{"platform:administration:suspend", "project:7:publish_agents",
		"project:7:approve_change_requests", "agent:7/12:edit_agent_registry"}, values)
	grants := user[schemaEntitlementUser].(map[string]any)["grants"].([]any)
	require.Len(t, grants, 4)
	require.Equal(t, "owner", grants[3].(map[string]any)["grantedVia"])

	// The answer is audited with the client as the actor and the count.
	require.Len(t, source.audits, 1)
	require.Equal(t, "scim", source.audits[0].Format)
	require.Equal(t, 1, source.audits[0].Rows)
}

func TestEntitlementGroupAndLookups(t *testing.T) {
	source := entitlementFixture()
	recorder := serveEntitlements(source, http.MethodGet, "/entitlements/Groups/project-role-15")
	require.Equal(t, http.StatusOK, recorder.Code)
	group := decodeBody(t, recorder)
	require.Equal(t, "Alpha / admin", group["displayName"])
	require.Equal(t, []any{map[string]any{"value": "101", "display": "alice@example.com", "type": "User",
		"$ref": "/api/v2/scim/v2/entitlements/Users/101"}}, group["members"])
	extension := group[schemaEntitlementGroup].(map[string]any)
	require.Equal(t, []any{"publish_agents"}, extension["capabilities"])
	require.EqualValues(t, 7, extension["projectId"])

	// excludedAttributes=members drops the member list (Entra sends it).
	recorder = serveEntitlements(source, http.MethodGet, "/entitlements/Groups?excludedAttributes=members")
	require.Equal(t, http.StatusOK, recorder.Code)
	listed := decodeBody(t, recorder)["Resources"].([]any)[0].(map[string]any)
	require.NotContains(t, listed, "members")

	require.Equal(t, http.StatusNotFound, serveEntitlements(source, http.MethodGet, "/entitlements/Groups/project-role-99").Code)
	require.Equal(t, http.StatusNotFound, serveEntitlements(source, http.MethodGet, "/entitlements/Users/abc").Code)
	require.Equal(t, http.StatusOK, serveEntitlements(source, http.MethodGet, "/entitlements/Users/101").Code)
}

func TestEntitlementTreeIsReadOnlyFilteredAndAudited(t *testing.T) {
	source := entitlementFixture()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		require.Equal(t, http.StatusMethodNotAllowed,
			serveEntitlements(source, method, "/entitlements/Users/101").Code, method)
	}
	require.Equal(t, http.StatusMethodNotAllowed, serveEntitlements(source, http.MethodPost, "/entitlements/Groups").Code)

	recorder := serveEntitlements(source, http.MethodGet, `/entitlements/Users?filter=userName+eq+%22a%22+or+active+eq+true`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalidFilter", decodeBody(t, recorder)["scimType"])
	recorder = serveEntitlements(source, http.MethodGet, `/entitlements/Users?filter=externalId+eq+%22x%22`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	// A read the trail cannot record is refused, with no resource in it.
	source.auditFails = true
	recorder = serveEntitlements(source, http.MethodGet, "/entitlements/Users")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "alice@example.com")

	// Without a source the tree answers 503, never an empty directory.
	require.Equal(t, http.StatusServiceUnavailable, serveEntitlements(nil, http.MethodGet, "/entitlements/Users").Code)
	// The discovery documents need no source.
	require.Equal(t, http.StatusOK, serveEntitlements(nil, http.MethodGet, "/entitlements/ServiceProviderConfig").Code)
	require.Equal(t, http.StatusOK, serveEntitlements(nil, http.MethodGet, "/entitlements/ResourceTypes").Code)
	require.Equal(t, http.StatusOK, serveEntitlements(nil, http.MethodGet, "/entitlements/Schemas").Code)
}
