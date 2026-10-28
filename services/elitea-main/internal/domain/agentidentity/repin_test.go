package agentidentity

import (
	"errors"
	"testing"
	"time"
)

func TestRepinIsANewProposalOfTheSameBinding(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	active := AgentIdentityBinding{ID: "b", State: BindingActive, DefinitionDigest: [32]byte{1},
		ProposedBy: "100", ApprovedBy: "200", LastTransitionBy: "200", RevocationEpoch: 3}
	next, err := active.Repin([32]byte{2}, "200", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "b" || next.State != BindingProposed || next.DefinitionDigest != ([32]byte{2}) || next.ProposedBy != "200" ||
		next.ApprovedBy != "" || next.LastTransitionBy != "200" || next.RevocationEpoch != 4 || !next.UpdatedAt.Equal(now) {
		t.Fatalf("repinned %+v", next)
	}
	// The re-pinner is the proposer now: they cannot approve it.
	if _, err := next.Transition(ActionApprove, "200", "r", now); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval of a re-pin: %v", err)
	}
	if approved, err := next.Transition(ActionApprove, "100", "r", now); err != nil || approved.State != BindingActive {
		t.Fatalf("second person approval: %+v %v", approved, err)
	}
}

func TestRepinRefusals(t *testing.T) {
	now := time.Now()
	base := AgentIdentityBinding{State: BindingSuspended, DefinitionDigest: [32]byte{1}}
	if _, err := base.Repin([32]byte{1}, "1", now); !errors.Is(err, ErrDefinitionDigestCurrent) {
		t.Fatalf("same digest: %v", err)
	}
	if _, err := base.Repin([32]byte{}, "1", now); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("zero digest: %v", err)
	}
	if _, err := base.Repin([32]byte{2}, " ", now); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("no actor: %v", err)
	}
	revoked := base
	revoked.State = BindingRevoked
	if _, err := revoked.Repin([32]byte{2}, "1", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("revoked: %v", err)
	}
}
