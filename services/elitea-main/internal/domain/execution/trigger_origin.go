package execution

// TriggerOrigin records how a runtime execution started. It is stored on
// elitea_runtime.execution_jobs.trigger_origin (shared migration 0140).
//
// The analytics active-user reads use it (legacy issues 6802 and 6881). An
// unattended run executes as the person who configured it, so billing,
// budgets and counters still charge that person. The active-user figures do
// not count that person as active because of the unattended run.
type TriggerOrigin string

const (
	// TriggerOriginManual is a person who started the run from the UI. It is
	// the column default, so an empty value means this.
	TriggerOriginManual TriggerOrigin = "manual"
	// TriggerOriginAPI is a programmatic client that started the run, such as
	// an MCP client calling tools/call. It is a person's action.
	TriggerOriginAPI TriggerOrigin = "api"
	// TriggerOriginSchedule is a pipeline schedule that fired.
	TriggerOriginSchedule TriggerOrigin = "schedule"
	// TriggerOriginWebhook is an inbound pipeline trigger that fired.
	TriggerOriginWebhook TriggerOrigin = "webhook"
	// TriggerOriginIndex is an index ingest.
	TriggerOriginIndex TriggerOrigin = "index"
)

// Valid reports whether the origin is one the database CHECK accepts. The
// empty value is valid and means TriggerOriginManual.
func (o TriggerOrigin) Valid() bool {
	switch o {
	case "", TriggerOriginManual, TriggerOriginAPI, TriggerOriginSchedule,
		TriggerOriginWebhook, TriggerOriginIndex:
		return true
	default:
		return false
	}
}

// Stored returns the value written to the column. The empty value becomes
// TriggerOriginManual.
func (o TriggerOrigin) Stored() string {
	if o == "" {
		return string(TriggerOriginManual)
	}
	return string(o)
}

// Automated reports whether a person did not start the run directly. The
// analytics active-user reads exclude these runs.
func (o TriggerOrigin) Automated() bool {
	switch o {
	case TriggerOriginSchedule, TriggerOriginWebhook, TriggerOriginIndex:
		return true
	default:
		return false
	}
}
