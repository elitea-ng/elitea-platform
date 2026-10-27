package credentialadapters

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// ProbeOutcome is what a probe found. It is the whole answer: a probe
// result carries no provider body, no header and no material.
type ProbeOutcome string

const (
	// ProbeSucceeded: the provider accepted the credential for the probe
	// operation.
	ProbeSucceeded ProbeOutcome = "succeeded"
	// ProbeRejected: the provider refused the credential (401/403 class).
	ProbeRejected ProbeOutcome = "rejected"
	// ProbeUnreachable: the target did not answer, or answered with an error
	// that says nothing about the credential.
	ProbeUnreachable ProbeOutcome = "unreachable"
	// ProbeUnavailable: no probe runs for this provider here.
	ProbeUnavailable ProbeOutcome = "unavailable"
)

// Valid reports whether o is a modelled outcome.
func (o ProbeOutcome) Valid() bool {
	switch o {
	case ProbeSucceeded, ProbeRejected, ProbeUnreachable, ProbeUnavailable:
		return true
	}
	return false
}

// ProbeRequest is one probe of a credential reference. It names the
// reference only; the Prober takes custody of the material itself, for the
// one call, and drops it before returning.
type ProbeRequest struct {
	TenantProjectID int64
	Reference       credentialgrant.CredentialReference
	Operation       credentialgrant.OperationID
	// Descriptor is a copy of the provider's descriptor.
	Descriptor AdapterDescriptor
}

// ProbeResult is the outcome and, when the provider answered, its HTTP
// status. Nothing else crosses back.
type ProbeResult struct {
	Outcome        ProbeOutcome
	ProviderStatus int
}

// Prober runs a descriptor's probe operation server side with transient
// custody of the material. An error is an internal failure, never shown to
// the caller.
type Prober interface {
	Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error)
}

// UnavailableProber is the fail-closed default: it reads no material and
// calls no provider, and answers ProbeUnavailable for every request.
type UnavailableProber struct{}

// Probe answers ProbeUnavailable.
func (UnavailableProber) Probe(context.Context, ProbeRequest) (ProbeResult, error) {
	return ProbeResult{Outcome: ProbeUnavailable}, nil
}
