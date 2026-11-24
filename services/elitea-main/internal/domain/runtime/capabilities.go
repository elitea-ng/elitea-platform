package runtime

import (
	"errors"
	"fmt"
	"slices"
)

const (
	// MaxNegotiatedCapabilities bounds the number of capability identifiers
	// carried on ClaimCommandRequestV1.worker_capabilities and
	// ClaimReceiptV1.server_capabilities.
	MaxNegotiatedCapabilities = 64

	// MaxCapabilityNameBytes bounds the ASCII length of one capability
	// identifier.
	MaxCapabilityNameBytes = 128
)

// Canonical domain-named capabilities exchanged on ClaimCommandRequestV1 and
// ClaimReceiptV1. Names are additive with legacy boolean opt-ins so rolling
// upgrades require no deployment order.
const (
	// Worker -> Main capabilities matching ClaimCommandRequestV1 fields 16-21.
	CapabilityAgentModelCheckpointRecovery = "runtime.agent_model_checkpoint_recovery.v1"
	CapabilityNodeRecovery                 = "runtime.node_recovery.v1"
	CapabilityGovernedSecurityContext      = "runtime.governed_security_context.v1"
	CapabilityFanoutInterruptDecisions     = "runtime.fanout_interrupt_decisions.v1"
	CapabilityFanoutDelegatedCredentials   = "runtime.fanout_delegated_credentials.v1"
	CapabilityIdleCompaction               = "runtime.idle_compaction.v1"

	// Main -> Worker capabilities matching ClaimReceiptV1 fields 18-20.
	CapabilityNegotiation          = "runtime.capability_negotiation.v1"
	CapabilityToolkitTypedRefusals = "runtime.toolkit_typed_refusals.v1"
	CapabilityPausedWriter         = "runtime.paused_writer.v1"
)

var (
	// ErrInvalidCapabilityList marks a capability list that violates count,
	// length, syntax, or uniqueness bounds.
	ErrInvalidCapabilityList = errors.New("runtime capabilities are invalid")

	// ErrMissingRequiredCapability marks a peer that does not advertise a
	// required capability.
	ErrMissingRequiredCapability = errors.New("required runtime capability is not supported")
)

// CapabilitySet holds a validated, deduplicated, deterministically sorted set
// of domain-named runtime capabilities.
type CapabilitySet struct {
	sorted []string
}

// ParseCapabilities validates a wire capability list strictly: at most
// MaxNegotiatedCapabilities unique entries, each 1..MaxCapabilityNameBytes of
// lowercase dot-separated domain syntax.
func ParseCapabilities(raw []string) (CapabilitySet, error) {
	if len(raw) > MaxNegotiatedCapabilities {
		return CapabilitySet{}, fmt.Errorf("%w: count exceeds %d", ErrInvalidCapabilityList, MaxNegotiatedCapabilities)
	}
	if len(raw) == 0 {
		return CapabilitySet{}, nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if !ValidCapabilityName(entry) {
			return CapabilitySet{}, fmt.Errorf("%w: malformed capability name", ErrInvalidCapabilityList)
		}
		if _, dup := seen[entry]; dup {
			return CapabilitySet{}, fmt.Errorf("%w: duplicate capability", ErrInvalidCapabilityList)
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	slices.Sort(out)
	return CapabilitySet{sorted: out}, nil
}

// ValidCapabilityName reports whether name is a bounded lowercase ASCII
// domain-named capability with at least two dot-separated segments (for
// example "runtime.idle_compaction.v1").
func ValidCapabilityName(name string) bool {
	if len(name) == 0 || len(name) > MaxCapabilityNameBytes {
		return false
	}
	segments := 0
	segStart := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' {
			c := name[i]
			if !isLowerAlnum(c) && c != '_' && c != '-' {
				return false
			}
			continue
		}
		segLen := i - segStart
		if segLen == 0 {
			return false
		}
		if !isLowerAlnum(name[segStart]) || !isLowerAlnum(name[i-1]) {
			return false
		}
		segments++
		segStart = i + 1
	}
	return segments >= 2
}

func isLowerAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// Has reports whether the capability is present in the set.
func (s CapabilitySet) Has(name string) bool {
	_, found := slices.BinarySearch(s.sorted, name)
	return found
}

// Len returns the number of capabilities in the set.
func (s CapabilitySet) Len() int {
	return len(s.sorted)
}

// Strings returns a fresh sorted copy of the capabilities in the set.
func (s CapabilitySet) Strings() []string {
	if len(s.sorted) == 0 {
		return nil
	}
	return slices.Clone(s.sorted)
}

// With returns a new CapabilitySet containing all existing capabilities plus
// names (ignoring duplicates already present). It fails closed if any added
// name is malformed or the resulting set exceeds MaxNegotiatedCapabilities.
func (s CapabilitySet) With(names ...string) (CapabilitySet, error) {
	if len(names) == 0 {
		return s, nil
	}
	merged := make([]string, len(s.sorted), min(len(s.sorted)+len(names), MaxNegotiatedCapabilities))
	copy(merged, s.sorted)
	for _, name := range names {
		if !ValidCapabilityName(name) {
			return CapabilitySet{}, fmt.Errorf("%w: malformed capability name", ErrInvalidCapabilityList)
		}
		idx, found := slices.BinarySearch(merged, name)
		if found {
			continue
		}
		if len(merged) >= MaxNegotiatedCapabilities {
			return CapabilitySet{}, fmt.Errorf("%w: count exceeds %d", ErrInvalidCapabilityList, MaxNegotiatedCapabilities)
		}
		merged = slices.Insert(merged, idx, name)
	}
	return CapabilitySet{sorted: merged}, nil
}

// Require checks that every capability in required is present in the set.
func (s CapabilitySet) Require(required ...string) error {
	for _, capName := range required {
		if !s.Has(capName) {
			return fmt.Errorf("%w: %s", ErrMissingRequiredCapability, capName)
		}
	}
	return nil
}
