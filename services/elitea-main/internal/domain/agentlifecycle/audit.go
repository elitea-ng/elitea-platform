package agentlifecycle

// The security audit actions (ADR-0039 outbox, event type
// securityaudit.TypeAgentLifecycle). Each one is appended in the same
// transaction as the change it records.
const (
	ActionChangeSubmitted        = "agent.change.submitted"
	ActionChangeApproved         = "agent.change.approved"
	ActionChangeRejected         = "agent.change.rejected"
	ActionChangeWithdrawn        = "agent.change.withdrawn"
	ActionChangePublished        = "agent.change.published"
	ActionChangeGateOverride     = "agent.change.gate_overridden"
	ActionRegistryTierChanged    = "agent.registry.tier_changed"
	ActionCertificationDue       = "agent.certification.due"
	ActionCertificationLapsed    = "agent.certification.lapsed"
	ActionCertificationSuspended = "agent.certification.suspended"
	ActionCertificationCert      = "agent.certification.certified"
	ActionAttestationRecorded    = "agent.attestation.recorded"
	ActionAttestationRevoked     = "agent.attestation.revoked"
	ActionGateSuiteUpdated       = "agent.gate_suite.updated"
)

// The reason codes of a certification moving to due or lapsed. They go into
// the audit event's reasons and the registry's state_reason.
const (
	ReasonRecertifyWindow   = "recertify_window"
	ReasonNewRelease        = "new_release"
	ReasonToolkitDrifted    = "release_toolkit_drifted"
	ReasonGateRegressed     = "evaluation_regressed"
	ReasonExternal          = "external_mark"
	ReasonRecertifyByPassed = "recertify_by_passed"
	ReasonLapseSuspend      = "lapse_action_suspend"
	ReasonLapseWarn         = "lapse_action_warn"
	// ReasonTierWithoutCadence returns a due, lapsed or suspended agent to
	// certified when its tier moves to one with no recertification cadence.
	ReasonTierWithoutCadence = "tier_without_cadence"
)
