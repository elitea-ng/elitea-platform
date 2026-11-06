// Package browsersession is the cloud browser session record and the view
// grant digest (ADR-0049 decision 6, #1597).
//
// A session is one sandbox container on the supervisor's `browser` profile.
// The worker starts it at the run's first browser call and reports it to
// Main under its live claim (RuntimeControlService.ReportBrowserSession);
// Main keeps the row in elitea_runtime.browser_sessions. A viewer opens
// GET /api/v2/browser_sessions/{session_id}/view on any elitea-main replica,
// which reads the row and relays the WebSocket to the supervisor's
// BrowserView gRPC stream.
//
// Every function here is pure. The digests match the supervisor's
// (services/elitea-worker-rust/src/sandbox/browser_session.rs) byte for
// byte; browsersession_test.go pins them against fixed vectors.
package browsersession

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
)

// State is where a session is in its life. It moves forward only.
type State string

const (
	// StateLive: the session runs; a viewer may watch and, with control,
	// drive.
	StateLive State = "live"
	// StateLingering: the run ended; a viewer may watch until the linger
	// ends, and input is refused.
	StateLingering State = "lingering"
	// StateEnded: the session is gone.
	StateEnded State = "ended"
)

// Rank orders the states: a report never moves a row backwards.
func (s State) Rank() int {
	switch s {
	case StateLive:
		return 1
	case StateLingering:
		return 2
	case StateEnded:
		return 3
	default:
		return 0
	}
}

// Spec is BrowserSessionSpecV1 as the worker started the session.
type Spec struct {
	Revision    uint32
	Name        string
	Engine      int32
	CPUMillis   uint64
	MemoryBytes uint64
	WallSeconds uint64
	MaxPages    uint32
}

// Session is one row of elitea_runtime.browser_sessions.
type Session struct {
	SessionID   string
	ExecutionID string
	Generation  uint64
	TenantID    string
	ProjectID   int64
	// OwnerActorID is the run's owner (execution_jobs.actor_id).
	OwnerActorID string
	// ConversationUUID is the conversation the run answers; empty for none.
	ConversationUUID string
	// SupervisorAudience names the supervisor's browser profile: one of
	// Main's configured browser supervisors. Never an address.
	SupervisorAudience string
	Spec               Spec
	State              State
	LingerUntil        time.Time
	// StartedAt is when Main first stored the row (started_at).
	StartedAt time.Time
}

// Linger is the supervisor's linger after a run ends
// (browser_session_lifetime.rs LINGER): a session is never watchable past
// its start, its wall and this.
const Linger = 10 * time.Minute

// Deadline is the latest a session can exist: its start, plus its wall,
// plus the linger. The supervisor retires every session by then, whether
// or not the worker reported its end (a worker that crashed, lost its
// lease or could not reach Main reports nothing). The zero time is a row
// with no start, which has no bound.
func (s Session) Deadline() time.Time {
	if s.StartedAt.IsZero() {
		return time.Time{}
	}
	wall := s.Spec.WallSeconds
	if wall > 1<<32 {
		wall = 1 << 32
	}
	return s.StartedAt.Add(time.Duration(wall)*time.Second + Linger) //nolint:gosec // bounded above
}

// Watchable reports whether a viewer may attach at now: a live session, or
// a lingering one before its linger ends, and either only before its
// Deadline. A lingering row always carries its linger end (the control
// server refuses one without).
func (s Session) Watchable(now time.Time) bool {
	if deadline := s.Deadline(); !deadline.IsZero() && !now.Before(deadline) {
		return false
	}
	switch s.State {
	case StateLive:
		return true
	case StateLingering:
		return !s.LingerUntil.IsZero() && now.Before(s.LingerUntil)
	default:
		return false
	}
}

var nameShape = regexp.MustCompile(`^elitea-br-[0-9a-f]{24}$`)

// ValidName reports whether name has the session name shape.
func ValidName(name string) bool { return nameShape.MatchString(name) }

// NameFor is the session name the worker derives for an execution
// (browser_run/session.rs session_name): `elitea-br-` and the first 24 hex
// of SHA-256 over "elitea.browser-session\x00" and the execution id. Main
// accepts a report only for the reporting run's own name.
func NameFor(executionID string) string {
	sum := sha256.Sum256([]byte("elitea.browser-session\x00" + executionID))
	return "elitea-br-" + hex.EncodeToString(sum[:])[:24]
}

const (
	specDigestDomain = "elitea.sandbox.browser-session.v1\x00"
	viewDigestDomain = "elitea.sandbox.browser-view.v1\x00"
)

// SpecDigest is the supervisor's spec_digest: the domain, then revision,
// the name length-prefixed, engine, cpu_millis, memory_bytes,
// wall_seconds and max_pages, each fixed-width big-endian.
func SpecDigest(spec Spec) [32]byte {
	h := sha256.New()
	h.Write([]byte(specDigestDomain))
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], spec.Revision)
	h.Write(b[:4])
	binary.BigEndian.PutUint64(b[:], uint64(len(spec.Name)))
	h.Write(b[:])
	h.Write([]byte(spec.Name))
	binary.BigEndian.PutUint32(b[:4], uint32(spec.Engine)) //nolint:gosec // the wire's i32, as the supervisor hashes it
	h.Write(b[:4])
	for _, v := range []uint64{spec.CPUMillis, spec.MemoryBytes, spec.WallSeconds} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	binary.BigEndian.PutUint32(b[:4], spec.MaxPages)
	h.Write(b[:4])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ViewDigest is the request digest of a view stream's grant: the domain,
// the 32-byte spec digest, then one byte, 1 when the stream may drive. A
// watch-only grant cannot open a driving stream.
func ViewDigest(spec Spec, control bool) [32]byte {
	d := SpecDigest(spec)
	h := sha256.New()
	h.Write([]byte(viewDigestDomain))
	h.Write(d[:])
	if control {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// The refusals of a view stream, as the supervisor's client reports them
// (whatever the transport's own codes): the relay answers each with its
// own HTTP status before the WebSocket opens.
var (
	// ErrViewSessionGone: the supervisor holds no such session, or it ended
	// (410). The row is then marked ended.
	ErrViewSessionGone = errors.New("browser session is gone")
	// ErrViewUnavailable: the supervisor did not answer, or not in time (502).
	ErrViewUnavailable = errors.New("browser supervisor unavailable")
	// ErrViewRefused: the supervisor refused the grant or the spec (502, and
	// logged: Main signed something the supervisor does not accept).
	ErrViewRefused = errors.New("browser view refused by the supervisor")
	// ErrViewBusy: the supervisor is at a limit (429).
	ErrViewBusy = errors.New("browser supervisor is at its limit")
)

// ErrInvalidSpec is a spec Main does not store.
var ErrInvalidSpec = errors.New("browser session spec is invalid")

// ValidateSpec checks the shape the supervisor admits. The supervisor still
// applies its own ceilings.
func ValidateSpec(spec Spec) error {
	if spec.Revision != 1 || !ValidName(spec.Name) || (spec.Engine != 1 && spec.Engine != 2) ||
		spec.CPUMillis == 0 || spec.MemoryBytes == 0 || spec.WallSeconds == 0 || spec.MaxPages == 0 ||
		spec.CPUMillis > 1<<40 || spec.MemoryBytes > 1<<50 || spec.WallSeconds > 1<<32 || spec.MaxPages > 1<<16 {
		return ErrInvalidSpec
	}
	return nil
}
