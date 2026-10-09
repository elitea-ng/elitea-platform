// Package executioninterrupt owns the per-interrupt HITL decision contract
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md): its limits, strict
// decoding of cards, decisions and ACKs, and the contract canonical JSON form.
package executioninterrupt

// Every bound of the ledger and its API lives here. The test
// TestExecutionInterruptLimitsMatchContractSchemas pins each one that a schema
// expresses to the libs/jsonschema/runtime/v1/fanout-*.schema.json bound, so
// Go, Rust and the schemas cannot drift.
const (
	// MaxOpenInterrupts caps PENDING plus DECIDED cards per root response
	// (contract §5 "Open cap", user decision 2026-10-08).
	MaxOpenInterrupts = 16
	// MaxDecisionBodyBytes caps the raw and the canonical decision body.
	MaxDecisionBodyBytes = 8192
	// MaxDecisionValueChars caps the decision value in characters.
	MaxDecisionValueChars = 8192
	// MaxCardBytes caps the canonical card bytes Main stores.
	MaxCardBytes = 32768
	// MaxInterruptIDBytes caps the printable-ASCII Worker interrupt id.
	MaxInterruptIDBytes = 512
	// MaxAvailableActions caps the actions one card offers.
	MaxAvailableActions = 4
	// MaxHierarchyTiers caps parent_agent_path.
	MaxHierarchyTiers = 3
	// MaxFanoutOrdinal is the largest member ordinal (Map). Parallel stops at 16.
	MaxFanoutOrdinal = 64
	// MaxFetchDecisions caps the decisions one private fetch returns.
	MaxFetchDecisions = 16
	// MaxFetchEntryBytes caps one canonical fetch entry.
	MaxFetchEntryBytes = 65536
	// MinCredentialRefBytes and MaxCredentialRefBytes bound the opaque
	// token-store reference of a delegated-auth decision.
	MinCredentialRefBytes = 16
	MaxCredentialRefBytes = 128
	// MaxAckBodyBytes caps the raw private ACK body. The largest valid ACK is
	// under 500 bytes.
	MaxAckBodyBytes = 1024
	// MaxCheckpointIDBytes caps the ACKed child checkpoint id.
	MaxCheckpointIDBytes = 128
	// MaxFrontierBytes caps the private frontier JSON.
	MaxFrontierBytes = 2048
	// MaxChildThreadBytes caps the frontier's frozen child thread.
	MaxChildThreadBytes = 512
	// MaxFanoutNodeBytes caps the frontier's fan-out node id.
	MaxFanoutNodeBytes = 128
	// MaxSourceEventIDBytes caps the accepted frame's event id.
	MaxSourceEventIDBytes = 256
	// MaxClaimIDBytes caps an execution claim id in the audit.
	MaxClaimIDBytes = 256
	// MaxJSONDepth caps nesting in every decoded body.
	MaxJSONDepth = 8
)
