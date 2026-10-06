package runtime

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSnapshotMiss        = errors.New("compiled snapshot index miss")
	ErrSnapshotUnavailable = errors.New("compiled snapshot is not ready")
	ErrSnapshotConflict    = errors.New("compiled snapshot provenance conflict")
	ErrSnapshotQuota       = errors.New("compiled snapshot quota exceeded")
)

type SnapshotScope struct {
	TenantID  string
	ProjectID int32
}

func (s SnapshotScope) Validate() error {
	if !snapshotIdentity(s.TenantID, false) || s.ProjectID <= 0 {
		return ErrSnapshotInvalid
	}
	return nil
}

// SnapshotCandidate is resolved from the original fenced supervisor ledger.
// ReceiptSHA256 is empty only while the compile owner retains captured output.
type SnapshotCandidate struct {
	Scope                    SnapshotScope
	Key                      string
	Root                     string
	DescriptorJSON           []byte
	CompilationJobKey        string
	CompilationRequestDigest string
	CompilationRuntimeID     string
	// CompilationLeaseEpoch is immutable export provenance; current worker leases stay separate.
	CompilationLeaseEpoch uint64
	ReceiptSHA256         string
	// CurrentLeaseLive is transient staging state, never immutable provenance.
	CurrentLeaseLive bool
}

func (c SnapshotCandidate) Descriptor() (RustSnapshotDescriptor, error) {
	d, err := ParseRustSnapshotDescriptor(c.DescriptorJSON, c.Root)
	if err != nil || c.Scope.Validate() != nil || d.Binding.TenantID != c.Scope.TenantID || d.Binding.ProjectID != c.Scope.ProjectID || d.SnapshotKeySHA256 != c.Key || !SnapshotDigest(c.CompilationJobKey) || !SnapshotDigest(c.CompilationRequestDigest) || c.CompilationRuntimeID == "" || len(c.CompilationRuntimeID) > 512 || c.CompilationLeaseEpoch == 0 || c.ReceiptSHA256 != "" && !SnapshotDigest(c.ReceiptSHA256) {
		return d, ErrSnapshotInvalid
	}
	expected, err := SnapshotJobDigest("compile", d.Binding, "")
	if err != nil || expected != c.CompilationRequestDigest {
		return d, ErrSnapshotInvalid
	}
	return d, nil
}
func (c SnapshotCandidate) SameArtifact(other SnapshotCandidate) bool {
	return c.Scope == other.Scope && c.Key == other.Key && c.Root == other.Root && string(c.DescriptorJSON) == string(other.DescriptorJSON) && c.CompilationJobKey == other.CompilationJobKey && c.CompilationRequestDigest == other.CompilationRequestDigest && c.CompilationRuntimeID == other.CompilationRuntimeID
}

// Every callback runs while a durable row lock prevents eviction or replacement.
// CommitReady revalidates the successful original receipt and cleanup itself.
// Implementations never promote a publishing row on file-upload success alone.
type SnapshotExecution struct {
	DescriptorJSON []byte
	Root           string
	Key            string
	RuntimeID      string
}

// OriginalExecution returns existing dispatched/terminal execution authority.
// It grants no content read and cannot authorize a new runtime after cache expiry.
type RustSnapshotIndex interface {
	OriginalExecution(context.Context, SnapshotScope, string, string, string) (SnapshotExecution, bool, error)
	Candidate(context.Context, SnapshotScope, string, string, bool) (SnapshotCandidate, error)
	Ready(context.Context, SnapshotScope, string) (SnapshotCandidate, error)
	Reserve(context.Context, SnapshotCandidate) error
	WithPublishing(context.Context, SnapshotCandidate, func() error) error
	WithReady(context.Context, SnapshotScope, string, string, func(SnapshotCandidate) error) error
	CommitReady(context.Context, SnapshotCandidate, func() error) error
}

type SnapshotEviction struct {
	Candidate SnapshotCandidate
	Owner     string
	Epoch     uint64
}
type RustSnapshotEvictor interface {
	ClaimExpired(context.Context, string, int, time.Duration) ([]SnapshotEviction, error)
	CompleteEviction(context.Context, SnapshotEviction, func() error) error
}
