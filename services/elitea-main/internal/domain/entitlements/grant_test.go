package entitlements

import (
	"bytes"
	"slices"
	"testing"
)

func TestGroupIDRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		scope string
		role  int64
		id    string
	}{{ScopePlatform, 5, "platform-role-5"}, {ScopeProject, 301, "project-role-301"}} {
		if got := GroupID(tc.scope, tc.role); got != tc.id {
			t.Fatalf("GroupID(%s, %d) = %q", tc.scope, tc.role, got)
		}
		scope, role, ok := ParseGroupID(tc.id)
		if !ok || scope != tc.scope || role != tc.role {
			t.Fatalf("ParseGroupID(%q) = %s %d %v", tc.id, scope, role, ok)
		}
	}
	if GroupID(ScopeAgent, 3) != "" || GroupID(ScopeProject, 0) != "" {
		t.Fatal("an agent ownership or a missing role has no group")
	}
	for _, bad := range []string{"project-role-", "project-role-01", "project-role--1", "role-1", "project-role-1x"} {
		if _, _, ok := ParseGroupID(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestCSVQuotesAndDefusesFormulas(t *testing.T) {
	var buffer bytes.Buffer
	writer, err := NewCSVWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(Grant{
		UserID: 1, UserName: "a@x.io", DisplayName: "=HYPERLINK(\"x\")", UserActive: true,
		ScopeType: ScopeProject, ScopeID: "2", ScopeName: "A, \"B\"\nC", ProjectID: 2, ScopeActive: false,
		Role: "admin", Capability: string(PublishAgents), Permissions: []string{"p.one", "p.two"}, GrantedVia: ViaRole,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	want := "user_id,user_name,display_name,user_active,scope_type,scope_id,scope_name,project_id,scope_active,role,capability,permissions,granted_via\r\n" +
		"1,a@x.io,\"'=HYPERLINK(\"\"x\"\")\",true,project,2,\"A, \"\"B\"\"\r\nC\",2,false,admin,publish_agents,p.one p.two,role\r\n"
	if buffer.String() != want || writer.Rows() != 1 {
		t.Fatalf("got %q", buffer.String())
	}
}

func TestPermissionCapabilityPairsFlattenTheMap(t *testing.T) {
	permissions, capabilities := PermissionCapabilityPairs()
	if len(permissions) != len(capabilities) || !slices.IsSorted(permissions) {
		t.Fatal("pairs are not parallel and sorted")
	}
	var governance []string
	for i, permission := range permissions {
		if permission == "models.applications.governance" {
			governance = append(governance, capabilities[i])
		}
	}
	if !slices.Equal(governance, []string{"approve_change_requests", "override_release_gates", "edit_agent_registry"}) {
		t.Fatalf("governance: %v", governance)
	}
	if got := CapabilitiesOf([]string{"safety.suspend", "models.chat.messages.list", "safety.suspend"}); !slices.Equal(got, []string{"suspend"}) {
		t.Fatalf("CapabilitiesOf: %v", got)
	}
}
