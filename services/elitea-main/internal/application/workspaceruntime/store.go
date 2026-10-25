// Package workspaceruntime is the cloud executor's half of the workspace
// store (ADR-0034 decisions 9, 10 and 14): what the worker's gRPC calls and
// the runtime-context `workspace` field need from Main, as small interfaces.
//
// The transport (internal/transport/runtimegrpc/workspace) authenticates the
// workload and verifies the fence before it builds a ClaimRef, so nothing in
// a ClaimRef is the worker's unverified word except ThreadUUID and Epoch,
// which state what the worker believes and which Store refuses when stale.
//
// The handoff service (workspacehandoff / workspacestore) implements Store,
// TurnResolver and PolicyEvaluator. The composition root passes nil until it
// does, and every caller answers "feature unavailable" for nil.
package workspaceruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	// RefCloud is the only ref the executor writes.
	RefCloud = "refs/elitea/cloud"
	// RefUpstream is the desktop's orphan upstream commit U (decision 6).
	RefUpstream = "refs/elitea/upstream"
	// KindCompact is a full segment with no prerequisites.
	KindCompact = "COMPACT"
	// KindPush is an incremental segment whose prerequisites are commits the
	// store already holds (Phase 2).
	KindPush = "PUSH"
	// MaxPrerequisites bounds the prerequisites of one PUSH.
	MaxPrerequisites = 8
	// MaxPlanSegments bounds a fetch plan's chain: one COMPACT and at most
	// workspacestore.DefaultMaxPushChain PUSH segments, so the plan frame
	// stays under the control listener's 80 KiB response bound.
	MaxPlanSegments = 1 + 32
	// ObjectFormatSHA1 is the only object format the store moves.
	ObjectFormatSHA1 = "sha1"
	// ExecutedByCloud stamps a cloud turn's response meta (decision 8).
	ExecutedByCloud = "cloud_executor"
	// MaxSegmentBytes is the absolute segment bound the transport accepts;
	// the effective policy's max_segment_mb is lower or equal.
	MaxSegmentBytes = 2 << 30
)

var (
	// ErrClaimUnauthorized: the claim names no live running execution of
	// this workload. Refused, never retried.
	ErrClaimUnauthorized = errors.New("workspace claim is not authorized")
	// ErrUnavailable: a dependency (database, object store) did not answer;
	// the caller may retry.
	ErrUnavailable = errors.New("workspace store is temporarily unavailable")
	// ErrInvalidRequest: a request outside the contract's bounds.
	ErrInvalidRequest = errors.New("invalid workspace store request")
	// ErrThreadBusy: another change to the thread is being saved right now
	// (workspacestore.ErrThreadBusy). Nothing was compared, so it is NOT a
	// ref conflict: the transport answers a retryable status (ABORTED) and
	// the worker resends the SAME publish after a backoff.
	ErrThreadBusy = errors.New("workspace thread is busy")
)

// RefusalCode is a typed business refusal, mapped one to one onto
// runtimev1.WorkspaceRefusalCodeV1.
type RefusalCode string

const (
	RefusalNotWorkspaceTurn        RefusalCode = "not_a_workspace_turn"
	RefusalStaleEpoch              RefusalCode = "stale_epoch"
	RefusalRefConflict             RefusalCode = "ref_conflict"
	RefusalRefNotAllowed           RefusalCode = "ref_not_allowed"
	RefusalSegmentMissing          RefusalCode = "segment_missing"
	RefusalChecksumMismatch        RefusalCode = "checksum_mismatch"
	RefusalUnsupportedObjectFormat RefusalCode = "unsupported_object_format"
	RefusalSegmentTooLarge         RefusalCode = "segment_too_large"
	RefusalQuotaExceeded           RefusalCode = "workspace_quota_exceeded"
)

// Refusal is the error a Store returns for a business refusal.
type Refusal struct {
	Code RefusalCode
	// Current: the refs as they are now (RefusalRefConflict only).
	Current []Ref
}

func (r *Refusal) Error() string { return fmt.Sprintf("workspace store refused: %s", r.Code) }

// Refuse builds a *Refusal error.
func Refuse(code RefusalCode) error { return &Refusal{Code: code} }

// AsRefusal reports the refusal inside err, if any.
func AsRefusal(err error) (*Refusal, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) && refusal != nil {
		return refusal, true
	}
	return nil, false
}

// ClaimRef names the worker's live claim as the transport verified it, plus
// what the worker believes about the thread.
type ClaimRef struct {
	ExecutionID      string
	Generation       uint64
	ClaimID          string
	FenceToken       []byte
	WorkloadIdentity string
	// ThreadUUID and Epoch come from the runtime context the worker redeemed;
	// a Store compares them with the thread the claim resolves to.
	ThreadUUID string
	Epoch      uint64
}

// Ref is one store ref.
type Ref struct {
	Name string
	SHA  string
}

// RefUpdate is one compare-and-set; a nil Old means "must not exist".
type RefUpdate struct {
	Name string
	Old  *string
	New  string
}

// Segment is one stored segment of a fetch plan.
type Segment struct {
	Seq           uint64
	Kind          string
	SHA256        string
	Bytes         uint64
	Prerequisites []string
	Refs          []Ref
}

// FetchPlan is what a worker hydrates from: the minimal segment chain.
type FetchPlan struct {
	HeadSeq  uint64
	Refs     []Ref
	Segments []Segment
}

// PublishRequest is the executor's publish: one COMPACT segment, one move
// of refs/elitea/cloud with its expected old value.
type PublishRequest struct {
	Kind          string
	SegmentSHA256 string
	Bytes         uint64
	Prerequisites []string
	Refs          []RefUpdate
	ObjectFormat  string
}

// PublishResult is the applied publish (or the original result of an
// identical replay).
type PublishResult struct {
	Seq  uint64
	Refs []Ref
}

// UploadResult reports an upload.
type UploadResult struct {
	// Exists: the digest was already stored for this thread.
	Exists bool
}

// Store is what the executor's four gRPC calls need. The server unit
// implements it over workspacestore/workspacehandoff. Every method first
// resolves claim to its thread (the claim's execution is a cloud turn of a
// handed-off thread in CLOUD_RUNNING) and refuses RefusalNotWorkspaceTurn
// otherwise, and RefusalStaleEpoch when claim.Epoch or claim.ThreadUUID is
// not the thread's.
type Store interface {
	// FetchPlan returns the plan for the claim's thread (decision 2).
	FetchPlan(ctx context.Context, claim ClaimRef, sinceSeq uint64) (FetchPlan, error)
	// OpenSegment opens one segment of the claim's thread by digest. The
	// caller closes it.
	OpenSegment(ctx context.Context, claim ClaimRef, sha256 string) (io.ReadCloser, error)
	// PutSegment stores one segment. It reads body to EOF and stores the
	// object only when the bytes match size and sha256
	// (RefusalChecksumMismatch otherwise).
	PutSegment(ctx context.Context, claim ClaimRef, sha256 string, size uint64, body io.Reader) (UploadResult, error)
	// Publish runs the publish transaction for refs/elitea/cloud only
	// (RefusalRefNotAllowed for any other ref).
	Publish(ctx context.Context, claim ClaimRef, request PublishRequest) (PublishResult, error)
	// EnvSecret reads the thread's harvested env (ADR-0034 decision 5
	// amendment, contract 6.4): the one per-thread vault secret the begin
	// stored. An empty map when the thread has none. The values are never
	// logged, audited or cached.
	EnvSecret(ctx context.Context, claim ClaimRef) (map[string]string, error)
	// RecordEvent writes an audit event the worker observed in the running
	// turn (decision 12): EventScanBlocked or EventPullRequestOpened, with
	// the allowlisted attributes only (never a path).
	RecordEvent(ctx context.Context, claim ClaimRef, event ExecutorEvent) error
}

// The executor audit events (workspacehandoff.EventScanBlocked and
// EventPullRequestOpened).
const (
	EventScanBlocked       = "handoff.scan_blocked"
	EventPullRequestOpened = "pull_request.opened"
)

// ExecutorEvent is one executor audit event: its kind and its attributes
// (scan_blocked: findings, rules; pull_request.opened: toolkit, branch,
// base, number, url, files).
type ExecutorEvent struct {
	Kind  string
	Attrs map[string]string
}

const (
	// MaxEnvVars and MaxEnvBytes are the contract's env_limits.
	MaxEnvVars  = 200
	MaxEnvBytes = 64 << 10
)

// ValidEnv checks a harvested env against the contract's limits and a
// POSIX-portable name shape.
func ValidEnv(env map[string]string) bool {
	if len(env) > MaxEnvVars {
		return false
	}
	total := 0
	for name, value := range env {
		total += len(name) + len(value)
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return total <= MaxEnvBytes
}

var (
	hexSHA1   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	uuidText  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ValidSHA1 reports a 40-character lowercase hex object id.
func ValidSHA1(value string) bool { return hexSHA1.MatchString(value) }

// ValidSHA256 reports a 64-character lowercase hex content address.
func ValidSHA256(value string) bool { return hexSHA256.MatchString(value) }

// ValidThreadUUID reports a canonical lowercase uuid.
func ValidThreadUUID(value string) bool { return uuidText.MatchString(value) }

// ValidPrerequisites reports a segment's prerequisites as its kind needs
// them: none for a COMPACT, 1..MaxPrerequisites distinct sha1 commits for a
// PUSH; false for any other kind.
func ValidPrerequisites(kind string, prerequisites []string) bool {
	switch kind {
	case KindCompact:
		return len(prerequisites) == 0
	case KindPush:
		if len(prerequisites) == 0 || len(prerequisites) > MaxPrerequisites {
			return false
		}
		seen := map[string]bool{}
		for _, prerequisite := range prerequisites {
			if !ValidSHA1(prerequisite) || seen[prerequisite] {
				return false
			}
			seen[prerequisite] = true
		}
		return true
	}
	return false
}

// ValidateExecutorPublish checks the shape the executor may publish before
// any store is asked: a COMPACT (no prerequisites) or a PUSH (1..8 sha1
// prerequisites), sha1, exactly one move of refs/elitea/cloud to a valid
// commit, with a valid expected old value.
func ValidateExecutorPublish(request PublishRequest) error {
	if !ValidPrerequisites(request.Kind, request.Prerequisites) || !ValidSHA256(request.SegmentSHA256) ||
		request.Bytes == 0 || request.Bytes > MaxSegmentBytes {
		return ErrInvalidRequest
	}
	if request.ObjectFormat != ObjectFormatSHA1 {
		return Refuse(RefusalUnsupportedObjectFormat)
	}
	if len(request.Refs) != 1 {
		return ErrInvalidRequest
	}
	update := request.Refs[0]
	if update.Name != RefCloud {
		return Refuse(RefusalRefNotAllowed)
	}
	if !ValidSHA1(update.New) || (update.Old != nil && !ValidSHA1(*update.Old)) {
		return ErrInvalidRequest
	}
	return nil
}
