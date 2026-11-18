package delegationgrant

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Drift is what a callee holds now that its grant does not cover (ADR-0044
// amendment 1, A1.2 rule 3). It is recorded on a stale grant so the caller's
// editors see what changed, and the worker narrows the same items out.
type Drift struct {
	// CalleeVersionID is the callee version the drifted set was read from.
	CalleeVersionID int64 `json:"callee_version_id"`
	// Digest is the digest of the callee's current set: the notification and
	// the re-approval key on it.
	Digest string `json:"digest"`
	// Added lists the items the grant does not cover.
	Added []Item `json:"added"`
}

// DriftAction is what a drift check does to a grant.
type DriftAction string

// The drift actions.
const (
	// DriftNone: the callee is within the grant (the same set or a smaller
	// one), or the grant is not one a drift check applies to.
	DriftNone DriftAction = "none"
	// DriftMarkStale: an approved grant whose callee now holds more.
	DriftMarkStale DriftAction = "mark_stale"
	// DriftUpdate: a stale grant whose callee holds a different wider set than
	// the drift on record.
	DriftUpdate DriftAction = "update"
	// DriftRestore: a stale grant whose callee is back within the grant.
	DriftRestore DriftAction = "restore"
)

// Verdict is the outcome of one drift check.
type Verdict struct {
	Action DriftAction
	Drift  Drift
}

// Assess compares the callee's current set with the grant. The check is set
// containment against what the callee held when the grant was proposed (the
// requested set), never version equality, and never against the approved
// set: an approver who left an item out of a partial approval has not made
// the callee drift by that omission.
//
//   - a current set the requested set covers (the same or smaller) needs
//     nothing, and brings a stale grant back to approved;
//   - a wider current set marks an approved grant stale and records what
//     changed; a stale grant whose drift differs is updated;
//   - only approved and stale grants are assessed: a pending grant is still
//     being decided and a superseded or revoked one is history.
func Assess(grant Grant, current GrantSet, calleeVersionID int64, recorded *Drift) Verdict {
	if grant.State != StateApproved && grant.State != StateStale {
		return Verdict{Action: DriftNone}
	}
	added := grant.RequestedSet.Missing(current)
	if len(added) == 0 {
		if grant.State == StateStale {
			return Verdict{Action: DriftRestore}
		}
		return Verdict{Action: DriftNone}
	}
	drift := Drift{CalleeVersionID: calleeVersionID, Digest: current.Digest(), Added: added}
	if grant.State == StateApproved {
		return Verdict{Action: DriftMarkStale, Drift: drift}
	}
	if recorded != nil && recorded.Digest == drift.Digest {
		return Verdict{Action: DriftNone, Drift: drift}
	}
	return Verdict{Action: DriftUpdate, Drift: drift}
}

// Closure is the callees an edge reaches through approved grants, each with
// the digest of the grant into it, keyed by application id.
type Closure map[int64]string

// Grew reports whether the closure holds a callee, or a grant digest, that
// before did not: the closure of an approved edge grew (A1.2 transitivity).
// The caller's editors are told, because the attach dialog showed the closure
// as information; nothing changes state, since each hop needs its own grant
// and passes the principal bound anyway. A first reading (before nil) is a
// baseline, not growth.
func (c Closure) Grew(before Closure) bool {
	if before == nil {
		return false
	}
	for id, digest := range c {
		if previous, had := before[id]; !had || previous != digest {
			return true
		}
	}
	return false
}

// Equal reports whether two readings are the same closure. A reading that was
// never taken (nil) equals nothing, so the first reading is always recorded.
func (c Closure) Equal(other Closure) bool {
	if c == nil || other == nil || len(c) != len(other) {
		return false
	}
	for id, digest := range c {
		if previous, ok := other[id]; !ok || previous != digest {
			return false
		}
	}
	return true
}

// Key is a fixed-size digest of a closure, used in the notification dedup key
// (a closure of many agents would not fit a btree index row as text).
func (c Closure) Key() string {
	ids := make([]int64, 0, len(c))
	for id := range c {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteByte('=')
		b.WriteString(c[id])
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// The notification event types of delegation drift. The message in the meta
// is what the bell shows.
const (
	NotifyStale       = "delegation_grant_stale"
	NotifyClosureGrew = "delegation_closure_grew"
	// NotifyLegacyPending is the one notification per calling agent that lists
	// the pending delegations the migration of existing compositions wrote
	// (story S6).
	NotifyLegacyPending = "delegation_legacy_pending"
)

// Tracked is an approved or stale grant with what the last drift check
// recorded on it: the drift of a stale grant and the closure of the edge.
type Tracked struct {
	Grant   Grant
	Drift   *Drift
	Closure Closure
}

// DriftApplied is what applying a drift verdict did to a grant.
type DriftApplied struct {
	// Grant is the grant after the change.
	Grant Grant
	// Changed: the grant's state, drift or closure moved.
	Changed bool
	// NotifiedStale and NotifiedClosure count the people told about the stale
	// grant and about the closure growth in this call (0 when the notice
	// already existed).
	NotifiedStale, NotifiedClosure int
}

// ActionDriftRecorded is the security audit action of a drift check that
// changed a grant (stale, updated or restored), next to the state action.
const ActionDriftRecorded = "agent.delegation.drift_recorded"
