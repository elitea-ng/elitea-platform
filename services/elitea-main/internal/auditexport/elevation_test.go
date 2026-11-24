package auditexport

import (
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// An admin.elevation event (ADR-0059 R2.3) is exported with its step under
// details.elevation, for every action of the list, and conforms to
// elitea.audit.v1.
func TestAdminElevationIsExportedWithItsStep(t *testing.T) {
	schema := compileSchema(t)
	const elevationID = "0b7c2d4e-0000-4000-8000-000000000001"
	for _, action := range securityaudit.ElevationActions() {
		t.Run(string(action), func(t *testing.T) {
			change := securityaudit.ElevationChange{
				Action: action, Source: securityaudit.ElevationSourceConsole, ElevationID: elevationID, UserID: 42,
				Level: "super_admin", Path: "approved", ReasonCode: "identity_outage",
				ExpiresAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
			}
			switch action {
			case securityaudit.ElevationEnded, securityaudit.ElevationExpired:
				change.EndReason = "holder"
			case securityaudit.ElevationNoteAdded:
				change.NoteID, change.NoteLength = "note-1", 40
			case securityaudit.ElevationCredentialIssued, securityaudit.ElevationCredentialRevoked:
				change = securityaudit.ElevationChange{Action: action, Source: securityaudit.ElevationSourceOperator,
					CredentialID: "cred-1"}
			case securityaudit.ElevationCredentialUsed:
				change.CredentialID = "cred-1"
			case securityaudit.ElevationLocalAdminSignin:
				change = securityaudit.ElevationChange{Action: action, Source: securityaudit.ElevationSourceConsole, UserID: 42}
			}
			record := securityRecord(t, securityaudit.TypeAdminElevation, func(e *securityaudit.SecurityEvent) {
				built := securityaudit.NewAdminElevation(securityaudit.AdminElevation{ActorUserID: 7, Change: change})
				built.ID, built.Source, built.Time = e.ID, e.Source, e.Time
				*e = built
			})
			event, err := FromSecurityRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(t, schema, event); err != nil {
				t.Fatalf("admin.elevation does not conform to elitea.audit.v1: %v", err)
			}
			details, ok := event.Details.(SecurityAuditDetails)
			if !ok || details.Elevation == nil {
				t.Fatalf("details = %#v, want the elevation step", event.Details)
			}
			if details.Elevation.Action != string(action) || details.Elevation.Source != change.Source {
				t.Fatalf("elevation = %+v", details.Elevation)
			}
			if (details.Elevation.NoteLength != nil) != (action == securityaudit.ElevationNoteAdded) {
				t.Fatalf("note_length = %v on %s", details.Elevation.NoteLength, action)
			}
			if event.Action != action.EventAction() {
				t.Fatalf("action = %q", event.Action)
			}
		})
	}
}
