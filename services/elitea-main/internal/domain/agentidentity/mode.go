// Package agentidentity holds the non-human agent principal of ADR-0039:
// the identity modes, the per-version AgentIdentityBinding and its state
// machine. It stores references only, never token or secret material.
package agentidentity

// IdentityMode is how an agent version obtains its external identity
// (ADR-0039 decision 2). A policy revision lists the modes it allows; the
// default revision allows none until gate D2 is decided.
type IdentityMode string

const (
	// IdentityModeDedicatedAgent is one external identity per deployed agent.
	IdentityModeDedicatedAgent IdentityMode = "dedicated_agent"
	// IdentityModeRuntimeDomainBinding is an approved runtime-domain identity
	// plus a trusted logical-agent binding.
	IdentityModeRuntimeDomainBinding IdentityMode = "runtime_domain_binding"
	// IdentityModeEliteaNative makes Elitea the issuer of a short-lived
	// per-redemption agent assertion (ADR-0039 decision 12).
	IdentityModeEliteaNative IdentityMode = "elitea_native"
)

// IdentityModes is every modelled mode, in a stable order.
func IdentityModes() []IdentityMode {
	return []IdentityMode{
		IdentityModeDedicatedAgent,
		IdentityModeRuntimeDomainBinding,
		IdentityModeEliteaNative,
	}
}

// Valid reports whether m is a modelled mode.
func (m IdentityMode) Valid() bool {
	switch m {
	case IdentityModeDedicatedAgent, IdentityModeRuntimeDomainBinding, IdentityModeEliteaNative:
		return true
	}
	return false
}
