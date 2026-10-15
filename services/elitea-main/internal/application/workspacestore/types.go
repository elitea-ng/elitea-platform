// Package workspacestore is the Elitea workspace store (ADR-0034 decision 2):
// git bundles ("segments") in the hidden per-project workspace bucket, and the
// thread's refs and segment log in PostgreSQL.
//
// There is no git server. A writer uploads one immutable, content-addressed
// segment through an upload grant, then publishes it: Main checks the stored
// object (size and SHA-256) BEFORE it opens a transaction, and the publish is
// one short transaction that locks the thread's handoff row, checks the state
// and the epoch, applies the ref allowlist of the writer, compares every
// expected old ref value (all or nothing), inserts the segment with the next
// per-thread seq and moves the refs. No storage call runs inside it
// (decision 15).
//
// The desktop REST routes (internal/api/v2/workspacehandoff) and the later
// worker gRPC calls (WorkspaceFetchPlan, WorkspaceUpload, WorkspacePublish)
// are thin adapters over the same Service methods: the Caller says which
// writer is asking.
package workspacestore

import (
	"errors"
	"regexp"
	"time"
)

// State is a thread's ownership state (ADR-0034 decision 7).
type State string

const (
	StateLocal        State = "LOCAL"
	StateHandingOff   State = "HANDING_OFF"
	StateCloudIdle    State = "CLOUD_IDLE"
	StateCloudRunning State = "CLOUD_RUNNING"
	StateReclaiming   State = "RECLAIMING"
)

// Cloud reports whether the cloud executor owns the thread.
func (s State) Cloud() bool { return s == StateCloudIdle || s == StateCloudRunning }

// Writer is who publishes a segment.
type Writer string

const (
	// WriterDesktop is the thread owner's desktop, during HANDING_OFF.
	WriterDesktop Writer = "desktop"
	// WriterCloudExecutor is the cloud turn's worker, during CLOUD_RUNNING.
	WriterCloudExecutor Writer = "cloud_executor"
)

// The thread's refs (ADR-0034 decision 2). Phase 1 sends one snapshot per
// handoff, so the handoff ref is always N = 1.
const (
	RefBase     = "refs/elitea/base"
	RefUpstream = "refs/elitea/upstream"
	RefHandoff1 = "refs/elitea/handoff/1"
	RefCloud    = "refs/elitea/cloud"
)

const (
	// KindCompact is a full bundle with no prerequisites, the only kind Phase 1
	// publishes.
	KindCompact = "COMPACT"
	// ObjectFormatSHA1 is the only git object format Phase 1 accepts.
	ObjectFormatSHA1 = "sha1"
	// MediaType is the Content-Type of every segment upload.
	MediaType = "application/x-git-bundle"

	// GrantTTL is the lifetime of an upload grant and of a presigned URL.
	GrantTTL = 15 * time.Minute
	// MultipartThreshold: a segment above it is uploaded in parts on a backend
	// with native multipart (the ADR-0016 S16 threshold).
	MultipartThreshold = 64 << 20
	// PartSize is the size of each part but the last.
	PartSize = 64 << 20
	// SupersededGrace is how long a superseded segment's object stays, so
	// readers in flight can finish (decision 12).
	SupersededGrace = 24 * time.Hour
	// OrphanGrace is the age after which an object with no live segment row
	// is deleted (decision 12).
	OrphanGrace = 24 * time.Hour
)

// Limits are the effective caps of a thread's project (decisions 5 and 11).
type Limits struct {
	MaxSegmentBytes          int64
	MaxFileBytes             int64
	MaxFiles                 int64
	MaxUnpackedBytes         int64
	MaxProjectWorkspaceBytes int64
}

// Thread names one Local work thread.
type Thread struct {
	ProjectID        int64
	ConversationUUID string
}

// Caller is the writer or reader acting on a thread. A desktop caller is the
// thread owner (UserID); a cloud executor caller names the running turn
// (ExecutionID), which the later gRPC adapter takes from the claim.
type Caller struct {
	Writer      Writer
	UserID      int64
	ExecutionID string
}

// RefUpdate is one ref move of a publish. Old nil means "the ref must not
// exist yet".
type RefUpdate struct {
	Name string
	Old  *string
	New  string
}

var (
	ErrInvalid = errors.New("invalid workspace store request")
	// ErrNotFound: no handoff for this thread, or the caller is not its owner
	// (one answer for both).
	ErrNotFound = errors.New("workspace thread not found")
	// ErrStaleEpoch: the epoch, the state or the running turn no longer
	// matches the caller.
	ErrStaleEpoch = errors.New("stale workspace epoch")
	// ErrRefNotAllowed: the writer may not move this ref in this state.
	ErrRefNotAllowed = errors.New("ref not allowed for this writer")
	// ErrSegmentMissing: the declared segment is not stored.
	ErrSegmentMissing = errors.New("segment object is missing")
	// ErrChecksumMismatch: the stored object's size or SHA-256 differs from the
	// declared one.
	ErrChecksumMismatch = errors.New("segment checksum mismatch")
	// ErrUnsupportedObjectFormat: Phase 1 accepts sha1 repositories only.
	ErrUnsupportedObjectFormat = errors.New("unsupported git object format")
	// ErrSegmentTooLarge: the segment is over the effective max_segment_mb.
	ErrSegmentTooLarge = errors.New("segment too large")
	// ErrQuotaExceeded: the project's workspace copies would pass
	// max_project_workspace_gb.
	ErrQuotaExceeded = errors.New("project workspace quota exceeded")
	// ErrUnavailable: the object store is not configured.
	ErrUnavailable = errors.New("workspace store is unavailable")
)

// RefConflictError is a failed compare-and-set. Current holds the stored
// value (nil when absent) of every ref the publish named.
type RefConflictError struct {
	Current map[string]*string
}

func (e *RefConflictError) Error() string { return "workspace ref conflict" }

// Is lets errors.Is(err, ErrRefConflict) match.
func (e *RefConflictError) Is(target error) bool { return target == ErrRefConflict }

// ErrRefConflict is the sentinel RefConflictError matches.
var ErrRefConflict = errors.New("workspace ref conflict")

var (
	sha1Pattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	refNamePattern = regexp.MustCompile(`^refs/elitea/(base|upstream|cloud|handoff/[1-9][0-9]{0,8})$`)
)

// ValidSHA1 reports a lowercase hex git sha1.
func ValidSHA1(value string) bool { return sha1Pattern.MatchString(value) }

// ValidSHA256 reports a lowercase hex SHA-256 digest.
func ValidSHA256(value string) bool { return sha256Pattern.MatchString(value) }

// ValidUUID reports a lowercase canonical UUID.
func ValidUUID(value string) bool { return uuidPattern.MatchString(value) }

// ValidRefName reports a ref of the thread's namespace.
func ValidRefName(value string) bool { return refNamePattern.MatchString(value) }

func validThread(thread Thread) bool {
	return thread.ProjectID > 0 && thread.ProjectID <= 2147483647 && ValidUUID(thread.ConversationUUID)
}

func validCaller(caller Caller) bool {
	switch caller.Writer {
	case WriterDesktop:
		return caller.UserID > 0 && caller.UserID <= 2147483647
	case WriterCloudExecutor:
		return caller.ExecutionID != "" && len(caller.ExecutionID) <= 64
	}
	return false
}

// ObjectKey is a segment's key inside the workspace bucket (decision 3):
// t/{conversationUUID}/seg/{sha256}.seg.
func ObjectKey(conversationUUID, sha256 string) string {
	return "t/" + conversationUUID + "/seg/" + sha256 + ".seg"
}

// ThreadPrefix is the key prefix of all a thread's objects.
func ThreadPrefix(conversationUUID string) string {
	return "t/" + conversationUUID + "/"
}

// AllowedRefs is the ref allowlist of a writer in a state (decision 2): the
// desktop writes base, upstream (only when HEAD has an upstream) and the
// handoff ref, in HANDING_OFF; the cloud executor writes only the cloud ref,
// in CLOUD_RUNNING. Nothing is writable in any other state.
func AllowedRefs(writer Writer, state State, hasUpstream bool) []string {
	switch {
	case writer == WriterDesktop && state == StateHandingOff:
		if hasUpstream {
			return []string{RefBase, RefUpstream, RefHandoff1}
		}
		return []string{RefBase, RefHandoff1}
	case writer == WriterCloudExecutor && state == StateCloudRunning:
		return []string{RefCloud}
	}
	return nil
}

// WriterState is the state a writer must find the thread in.
func WriterState(writer Writer) State {
	if writer == WriterCloudExecutor {
		return StateCloudRunning
	}
	return StateHandingOff
}
