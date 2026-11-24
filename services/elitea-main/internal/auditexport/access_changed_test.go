package auditexport

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// An admin.access.changed event (ADR-0059 R1.5) is exported with its access
// payload, and conforms to elitea.audit.v1.
func TestAdminAccessChangedIsExportedWithItsSets(t *testing.T) {
	schema := compileSchema(t)
	for _, change := range []securityaudit.AccessChange{
		{Kind: securityaudit.AccessRoleAssignment, Mode: "administration", Source: "bootstrap", After: []string{"super_admin"}},
		{Kind: securityaudit.AccessIdentityTrust, Fields: []string{"issuer", "secret"}, ReasonCode: "incident"},
		{
			Kind: securityaudit.AccessProjectMembership, Before: nil, After: []string{"editor"},
			Membership: &securityaudit.MembershipFacts{MemberHoldsAdminRole: true},
		},
		// Issue 2994: the rename and the estate-wide push.
		{
			Kind: securityaudit.AccessRoleRenamed, Mode: "default", Before: []string{"auditors"},
			After: []string{"reviewers"}, Fields: []string{"project_role_copies"},
		},
		{
			Kind: securityaudit.AccessProjectPermissionsPushed, Mode: "default",
			Before: []string{"models.alpha.view"}, After: []string{"models.beta.view"}, Fields: []string{"roles_assigned"},
		},
	} {
		t.Run(string(change.Kind), func(t *testing.T) {
			record := securityRecord(t, securityaudit.TypeAdminAccessChanged, func(e *securityaudit.SecurityEvent) {
				built := securityaudit.NewAdminAccessChanged(securityaudit.AdminAccessChanged{
					ActorUserID: 7, SubjectType: "user", SubjectID: "42", Change: change,
				})
				built.ID, built.Source, built.Time = e.ID, e.Source, e.Time
				*e = built
			})
			event, err := FromSecurityRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(t, schema, event); err != nil {
				t.Fatalf("admin.access.changed does not conform to elitea.audit.v1: %v", err)
			}
			details, ok := event.Details.(SecurityAuditDetails)
			if !ok || details.Access == nil {
				t.Fatalf("details = %#v, want the access payload", event.Details)
			}
			if details.Access.Kind != string(change.Kind) || details.Access.Before == nil ||
				strings.Join(details.Access.After, ",") != strings.Join(change.After, ",") {
				t.Fatalf("access = %+v", details.Access)
			}
			if event.Action != change.Kind.Action() {
				t.Fatalf("action = %q", event.Action)
			}
		})
	}
}
