package execution

import (
	"errors"
	"fmt"
	"unicode/utf8"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	IndexRuntimeIdentitySchemaVersion = "elitea.index.runtime-identity.v1"
	maxIndexRuntimeIdentityBytes      = 256
)

var ErrInvalidIndexRuntimeIdentity = errors.New("invalid index runtime identity binding")

// IndexRuntimeAuthMode is selected once during admission. Runtime resolution
// must never change modes because one credential is unavailable.
type IndexRuntimeAuthMode string

const (
	// IndexRuntimeAuthActorPAT preserves the current path for an interactive
	// actor who owns an active PAT.
	IndexRuntimeAuthActorPAT IndexRuntimeAuthMode = "actor_pat"

	// IndexRuntimeAuthTrustedSessionDelegation is the typed seam for the current
	// no-PAT browser-session branch. The persisted binding contains only a
	// digest of a server-side, short-lived delegation grant. It never contains
	// the browser session, PAT, or another bearer credential.
	IndexRuntimeAuthTrustedSessionDelegation IndexRuntimeAuthMode = "trusted_session_delegation"
)

func (m IndexRuntimeAuthMode) Valid() bool {
	switch m {
	case IndexRuntimeAuthActorPAT, IndexRuntimeAuthTrustedSessionDelegation:
		return true
	default:
		return false
	}
}

// IndexRuntimeIdentityBinding is immutable admission evidence. Its duplicated
// identity fields intentionally bind the selected mode to one execution,
// generation, tenant, project, and actor. Resolve must compare it with the
// durable Job before issuing any runtime credential.
//
// This type has no transport tags: it is not a bus command or worker payload.
type IndexRuntimeIdentityBinding struct {
	SchemaVersion         string
	ExecutionID           string
	Generation            uint64
	TenantID              string
	ResourceProjectID     string
	ActorID               string
	Mode                  IndexRuntimeAuthMode
	DelegationGrantDigest runtimedomain.Digest
}

func NewIndexRuntimeIdentityBinding(
	job Job,
	mode IndexRuntimeAuthMode,
	delegationGrantDigest runtimedomain.Digest,
) (IndexRuntimeIdentityBinding, error) {
	binding := IndexRuntimeIdentityBinding{
		SchemaVersion:         IndexRuntimeIdentitySchemaVersion,
		ExecutionID:           job.ID,
		Generation:            job.Generation,
		TenantID:              job.TenantID,
		ResourceProjectID:     job.ResourceProjectID,
		ActorID:               job.ActorID,
		Mode:                  mode,
		DelegationGrantDigest: delegationGrantDigest,
	}
	if err := binding.ValidateAgainst(job); err != nil {
		return IndexRuntimeIdentityBinding{}, err
	}
	return binding, nil
}

func (b IndexRuntimeIdentityBinding) ValidateAgainst(job Job) error {
	if b.SchemaVersion != IndexRuntimeIdentitySchemaVersion ||
		!b.Mode.Valid() ||
		!validIndexRuntimeIdentity(b.ExecutionID) ||
		!validIndexRuntimeIdentity(b.TenantID) ||
		!validIndexRuntimeIdentity(b.ResourceProjectID) ||
		!validIndexRuntimeIdentity(b.ActorID) ||
		b.Generation == 0 {
		return ErrInvalidIndexRuntimeIdentity
	}
	if job.CapabilityID != IndexIngestCapability ||
		b.ExecutionID != job.ID ||
		b.Generation != job.Generation ||
		b.TenantID != job.TenantID ||
		b.ResourceProjectID != job.ResourceProjectID ||
		b.ActorID != job.ActorID {
		return fmt.Errorf("%w: durable execution identity mismatch", ErrInvalidIndexRuntimeIdentity)
	}
	switch b.Mode {
	case IndexRuntimeAuthActorPAT:
		if !b.DelegationGrantDigest.IsZero() {
			return fmt.Errorf("%w: actor PAT mode contains delegation evidence", ErrInvalidIndexRuntimeIdentity)
		}
	case IndexRuntimeAuthTrustedSessionDelegation:
		if b.DelegationGrantDigest.IsZero() {
			return fmt.Errorf("%w: trusted-session mode lacks delegation evidence", ErrInvalidIndexRuntimeIdentity)
		}
	default:
		return ErrInvalidIndexRuntimeIdentity
	}
	return nil
}

func validIndexRuntimeIdentity(value string) bool {
	return value != "" &&
		len(value) <= maxIndexRuntimeIdentityBytes &&
		utf8.ValidString(value)
}
