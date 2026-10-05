package runtime

import (
	"errors"
	"time"
)

var (
	ErrInvalidFence = errors.New("invalid execution fence")
	ErrStaleFence   = errors.New("stale execution fence")
	ErrLeaseExpired = errors.New("execution lease expired")
)

type FenceToken [32]byte

func (t FenceToken) IsZero() bool {
	return t == FenceToken{}
}

type DesiredState string

const (
	DesiredRunning   DesiredState = "RUNNING"
	DesiredCancelled DesiredState = "CANCELLED"
	DesiredDraining  DesiredState = "DRAINING"
	DesiredSuspended DesiredState = "SUSPENDED"
)

func (s DesiredState) Valid() bool {
	return s == DesiredRunning || s == DesiredCancelled || s == DesiredDraining || s == DesiredSuspended
}

// Fence names one claim generation. Every output and control mutation must
// carry all fields; execution ID alone is not authority.
type Fence struct {
	CommandID   string
	ExecutionID string
	Generation  uint64
	// WorkloadIdentity is derived from the authenticated transport peer (for
	// example its SPIFFE ID). It is never copied from a command or frame.
	WorkloadIdentity  string
	WorkloadSessionID string
	ProducerID        string
	ClaimAttempt      uint64
	// LeaseEpoch is monotonic across replacement claims and stable while the
	// same claim renews; the v1 renewal response extends time but cannot rotate
	// the authority-bearing fence.
	LeaseEpoch uint64
	Token      FenceToken
}

func (f Fence) Validate() error {
	if f.CommandID == "" || f.ExecutionID == "" || f.Generation == 0 || f.WorkloadIdentity == "" || f.WorkloadSessionID == "" || f.ProducerID == "" || f.ClaimAttempt == 0 || f.LeaseEpoch == 0 || f.Token.IsZero() {
		return ErrInvalidFence
	}
	return nil
}

type ActiveLease struct {
	ClaimID      string
	Fence        Fence
	ExpiresAt    time.Time
	DesiredState DesiredState
}

func (l ActiveLease) Verify(now time.Time, fence Fence) error {
	if l.ClaimID == "" || !l.DesiredState.Valid() {
		return ErrInvalidFence
	}
	if err := fence.Validate(); err != nil {
		return err
	}
	if l.Fence != fence {
		return ErrStaleFence
	}
	if !now.Before(l.ExpiresAt) {
		return ErrLeaseExpired
	}
	return nil
}
