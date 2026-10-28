package agentidentity

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDefinitionDigestCurrent refuses a re-pin to the digest the binding
// already pins.
var ErrDefinitionDigestCurrent = errors.New("agent identity binding: the binding already pins the computed definition digest")

// Repin moves the binding to a new definition digest (#1462): the computed
// digest of its version after the agent, or a toolkit embedded in its
// definition, was edited. It is a new proposal of the same binding, so the
// two-person rule applies again:
//
//	proposed|active|suspended --repin--> proposed
//
// The actor becomes the proposer and the approval is cleared, so a
// different user must approve it before it binds a run again. The binding
// keeps its id, so its toolkit bindings, grants and mandates still name it;
// none is usable while it is proposed. The epoch increases by one, which
// makes a run admitted under the old pin stale.
func (b AgentIdentityBinding) Repin(digest [32]byte, actor string, now time.Time) (AgentIdentityBinding, error) {
	switch b.State {
	case BindingProposed, BindingActive, BindingSuspended:
	default:
		return AgentIdentityBinding{}, ErrInvalidTransition
	}
	if strings.TrimSpace(actor) == "" {
		return AgentIdentityBinding{}, fmt.Errorf("%w: the actor is unknown", ErrInvalidBinding)
	}
	if digest == ([32]byte{}) {
		return AgentIdentityBinding{}, fmt.Errorf("%w: definition_digest must be a sha256 digest", ErrInvalidBinding)
	}
	if digest == b.DefinitionDigest {
		return AgentIdentityBinding{}, ErrDefinitionDigestCurrent
	}
	out := b
	out.DefinitionDigest = digest
	out.State = BindingProposed
	out.ProposedBy, out.ApprovedBy, out.LastTransitionBy = actor, "", actor
	out.RevocationEpoch = b.RevocationEpoch + 1
	out.UpdatedAt = now
	return out, nil
}
