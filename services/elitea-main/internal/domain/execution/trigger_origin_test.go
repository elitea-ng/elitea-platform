package execution

import (
	"testing"
	"time"
)

func TestTriggerOriginMatchesTheDatabaseCheck(t *testing.T) {
	t.Parallel()

	// The CHECK in shared/0140_execution_trigger_origin.sql.
	stored := []TriggerOrigin{
		TriggerOriginManual, TriggerOriginAPI, TriggerOriginSchedule,
		TriggerOriginWebhook, TriggerOriginIndex,
	}
	for _, origin := range stored {
		if !origin.Valid() {
			t.Errorf("%q is not valid", origin)
		}
		if origin.Stored() != string(origin) {
			t.Errorf("%q is stored as %q", origin, origin.Stored())
		}
	}
	if !TriggerOrigin("").Valid() || TriggerOrigin("").Stored() != "manual" {
		t.Error("the empty origin must be valid and stored as manual")
	}
	if TriggerOrigin("cron").Valid() {
		t.Error("an unknown origin is valid")
	}
}

// Only the three unattended origins are automated. `api` is a person acting
// through a client, which legacy issue 6802 counts as user activity.
func TestTriggerOriginAutomated(t *testing.T) {
	t.Parallel()

	want := map[TriggerOrigin]bool{
		"": false, TriggerOriginManual: false, TriggerOriginAPI: false,
		TriggerOriginSchedule: true, TriggerOriginWebhook: true, TriggerOriginIndex: true,
	}
	for origin, automated := range want {
		if origin.Automated() != automated {
			t.Errorf("%q.Automated() = %v, want %v", origin, origin.Automated(), automated)
		}
	}
}

func TestJobValidateRejectsAnUnknownTriggerOrigin(t *testing.T) {
	t.Parallel()

	job := Job{
		ID: "e", CommandID: "c", TenantID: "1", ResourceProjectID: "1",
		ProjectionProjectID: "1", ActorID: "1", CapabilityID: AgentApplicationCapability,
		Generation: 1, State: JobPending, CreatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("baseline Validate() = %v", err)
	}
	job.TriggerOrigin = "cron"
	if job.Validate() == nil {
		t.Fatal("an unknown trigger origin passed Validate")
	}
}
