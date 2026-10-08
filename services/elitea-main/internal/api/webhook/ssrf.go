package webhook

// SSRF hardening for outbound webhook destinations.
//
// #876 accepted any `url` a caller supplied and dialled it unchecked. The
// DNS-aware, dial-time guard that closed that gap now lives in
// internal/infra/egress, so infrastructure adapters (internal/infra/storage)
// can use the same guard without importing the api layer. These aliases keep
// every existing caller of the webhook names unchanged.

import (
	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// DestinationAllowlistEnv names the variable an operator sets to permit
// tenant webhooks to reach private-network destinations (an internal
// receiver, a service mesh host, a lab endpoint). Spelled out as a literal
// because the env-drift gate and the chart are both searched by the string.
//
// UNSET (the default): every private-network and loopback destination is
// refused. Link-local, metadata, multicast and reserved addresses are refused
// NO MATTER WHAT this variable says (see egress.Guard).
const DestinationAllowlistEnv = "ELITEA_WEBHOOK_EGRESS_ALLOWLIST"

// DestinationGuard is the webhook name for egress.Guard.
type DestinationGuard = egress.Guard

// ErrDestinationRefused is egress.ErrDestinationRefused, so errors.Is works
// with either name.
var ErrDestinationRefused = egress.ErrDestinationRefused

// NewDestinationGuard builds a guard over an allowlist (nil refuses every
// private destination).
func NewDestinationGuard(allowlist *egresslib.Allowlist) *DestinationGuard {
	return egress.New(allowlist)
}

// NewDestinationGuardWithResolver is NewDestinationGuard with an injectable
// resolver, for tests that must not depend on live DNS.
func NewDestinationGuardWithResolver(allowlist *egresslib.Allowlist, resolver egress.Resolver) *DestinationGuard {
	return egress.NewWithResolver(allowlist, resolver)
}

// ParseDestinationAllowlist reads DestinationAllowlistEnv's grammar.
func ParseDestinationAllowlist(raw []string) (*egresslib.Allowlist, error) {
	return egress.ParseAllowlist(raw)
}
