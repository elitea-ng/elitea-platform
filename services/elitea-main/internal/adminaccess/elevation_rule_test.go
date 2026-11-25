package adminaccess_test

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The decision of ADR-0059 R2.8a, value by value: it applies only when an
// elevation took part and the standing set does not admit the entry's
// permission; sealed opens to the sealed super_admin elevation only.
func TestDecideElevation(t *testing.T) {
	entry := func(e adminaccess.Elevation) adminaccess.Action {
		return adminaccess.Action{Method: "PUT", Pattern: "/x", Permission: adminaccess.IdentitySSOTrustManage, Elevation: e}
	}
	elevated := adminaccess.ElevationCaller{Elevated: true}
	sealed := adminaccess.ElevationCaller{Elevated: true, SealedSuperAdmin: true}
	standing := adminaccess.ElevationCaller{Elevated: true, Standing: []string{adminaccess.IdentitySSOTrustManage}}
	for name, tc := range map[string]struct {
		entry   adminaccess.Action
		caller  adminaccess.ElevationCaller
		refused bool
		flagged bool
	}{
		"allow":                 {entry(adminaccess.ElevationAllow), elevated, false, false},
		"flag":                  {entry(adminaccess.ElevationFlag), elevated, false, true},
		"deny":                  {entry(adminaccess.ElevationDeny), elevated, true, false},
		"sealed, approved":      {entry(adminaccess.ElevationSealed), elevated, true, false},
		"sealed, sealed super":  {entry(adminaccess.ElevationSealed), sealed, false, true},
		"deny, sealed super":    {entry(adminaccess.ElevationDeny), sealed, true, false},
		"deny, not elevated":    {entry(adminaccess.ElevationDeny), adminaccess.ElevationCaller{}, false, false},
		"deny, standing admits": {entry(adminaccess.ElevationDeny), standing, false, false},
		"flag, standing admits": {entry(adminaccess.ElevationFlag), standing, false, false},
	} {
		decision := adminaccess.DecideElevation(tc.entry, tc.caller)
		if (decision.Refusal != nil) != tc.refused || decision.Flag != tc.flagged {
			t.Errorf("%s: %+v, want refused %v flagged %v", name, decision, tc.refused, tc.flagged)
		}
		if decision.Refusal != nil && decision.Refusal.Code != adminaccess.CodeElevationRestricted {
			t.Errorf("%s: code %q", name, decision.Refusal.Code)
		}
	}
}

// A legacy name the entry's permission narrows admits the action too: the
// gates accept it beside the narrow name.
func TestDecideElevationReadsTheLegacyNameANarrowNameNarrows(t *testing.T) {
	for _, entry := range adminaccess.ActionTable().Entries() {
		row, ok := adminaccess.Lookup(entry.Permission)
		if !ok || len(row.Narrows) == 0 || entry.Elevation != adminaccess.ElevationDeny {
			continue
		}
		caller := adminaccess.ElevationCaller{Elevated: true, Standing: []string{row.Narrows[0]}}
		if decision := adminaccess.DecideElevation(entry, caller); decision.Refusal != nil {
			t.Fatalf("%s: a standing %s (which %s narrows) is restricted", entry.Key(), row.Narrows[0], entry.Permission)
		}
		return
	}
	t.Skip("no deny entry names a narrow permission")
}

// Only a sealed session whose elevation is on the sealed path at the
// super_admin level is the sealed super_admin caller.
func TestElevationCallerOf(t *testing.T) {
	resolution := func(level, path string) auth.PermissionResolution {
		return auth.PermissionResolution{ElevationID: "e", Elevations: []auth.ElevationGrant{{ID: "e", Level: level, Path: path}}}
	}
	sealed := auth.Credential{Kind: auth.CredentialKindSealed, BrowserSessionRef: 1}
	session := auth.Credential{Kind: auth.CredentialKindSession, BrowserSessionRef: 1}
	for name, tc := range map[string]struct {
		credential auth.Credential
		resolution auth.PermissionResolution
		want       bool
	}{
		"sealed super_admin":    {sealed, resolution(adminaccess.ElevationLevelSuperAdmin, adminaccess.ElevationPathSealed), true},
		"sealed admin":          {sealed, resolution(adminaccess.ElevationLevelAdmin, adminaccess.ElevationPathSealed), false},
		"session, sealed path":  {session, resolution(adminaccess.ElevationLevelSuperAdmin, adminaccess.ElevationPathSealed), false},
		"sealed, approved path": {sealed, resolution(adminaccess.ElevationLevelSuperAdmin, adminaccess.ElevationPathApproved), false},
		"sealed, no elevation":  {sealed, auth.PermissionResolution{}, false},
	} {
		caller := adminaccess.ElevationCallerOf(tc.credential, tc.resolution)
		if caller.SealedSuperAdmin != tc.want {
			t.Errorf("%s: sealed super_admin %v, want %v", name, caller.SealedSuperAdmin, tc.want)
		}
	}
}
