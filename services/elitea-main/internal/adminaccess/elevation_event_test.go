package adminaccess_test

// The admin.elevation event built from a ledger row (ADR-0059 R2.3, issue
// 2517): no database, so these pin what the event carries and refuses.

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func sampleElevationRecord() adminaccess.ElevationRecord {
	return adminaccess.ElevationRecord{
		ID: uuid.MustParse("0b7c2d4e-0000-4000-8000-000000000001"), UserID: 42, BrowserSessionRef: 9,
		Level: adminaccess.ElevationLevelSuperAdmin, Path: adminaccess.ElevationPathApproved,
		ReasonCode: adminaccess.ElevationReasonIdentityOutage, Reason: "MARKER-REASON the IdP is down",
		StartedAt: elevationClock, ExpiresAt: elevationClock.Add(time.Hour),
	}
}

func canonicalOf(t *testing.T, event securityaudit.SecurityEvent) string {
	t.Helper()
	event.ID, event.Source, event.Time = uuid.NewString(), "elitea-main/test", elevationClock
	canonical, err := event.Canonical(1)
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}

// The reason text and the note text never reach the event; the note is its
// id and its length in characters.
func TestTheElevationEventCarriesNoText(t *testing.T) {
	record := sampleElevationRecord()
	started, err := adminaccess.NewElevationEvent(adminaccess.ElevationEvent{
		Action: securityaudit.ElevationStarted, Source: securityaudit.ElevationSourceConsole, Elevation: record,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := canonicalOf(t, started); strings.Contains(body, "MARKER") || !strings.Contains(body, `"identity_outage"`) {
		t.Fatalf("started = %s", body)
	}
	note, err := adminaccess.NewElevationEvent(adminaccess.ElevationEvent{
		Action: securityaudit.ElevationNoteAdded, Source: securityaudit.ElevationSourceConsole, Elevation: record,
		NoteID: "note-1", NoteText: "MARKER-NOTE é",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := canonicalOf(t, note)
	if strings.Contains(body, "MARKER") {
		t.Fatalf("the note text reached the event: %s", body)
	}
	var decoded struct {
		Data struct {
			Elevation struct {
				NoteID     string `json:"note_id"`
				NoteLength int    `json:"note_length"`
			} `json:"elevation"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Data.Elevation.NoteID != "note-1" || decoded.Data.Elevation.NoteLength != 13 {
		t.Fatalf("note = %+v, want note-1 and 13 characters", decoded.Data.Elevation)
	}
}

// The export schema's elevationStep lists exactly the ledger's levels, paths,
// reason codes and end reasons, and the event's sources: a value added on one
// side only fails here, not in a SIEM that validates the export.
func TestTheExportSchemaListsTheLedgerValues(t *testing.T) {
	raw, err := os.ReadFile("../../../../libs/jsonschema/audit/v1/event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	step := schema.Defs["elevationStep"].Properties
	for field, want := range map[string][]string{
		"level": {adminaccess.ElevationLevelAdmin, adminaccess.ElevationLevelSuperAdmin},
		"path":  {adminaccess.ElevationPathApproved, adminaccess.ElevationPathSelf, adminaccess.ElevationPathSealed},
		"reason_code": {
			adminaccess.ElevationReasonIncident, adminaccess.ElevationReasonIdentityOutage,
			adminaccess.ElevationReasonRecovery, adminaccess.ElevationReasonOther,
		},
		"end_reason": {
			adminaccess.ElevationEndHolder, adminaccess.ElevationEndApprover, adminaccess.ElevationEndOperator,
			adminaccess.ElevationEndExpired, adminaccess.ElevationEndLogout, adminaccess.ElevationEndSessionRevoked,
			adminaccess.ElevationEndUserSuspended, adminaccess.ElevationEndUserDeleted, adminaccess.ElevationEndSCIMDeactivated,
		},
		"source": {
			securityaudit.ElevationSourceConsole, securityaudit.ElevationSourceAPI,
			securityaudit.ElevationSourceOperator, securityaudit.ElevationSourceSystem,
		},
	} {
		if got := step[field].Enum; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("elevationStep.%s lists %v, the ledger %v", field, got, want)
		}
	}
}

// A value the ledger does not know is refused before anything is written.
func TestTheElevationEventRefusesUnknownValues(t *testing.T) {
	for name, mutate := range map[string]func(*adminaccess.ElevationEvent){
		"action":      func(e *adminaccess.ElevationEvent) { e.Action = "extended" },
		"source":      func(e *adminaccess.ElevationEvent) { e.Source = "scim" },
		"no source":   func(e *adminaccess.ElevationEvent) { e.Source = "" },
		"level":       func(e *adminaccess.ElevationEvent) { e.Elevation.Level = "owner" },
		"path":        func(e *adminaccess.ElevationEvent) { e.Elevation.Path = "backdoor" },
		"reason code": func(e *adminaccess.ElevationEvent) { e.Elevation.ReasonCode = "boredom" },
		"end reason":  func(e *adminaccess.ElevationEvent) { e.Elevation.EndReason = "whim" },
		"no row":      func(e *adminaccess.ElevationEvent) { e.Elevation = adminaccess.ElevationRecord{} },
		"no end":      func(e *adminaccess.ElevationEvent) { e.Action = securityaudit.ElevationEnded },
	} {
		t.Run(name, func(t *testing.T) {
			ev := adminaccess.ElevationEvent{
				Action: securityaudit.ElevationStarted, Source: securityaudit.ElevationSourceConsole,
				Elevation: sampleElevationRecord(),
			}
			mutate(&ev)
			if _, err := adminaccess.NewElevationEvent(ev); !errors.Is(err, adminaccess.ErrElevationEvent) {
				t.Fatalf("NewElevationEvent = %v, want ErrElevationEvent", err)
			}
		})
	}
}
