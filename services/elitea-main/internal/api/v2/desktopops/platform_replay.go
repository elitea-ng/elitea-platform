package desktopops

// The replay store of platform MCP calls (issue 1231). A saved toolkit's run
// is idempotent on its admission row in PostgreSQL. A platform call runs in
// process and has no such row, so without a store shared by every replica a
// retry that lands on another replica after a dropped answer would run a write
// twice. The store is a JetStream KV bucket on the live-update NATS server,
// the same server and account canvas presence uses.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// PlatformReplayBucket is the JetStream KV bucket that holds every
	// platform call's claim and answer. The nats-bootstrap Job creates it
	// (History 1, TTL 15m, max value size PlatformReplayMaxValue).
	PlatformReplayBucket = "ELITEA_PLATFORM_MCP_REPLAY"
	// PlatformReplayTTL is how long a retry with the same Idempotency-Key is
	// answered from the first attempt: the bucket's MaxAge, counted from the
	// entry's last write (the answer).
	PlatformReplayTTL = 15 * time.Minute
	// PlatformReplayMaxValue is the bucket's max value size. An answer whose
	// JSON is longer than platformReplayMaxBody is stored as a marker
	// without its body (PlatformReplayResult.Omitted).
	PlatformReplayMaxValue = 256 << 10
	platformReplayMaxBody  = 192 << 10
)

// ErrPlatformReplayUnavailable wraps every store failure other than a lost
// claim race. The handler answers it 503 with Retry-After and runs nothing:
// without the store a retry could not be told from a first call.
var ErrPlatformReplayUnavailable = errors.New("the platform MCP replay store is unavailable")

// ReplayVerdict is what Claim found for a key.
type ReplayVerdict int

const (
	// ReplayClaimed: the key was free and is now this call's; run the tool,
	// then Complete (or Release, if it never reached the tool).
	ReplayClaimed ReplayVerdict = iota
	// ReplayInProgress: another attempt holds the key and has not answered.
	ReplayInProgress
	// ReplayDone: the key's call answered; replay Result.
	ReplayDone
	// ReplayConflict: the key belongs to a call with other input.
	ReplayConflict
)

// PlatformReplayResult is one call's stored answer.
type PlatformReplayResult struct {
	Status int
	// Body is the answer's JSON; nil when Omitted.
	Body json.RawMessage
	// Omitted: the answer was larger than the store keeps. The call ran
	// (its HTTP status and OK are kept) and must not run again.
	Omitted bool
	OK      bool
}

// PlatformReplay is Claim's answer.
type PlatformReplay struct {
	Verdict ReplayVerdict
	// Result is set for ReplayDone.
	Result PlatformReplayResult
	// Revision is the claim's revision for ReplayClaimed: Complete and
	// Release only change the entry this claim wrote.
	Revision uint64
	// Digest is the claimed call's input digest (ReplayClaimed).
	Digest string
}

// PlatformReplayStore claims a call's key before it runs and keeps its
// answer. Production composes *NATSPlatformReplays; nothing else may be
// composed there, because a per-replica store lets a retry on another replica
// run the call again.
type PlatformReplayStore interface {
	// Claim takes key for a call whose input digest is digest, atomically:
	// exactly one of any number of concurrent claims, on any replica, is
	// ReplayClaimed.
	Claim(ctx context.Context, key, digest string) (PlatformReplay, error)
	// Complete records the answer of a claimed key.
	Complete(ctx context.Context, key string, claim PlatformReplay, result PlatformReplayResult) error
	// Release frees a claimed key whose call never reached the tool, so its
	// retry runs.
	Release(ctx context.Context, key string, claim PlatformReplay) error
}

// platformReplayKey is the KV key of one call: SHA-256 of (caller, project,
// Idempotency-Key), so no raw identifier or client-chosen byte reaches a key
// (KV keys allow only [-/_=.a-zA-Z0-9]).
func platformReplayKey(actor, project int64, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(strconv.FormatInt(actor, 10) + "\x00" + strconv.FormatInt(project, 10) + "\x00" + idempotencyKey))
	return "call." + hex.EncodeToString(digest[:])
}

// NATSPlatformReplays is the cross-replica PlatformReplayStore.
//
// Claim is a KV Create (it fails when the key exists) of an "in_progress"
// record; Complete is a KV Update at the claim's revision to the "done"
// record with the answer; Release deletes at the claim's revision. The bucket
// MaxAge collects every entry PlatformReplayTTL after its last write, so a
// claim whose replica died before answering stays "in_progress" until then
// and is never run again with that key.
//
// Per-message TTL (NATS 2.11+ KeyTTL) is not used: nats.go offers it on
// Create only, so the answer's Update could not restart it, and the bucket
// MaxAge already does exactly that.
type NATSPlatformReplays struct {
	kv  jetstream.KeyValue
	now func() time.Time
}

// NewNATSPlatformReplays binds to bucket (empty means PlatformReplayBucket)
// and verifies the settings the replay window rests on. It does not create
// the bucket: the nats-bootstrap Job owns every JetStream asset (#1076), and
// elitea-main's identity may not create a stream.
func NewNATSPlatformReplays(ctx context.Context, js jetstream.JetStream, bucket string) (*NATSPlatformReplays, error) {
	if js == nil {
		return nil, errors.New("platform MCP replay store: a JetStream context is required")
	}
	if bucket == "" {
		bucket = PlatformReplayBucket
	}
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, fmt.Errorf("platform MCP replay store: KV bucket %s does not exist; it is created by the nats-bootstrap Job (deploy/helm/elitea natsBootstrap; in compose, the nats-bootstrap service) — run it before elitea-main", bucket)
		}
		return nil, fmt.Errorf("platform MCP replay store: bind KV bucket %s: %w", bucket, err)
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("platform MCP replay store: KV bucket %s: read status: %w", bucket, err)
	}
	if err := verifyPlatformReplayBucket(status); err != nil {
		return nil, fmt.Errorf("platform MCP replay store: KV bucket %s: %w; it is configured by the nats-bootstrap Job — re-run it", bucket, err)
	}
	return newNATSPlatformReplaysOver(kv), nil
}

func verifyPlatformReplayBucket(status jetstream.KeyValueStatus) error {
	if status.TTL() < PlatformReplayTTL {
		return fmt.Errorf("TTL %v is shorter than the %v replay window, so a retry inside it could run a call again", status.TTL(), PlatformReplayTTL)
	}
	if bucket, ok := status.(*jetstream.KeyValueBucketStatus); ok {
		if limit := bucket.StreamInfo().Config.MaxMsgSize; limit > 0 && limit < PlatformReplayMaxValue {
			return fmt.Errorf("max value size %d is below %d, so a stored answer would be refused", limit, PlatformReplayMaxValue)
		}
	}
	return nil
}

func newNATSPlatformReplaysOver(kv jetstream.KeyValue) *NATSPlatformReplays {
	return &NATSPlatformReplays{kv: kv, now: time.Now}
}

const (
	replayStateInProgress = "in_progress"
	replayStateDone       = "done"
)

// replayRecord is one entry's value. It holds the input digest (a hash of the
// toolkit type, tool name and the SHA-256 of the arguments, never the
// arguments) and, once done, the answer the caller was given.
type replayRecord struct {
	State  string `json:"state"`
	Digest string `json:"digest"`
	// At is when the entry was last written (claimed, then answered).
	At      int64           `json:"at_unix_milli"`
	Status  int             `json:"status,omitempty"`
	OK      bool            `json:"ok,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Omitted bool            `json:"result_omitted,omitempty"`
}

// claimAttempts bounds the Create/Get loop: a Get that finds no entry after a
// Create that found one means the entry was released or expired in between,
// and the key is free again.
const claimAttempts = 3

func (s *NATSPlatformReplays) Claim(ctx context.Context, key, digest string) (PlatformReplay, error) {
	if s == nil || s.kv == nil {
		return PlatformReplay{}, ErrPlatformReplayUnavailable
	}
	value, err := json.Marshal(replayRecord{State: replayStateInProgress, Digest: digest, At: s.now().UnixMilli()})
	if err != nil {
		return PlatformReplay{}, fmt.Errorf("%w: encode claim: %w", ErrPlatformReplayUnavailable, err)
	}
	for range claimAttempts {
		revision, err := s.kv.Create(ctx, key, value)
		if err == nil {
			return PlatformReplay{Verdict: ReplayClaimed, Revision: revision, Digest: digest}, nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return PlatformReplay{}, fmt.Errorf("%w: claim: %w", ErrPlatformReplayUnavailable, err)
		}
		entry, err := s.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			continue
		}
		if err != nil {
			return PlatformReplay{}, fmt.Errorf("%w: read claim: %w", ErrPlatformReplayUnavailable, err)
		}
		return replayOf(entry.Value(), digest)
	}
	return PlatformReplay{}, fmt.Errorf("%w: the key kept changing under the claim", ErrPlatformReplayUnavailable)
}

func replayOf(value []byte, digest string) (PlatformReplay, error) {
	var held replayRecord
	if err := json.Unmarshal(value, &held); err != nil {
		return PlatformReplay{}, fmt.Errorf("%w: unreadable entry: %w", ErrPlatformReplayUnavailable, err)
	}
	switch {
	case held.Digest != digest:
		return PlatformReplay{Verdict: ReplayConflict}, nil
	case held.State == replayStateDone:
		return PlatformReplay{Verdict: ReplayDone, Result: PlatformReplayResult{
			Status: held.Status, Body: held.Body, Omitted: held.Omitted, OK: held.OK,
		}}, nil
	default:
		return PlatformReplay{Verdict: ReplayInProgress}, nil
	}
}

func (s *NATSPlatformReplays) Complete(ctx context.Context, key string, claim PlatformReplay, result PlatformReplayResult) error {
	if s == nil || s.kv == nil {
		return ErrPlatformReplayUnavailable
	}
	held := replayRecord{
		State: replayStateDone, Digest: claim.Digest, At: s.now().UnixMilli(),
		Status: result.Status, OK: result.OK, Body: result.Body, Omitted: result.Omitted,
	}
	if len(held.Body) > platformReplayMaxBody {
		held.Body, held.Omitted = nil, true
	}
	value, err := json.Marshal(held)
	if err != nil {
		return fmt.Errorf("%w: encode answer: %w", ErrPlatformReplayUnavailable, err)
	}
	// At the claim's revision: an entry this claim did not write (it expired
	// and another call claimed the key) is never overwritten.
	if _, err := s.kv.Update(ctx, key, value, claim.Revision); err != nil {
		return fmt.Errorf("%w: record answer: %w", ErrPlatformReplayUnavailable, err)
	}
	return nil
}

func (s *NATSPlatformReplays) Release(ctx context.Context, key string, claim PlatformReplay) error {
	if s == nil || s.kv == nil {
		return ErrPlatformReplayUnavailable
	}
	if err := s.kv.Delete(ctx, key, jetstream.LastRevision(claim.Revision)); err != nil {
		return fmt.Errorf("%w: release: %w", ErrPlatformReplayUnavailable, err)
	}
	return nil
}
