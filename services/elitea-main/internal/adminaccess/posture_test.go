package adminaccess_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The registry of R2.14a: stable ids, known severities, fail before warn
// before info, and the eight checks of the story.
func TestThePostureRegistryIsValid(t *testing.T) {
	t.Parallel()
	checks := adminaccess.PostureChecks()
	if err := adminaccess.ValidatePostureChecks(checks); err != nil {
		t.Fatal(err)
	}
	want := map[string]adminaccess.PostureSeverity{
		adminaccess.PostureBuiltInNameCollision:          adminaccess.PostureFail,
		adminaccess.PostureBuiltInSetDrift:               adminaccess.PostureFail,
		adminaccess.PostureInheritTokens:                 adminaccess.PostureWarn,
		adminaccess.PostureCustomRoleLegacyPermission:    adminaccess.PostureWarn,
		adminaccess.PostureLegacySystemRoleHeld:          adminaccess.PostureWarn,
		adminaccess.PostureInitialGlobalAdminsConfigured: adminaccess.PostureWarn,
		adminaccess.PostureSuperAdminHolders:             adminaccess.PostureInfo,
		adminaccess.PostureLegacySystemRoleExists:        adminaccess.PostureInfo,
	}
	rank := map[adminaccess.PostureSeverity]int{adminaccess.PostureFail: 0, adminaccess.PostureWarn: 1, adminaccess.PostureInfo: 2}
	last := 0
	for _, check := range checks {
		if severity, ok := want[check.ID]; !ok || severity != check.Severity {
			t.Errorf("%s is %s, want %s", check.ID, check.Severity, severity)
		}
		delete(want, check.ID)
		if rank[check.Severity] < last {
			t.Errorf("%s (%s) is listed after a less severe check", check.ID, check.Severity)
		}
		last = rank[check.Severity]
		if check.TextKey() != "admin.access.posture."+check.ID {
			t.Errorf("%s has the text key %q", check.ID, check.TextKey())
		}
	}
	for id := range want {
		t.Errorf("the registry has no %s", id)
	}
}

func TestThePostureRegistryRefusesABadCheck(t *testing.T) {
	t.Parallel()
	run := func(context.Context, adminaccess.PostureSources) (adminaccess.PostureFinding, error) {
		return adminaccess.PostureFinding{Pass: true}, nil
	}
	for name, checks := range map[string][]adminaccess.PostureCheck{
		"no id":            {{Severity: adminaccess.PostureFail, Run: run}},
		"a bad id":         {{ID: "Has-Caps", Severity: adminaccess.PostureFail, Run: run}},
		"a duplicate":      {{ID: "a", Severity: adminaccess.PostureFail, Run: run}, {ID: "a", Severity: adminaccess.PostureWarn, Run: run}},
		"unknown severity": {{ID: "a", Severity: "critical", Run: run}},
		"no read":          {{ID: "a", Severity: adminaccess.PostureInfo}},
	} {
		if err := adminaccess.ValidatePostureChecks(checks); err == nil {
			t.Errorf("ValidatePostureChecks accepted %s", name)
		}
	}
}

// Only a fail that does not pass is a failure; a check that cannot read its
// source fails the whole list rather than leave a silent hole.
func TestOnlyAFailingFailCheckIsAFailure(t *testing.T) {
	t.Parallel()
	finding := func(pass bool) func(context.Context, adminaccess.PostureSources) (adminaccess.PostureFinding, error) {
		return func(context.Context, adminaccess.PostureSources) (adminaccess.PostureFinding, error) {
			return adminaccess.PostureFinding{Pass: pass}, nil
		}
	}
	results, err := adminaccess.RunPosture(t.Context(), adminaccess.PostureSources{}, []adminaccess.PostureCheck{
		{ID: "f", Severity: adminaccess.PostureFail, Run: finding(false)},
		{ID: "w", Severity: adminaccess.PostureWarn, Run: finding(false)},
		{ID: "i", Severity: adminaccess.PostureInfo, Run: finding(false)},
		{ID: "p", Severity: adminaccess.PostureFail, Run: finding(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if want := result.Check.ID == "f"; result.Failed() != want {
			t.Errorf("%s: Failed() = %v, want %v", result.Check.ID, result.Failed(), want)
		}
		if result.Facts.Roles == nil || result.Facts.People == nil {
			t.Errorf("%s: nil facts lists", result.Check.ID)
		}
	}
	_, err = adminaccess.RunPosture(t.Context(), adminaccess.PostureSources{}, []adminaccess.PostureCheck{
		{ID: "broken", Severity: adminaccess.PostureInfo,
			Run: func(context.Context, adminaccess.PostureSources) (adminaccess.PostureFinding, error) {
				return adminaccess.PostureFinding{}, errors.New("unreadable")
			}},
	})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("an unreadable check: %v, want the list to fail naming it", err)
	}
}
