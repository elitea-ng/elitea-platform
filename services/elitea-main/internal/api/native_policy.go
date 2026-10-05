package api

import (
	"context"

	v2discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
)

// The discovery consumer of the native client policy (ADR-0025 WP4), adapted
// from the ONE nativepolicy.Service the router holds — the token response
// (Service.Decorate) and the 426 gate (Service.MinimumFor) read the same
// instance, so the three cannot disagree about the policy.

// nativePolicyDiscovery is the discovery document's ClientPolicySource.
type nativePolicyDiscovery struct{ policy *nativepolicy.Service }

// PublicPolicy implements v2discovery.ClientPolicySource: the public subset
// (decision 5 names no subset; this one is what a client may act on BEFORE
// sign-in — whether it may run without a device lock, may store anything
// offline, and how old it may be) and the per-client effective minimums.
func (d nativePolicyDiscovery) PublicPolicy(ctx context.Context) (v2discovery.PublicPolicy, map[string]string, error) {
	policy, minimums, err := d.policy.Minimums(ctx)
	if err != nil {
		return v2discovery.PublicPolicy{}, nil, err
	}
	return v2discovery.PublicPolicy{
		RequireDeviceLock: policy.RequireDeviceLock,
		OfflineEnabled:    policy.OfflineEnabled(),
		MinClientVersion:  policy.MinClientVersion,
	}, minimums, nil
}
