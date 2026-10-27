// Package reconcile finds out-of-band changes to external agent identities
// (ADR-0039 decision 11, issue #1154). No identity provider documents change
// notifications for agent objects, so a loop compares Elitea's
// ExternalAgentIdentity records with each configured provider's live state
// and fails closed on drift:
//
//	live state                                      action
//	disabled outside Elitea, record active          suspended, epoch +1, drift.disabled
//	not found, record active                        suspended, epoch +1, drift.missing
//	immutable id or generation mismatch             replaced (terminal), epoch +1, drift.replaced
//	credential or owner change (where reported),
//	record active                                   suspended, epoch +1, drift.changed
//	Elitea-tagged object with no record             audit external_identity.orphan only
//
// Every transition goes through the record's own state machine
// (identity.ExternalAgentIdentity.Transition) inside the store's locked
// transition, so its invariants and its TypeExternalIdentityChanged event
// apply; the drift event and the finding are written in the same
// transaction. Every enforcement point re-reads the record's state and epoch
// from the database, so the commit alone stops token issuance. Notifier, when
// set, is told after the commit, for latency only.
//
// A provider outage changes no state. After the policy's outage threshold of
// consecutive failed passes, the provider's persisted health denies new
// agent sessions (agentidentity.ProviderAdmission) until a pass succeeds.
//
// One replica runs a pass per provider per interval: the pass lease is a row
// per provider, claimed with a conditional upsert (Store.ClaimPass).
//
// Recovery is an administrator action. Nothing here re-provisions or deletes
// a provider object.
package reconcile

import (
	"context"
	"time"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// FindingKind is the kind of one drift finding.
type FindingKind string

const (
	KindDisabled FindingKind = "disabled"
	KindMissing  FindingKind = "missing"
	KindReplaced FindingKind = "replaced"
	KindChanged  FindingKind = "changed"
	KindOrphan   FindingKind = "orphan"
)

// FindingKinds is every kind, in a stable order.
func FindingKinds() []FindingKind {
	return []FindingKind{KindDisabled, KindMissing, KindReplaced, KindChanged, KindOrphan}
}

// Valid reports whether k is a modelled kind.
func (k FindingKind) Valid() bool {
	for _, v := range FindingKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// Action is the security audit action of the kind.
func (k FindingKind) Action() string {
	if k == KindOrphan {
		return "external_identity.orphan"
	}
	return "external_identity.drift." + string(k)
}

// Finding is one open drift finding. It holds references and epochs only.
type Finding struct {
	ID             string
	Kind           FindingKind
	Provider       string
	ProviderTenant string
	ExternalID     string
	// ExternalAgentIdentityID, TenantProjectID and ApplicationID are empty
	// (zero) for an orphan.
	ExternalAgentIdentityID string
	TenantProjectID         int64
	ApplicationID           int64
	// RevocationEpoch is the record's epoch the finding's transition wrote.
	RevocationEpoch int64
	DetectedAt      time.Time
	LastSeenAt      time.Time
}

// ProviderHealth is the persisted reconcile health of one provider.
type ProviderHealth struct {
	Provider            string
	ConsecutiveFailures int
	// AdmissionDenied is true from the threshold's failed pass until the next
	// successful one. agentidentity.Broker.OpenSession denies the provider
	// meanwhile.
	AdmissionDenied bool
	LastPassAt      time.Time // zero: no pass yet
	LastSuccessAt   time.Time // zero: no successful pass yet
}

// DriftChange is one drift transition for Store.ApplyDrift.
type DriftChange struct {
	// Finding is the finding to open (or refresh); its RevocationEpoch is
	// filled from the written record.
	Finding Finding
	// Apply is the transition the store runs on the locked row.
	Apply func(current identity.ExternalAgentIdentity, activeBindings int) (identity.ExternalAgentIdentity, error)
	// Event builds the drift audit event from the written record.
	Event func(written identity.ExternalAgentIdentity) securityaudit.SecurityEvent
}

// Store is the reconcile persistence. The PostgreSQL implementation is
// repos.AgentIdentityReconcile.
type Store interface {
	// ClaimPass takes the provider's pass lease for lease when nobody holds
	// it, and answers whether it did. Two replicas never both get it.
	ClaimPass(ctx context.Context, provider, holder string, lease time.Duration) (bool, error)
	// Records answers every record of the provider, in any state.
	Records(ctx context.Context, provider string) ([]identity.ExternalAgentIdentity, error)
	// ApplyDrift runs change.Apply on the locked record through the record's
	// transition path (with its TypeExternalIdentityChanged event), opens or
	// refreshes the finding and appends change.Event, in one transaction.
	// Apply returning identity.ErrExternalTransition writes nothing.
	ApplyDrift(ctx context.Context, id string, change DriftChange) (identity.ExternalAgentIdentity, error)
	// RecordOrphan opens the orphan finding and appends event in one
	// transaction, and answers true; an orphan already open is refreshed and
	// answers false with no event.
	RecordOrphan(ctx context.Context, finding Finding, event securityaudit.SecurityEvent) (bool, error)
	// OpenFindings answers the open findings, of provider or of every
	// provider when it is empty, newest first.
	OpenFindings(ctx context.Context, provider string, limit, offset int) ([]Finding, error)
	// ResolveFindings closes findings.
	ResolveFindings(ctx context.Context, ids []string) error
	// RecordPass records a pass result. A failed pass increments the
	// provider's consecutive failures and denies admission at threshold; a
	// successful one resets both.
	RecordPass(ctx context.Context, provider string, ok bool, threshold int) (ProviderHealth, error)
	// Health answers every provider's health, by name.
	Health(ctx context.Context) ([]ProviderHealth, error)
}

// EpochKindExternalIdentity is EpochChange.Kind of an external agent
// identity.
const EpochKindExternalIdentity = "external_identity"

// EpochChange is one committed epoch bump.
type EpochChange struct {
	Kind  string
	ID    string
	Epoch int64
}

// Notifier is told about every epoch bump a pass committed, after the
// commit. It is the latency seam for the credential broker's cache
// invalidation (#1121): correctness does not depend on it, because every
// enforcement point re-reads the epoch from the record. It must not block.
type Notifier interface {
	EpochChanged(ctx context.Context, change EpochChange)
}
