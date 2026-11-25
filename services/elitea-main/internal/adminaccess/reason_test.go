package adminaccess_test

// The first list of ADR-0059 decision 6 ("Dangerous actions"), item by item,
// against the action table (story R3.8a, issue 2539). Each item of the
// decision names the entries that carry it; an entry the table drops, or one
// whose danger or reason drifts, fails here with the decision's wording.

import (
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

type decisionItem struct {
	item   string
	danger adminaccess.Danger
	keys   []string
}

const (
	idp   = "/api/v2/admin/identity_providers/administration/{key}"
	scim  = "/api/v2/admin/scim_clients/administration"
	bind  = "/api/v2/admin/scim_group_bindings/administration"
	users = "POST " + adminaccess.AuthUsersActionPattern
)

var decisionSixFirstList = []decisionItem{
	// critical
	{"create or delete an identity provider; change its trust", adminaccess.DangerCritical, []string{
		"PUT " + idp + " trust", "DELETE " + idp, "POST " + idp + "/rebind"}},
	{"create or rotate a SCIM client", adminaccess.DangerCritical, []string{
		"POST " + scim, "POST " + scim + "/{id}/rotate"}},
	{"disable an identity provider", adminaccess.DangerCritical, []string{"PUT " + idp + " disable"}},
	{"create or change a SCIM group binding", adminaccess.DangerCritical, []string{
		"POST " + bind, "PUT " + bind + "/{id}", "DELETE " + bind + "/{id}"}},
	{"assign or remove an administrator role", adminaccess.DangerCritical, []string{
		users + " set_admin_role", "PUT /api/v2/admin/user_project_permissions/administration"}},
	{"edit the role matrix", adminaccess.DangerCritical, []string{
		"POST /api/v2/admin/permissions/{scope}/{mode}", "PUT /api/v2/admin/permissions/{scope}/{mode}",
		"DELETE /api/v2/admin/roles/{scope}/{mode}"}},
	{"bind a group to an administration role", adminaccess.DangerCritical, []string{
		"POST /api/v2/admin/modes/administration", "DELETE /api/v2/admin/modes/administration"}},
	{"delete a project", adminaccess.DangerCritical, []string{"DELETE /api/v2/projects/project/{mode}/{projectID}"}},
	{"release a legal hold", adminaccess.DangerCritical, []string{"POST /api/v2/admin/legal_holds/{id}/release"}},
	{"shorten a retention policy", adminaccess.DangerCritical, []string{
		"PUT /api/v2/admin/retention/policies/{dataClass} shorten",
		"PUT /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten",
		"DELETE /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten"}},
	{"edit a guardrail rule or its fail mode", adminaccess.DangerCritical, []string{
		"POST /api/v2/admin/gateway/governance guardrail", "PUT /api/v2/admin/gateway/governance/{id} guardrail",
		"DELETE /api/v2/admin/gateway/governance/{id} guardrail"}},
	{"change the endpoint, credential or keyless identity of a global provider", adminaccess.DangerCritical, []string{
		"POST /api/v2/admin/gateway/providers", "PUT /api/v2/admin/gateway/providers/{configID} route"}},
	{"change the break_glass section", adminaccess.DangerCritical, []string{
		"PUT /api/v2/admin/plugin_config_values/administration/{plugin} break_glass"}},
	// dangerous
	{"suspend or delete a user", adminaccess.DangerDangerous, []string{
		"PUT /api/v2/admin/user_suspend/{mode}/{userID} suspend", users + " delete"}},
	{"revoke a SCIM client", adminaccess.DangerDangerous, []string{
		"POST " + scim + "/{id}/revoke", "DELETE " + scim + "/{id}"}},
	{"delete a provider or a platform model that projects use", adminaccess.DangerDangerous, []string{
		"DELETE /api/v2/admin/gateway/providers/{configID}", "DELETE /api/v2/admin/gateway/platform_models/{configID}"}},
	{"platform-wide kill switch", adminaccess.DangerDangerous, []string{"POST /api/v2/admin/execution_suspensions platform"}},
	{"maintenance mode on", adminaccess.DangerDangerous, []string{
		"PUT /api/v2/admin/plugin_config_values/administration/{plugin} maintenance:on"}},
	{"reset branding", adminaccess.DangerDangerous, []string{"DELETE /api/v2/admin/branding/administration"}},
	{"revoke a credential", adminaccess.DangerDangerous, []string{"POST /api/v2/admin/credential_inventory/revoke"}},
	{"bulk actions over more than one project", adminaccess.DangerDangerous, []string{"POST /api/v2/admin/invites_bulk/administration"}},
}

func TestEveryItemOfDecisionSixHasAnEntryThatAsksForAReason(t *testing.T) {
	entries := map[string]adminaccess.Action{}
	for _, entry := range adminaccess.ActionTable().Entries() {
		entries[entry.Key()] = entry
	}
	listed := map[string]bool{}
	for _, item := range decisionSixFirstList {
		for _, key := range item.keys {
			listed[key] = true
			entry, ok := entries[key]
			switch {
			case !ok:
				t.Errorf("%q: the action table has no entry %s", item.item, key)
			case entry.Danger != item.danger:
				t.Errorf("%q: %s is %q, want %q", item.item, key, entry.Danger, item.danger)
			case !entry.Reason:
				t.Errorf("%q: %s does not ask for a reason", item.item, key)
			}
		}
	}
	// And the other way round: a marked entry no item names is a danger the
	// decision does not list.
	for key, entry := range entries {
		if entry.Reason && !listed[key] {
			t.Errorf("%s asks for a reason and no item of decision 6 names it", key)
		}
	}
}

// The multiplexed users route has one entry per action (the complete set is
// TestMultiplexedRouteHasOneEntryPerHandlerAction); the two dangerous ones
// ask for a reason and the others do not.
func TestTheUsersRouteMarksItsActionsOneByOne(t *testing.T) {
	route := adminaccess.ActionTable().Route(http.MethodPost, adminaccess.AuthUsersActionPattern)
	if len(route) < 2 {
		t.Fatalf("POST %s has %d entries, want one per action", adminaccess.AuthUsersActionPattern, len(route))
	}
	seen := map[string]bool{}
	for _, entry := range route {
		if entry.Name == "" || seen[entry.Name] {
			t.Errorf("entry %q: want one named entry per action", entry.Key())
		}
		seen[entry.Name] = true
		want := entry.Name == "delete" || entry.Name == "set_admin_role"
		if entry.Reason != want {
			t.Errorf("%s: reason = %v, want %v", entry.Key(), entry.Reason, want)
		}
	}
}

func TestTheReasonCodesAreOneClosedList(t *testing.T) {
	codes := adminaccess.ActionReasonCodes()
	want := []string{"incident", "request", "maintenance", "correction", "offboarding", "other"}
	if len(codes) != len(want) {
		t.Fatalf("codes = %v, want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] || !adminaccess.KnownActionReasonCode(want[i]) {
			t.Errorf("codes = %v, want %v", codes, want)
		}
	}
	codes[0] = "changed"
	if adminaccess.ActionReasonCodes()[0] != "incident" || adminaccess.KnownActionReasonCode("") {
		t.Error("the list is not closed")
	}
}
