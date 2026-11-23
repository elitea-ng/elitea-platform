package control

import (
	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// builtInServerCapabilities lists every capability serverCapabilitiesForClaim
// may add on an executable receipt, validated at startup so ExtraServerCapabilities
// can never overflow MaxNegotiatedCapabilities after a lease is allocated.
var builtInServerCapabilities = []string{
	runtimedomain.CapabilityNegotiation,
	runtimedomain.CapabilityToolkitTypedRefusals,
	runtimedomain.CapabilityPausedWriter,
	runtimedomain.CapabilityGovernedSecurityContext,
}

type effectiveWorkerCapabilities struct {
	wire                         runtimedomain.CapabilitySet
	agentModelCheckpointRecovery bool
	nodeRecovery                 bool
	governedSecurityContext      bool
	fanoutInterruptDecisions     bool
	fanoutDelegatedCredentials   bool
	idleCompaction               bool
}

func resolveWorkerCapabilities(request *runtimev1.ClaimCommandRequestV1, wire runtimedomain.CapabilitySet) effectiveWorkerCapabilities {
	return effectiveWorkerCapabilities{
		wire:                         wire,
		agentModelCheckpointRecovery: request.GetAgentModelCheckpointRecovery() || wire.Has(runtimedomain.CapabilityAgentModelCheckpointRecovery),
		nodeRecovery:                 request.GetNodeRecovery() || wire.Has(runtimedomain.CapabilityNodeRecovery),
		governedSecurityContext:      request.GetGovernedSecurityContextV1() || wire.Has(runtimedomain.CapabilityGovernedSecurityContext),
		fanoutInterruptDecisions:     request.GetFanoutInterruptDecisions() || wire.Has(runtimedomain.CapabilityFanoutInterruptDecisions),
		fanoutDelegatedCredentials:   request.GetFanoutDelegatedCredentials() || wire.Has(runtimedomain.CapabilityFanoutDelegatedCredentials),
		idleCompaction:               request.GetIdleCompaction() || wire.Has(runtimedomain.CapabilityIdleCompaction),
	}
}

func (c effectiveWorkerCapabilities) has(name string) bool {
	if c.wire.Has(name) {
		return true
	}
	switch name {
	case runtimedomain.CapabilityAgentModelCheckpointRecovery:
		return c.agentModelCheckpointRecovery
	case runtimedomain.CapabilityNodeRecovery:
		return c.nodeRecovery
	case runtimedomain.CapabilityGovernedSecurityContext:
		return c.governedSecurityContext
	case runtimedomain.CapabilityFanoutInterruptDecisions:
		return c.fanoutInterruptDecisions
	case runtimedomain.CapabilityFanoutDelegatedCredentials:
		return c.fanoutDelegatedCredentials
	case runtimedomain.CapabilityIdleCompaction:
		return c.idleCompaction
	default:
		return false
	}
}

func (s *Server) requiredWorkerCapabilitiesRejection(caps effectiveWorkerCapabilities) *runtimev1.ClaimCommandResponseV1 {
	for _, required := range s.requiredWorkerCaps.Strings() {
		if !caps.has(required) {
			return claimRejection(
				runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY,
				"The claiming worker lacks a required runtime capability.",
				false,
			)
		}
	}
	return nil
}

func (s *Server) serverCapabilitiesForClaim(command *runtimev1.WorkerCommandV1) ([]string, error) {
	caps := s.extraServerCaps
	var err error
	caps, err = caps.With(runtimedomain.CapabilityNegotiation)
	if err != nil {
		return nil, err
	}
	if advertisesTypedToolRefusals(command) {
		caps, err = caps.With(runtimedomain.CapabilityToolkitTypedRefusals)
		if err != nil {
			return nil, err
		}
	}
	if cloudTurnCapability(command.GetCapabilityId()) {
		caps, err = caps.With(runtimedomain.CapabilityPausedWriter)
		if err != nil {
			return nil, err
		}
	}
	if command.GetSecurityContextRef() != nil || s.governedPins != nil {
		caps, err = caps.With(runtimedomain.CapabilityGovernedSecurityContext)
		if err != nil {
			return nil, err
		}
	}
	return caps.Strings(), nil
}
