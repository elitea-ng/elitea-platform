package account

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

// EgressNotAllowedReason is the rejection reason emitted when a provider
// credential's api_base names a host the operator's egress allowlist does not
// cover (issue #13).
const EgressNotAllowedReason = "EGRESS_HOST_NOT_ALLOWED"

// ErrEgressNotAllowed is returned when a credential is rejected by the egress
// allowlist. It carries EgressNotAllowedReason so callers can map it to an HTTP
// 400 invalid_request_error (spec §2.5) without disclosing the host.
var ErrEgressNotAllowed = errors.New(EgressNotAllowedReason)

// EgressSource supplies the allowlist entries authored at RUNTIME.
//
// It is the gateway's governance-definition plane (internal/policy) seen from
// here: the compiled snapshot's `egress_allowlist` rows. The interface is
// declared at the consumer, and it carries no context, because the snapshot is
// already in memory and this is called on the credential path.
//
// nil leaves the gate on the environment floor alone — the posture of a gateway
// booted without a database.
type EgressSource interface {
	// EgressAllowlist returns the authored entries. It must be cheap: it is
	// called once per credential resolution.
	EgressAllowlist() []string
}

// egressGate is the merged egress decision: the operator's environment floor
// UNION the authored rows.
//
// # Why the environment variable stays a floor
//
// GATEWAY_EGRESS_ALLOWLIST is set in the chart by whoever owns the deployment.
// The authored rows are written by whoever holds `configuration.governance`.
// Those are not the same authority, so the runtime plane may only ADD a
// destination. If an authored row could withdraw one, a mistaken or hostile
// admin session could cut the platform off from its own provider, and the fix
// would need a database edit rather than a chart rollback.
//
// # Why the merge is cached
//
// The snapshot changes on a 30 s poll and the credential path runs on every
// request. Re-parsing tens of entries per credential would be waste, so the
// merged list is rebuilt only when the authored set actually changes. The
// fingerprint is the joined entry text: the authored set is small and
// operator-written, so an exact comparison is cheaper than any scheme that
// tries to be clever about it.
type egressGate struct {
	// env is the parsed floor. It is validated at construction, so a typo in
	// the chart fails startup rather than silently dropping a rule.
	env *egresslib.Allowlist

	// source is the runtime plane, held atomically. An empty pointer means
	// "floor only".
	//
	// It is atomic because SetSource runs at startup while GetKeysForProvider
	// reads it on every credential resolution. Those are different goroutines
	// even when the wiring order guarantees they do not overlap in practice,
	// and "in practice" is not a memory model.
	source atomic.Pointer[EgressSource]

	// state holds the last merge. Reads are a single atomic load.
	state atomic.Pointer[egressState]
	// rebuild serialises the rebuild so a burst of requests after a refresh
	// parses once rather than once each.
	rebuild sync.Mutex

	logger *slog.Logger
}

// egressState is one immutable merge result.
type egressState struct {
	// key fingerprints the authored entries this state was built from.
	key string
	// merged is env UNION authored.
	merged *egresslib.Allowlist
	// db is the authored entry text that survived parsing, for the report.
	db []string
	// dropped is the authored entry text that did NOT parse. It is reported
	// rather than swallowed: an entry that vanishes silently is the failure
	// this whole feature exists to end.
	dropped []string
}

// newEgressGate parses the environment floor and binds the runtime source.
func newEgressGate(envEntries []string, source EgressSource, logger *slog.Logger) (*egressGate, error) {
	env, err := egresslib.Parse(envEntries)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	g := &egressGate{env: env, logger: logger}
	g.state.Store(&egressState{key: "", merged: env})
	if source != nil {
		g.source.Store(&source)
	}
	return g, nil
}

// SetSource binds the runtime plane after construction.
//
// The account is built before the policy store, because the policy store is
// optional and the account is not. Rather than reorder two independent
// lifecycles, the gate accepts its source once the store exists. It is called
// from main() during startup, before the listener opens.
func (g *egressGate) SetSource(source EgressSource) {
	if g == nil {
		return
	}
	g.rebuild.Lock()
	defer g.rebuild.Unlock()
	if source == nil {
		g.source.Store(nil)
	} else {
		g.source.Store(&source)
	}
	// Drop the cached merge: its key was computed against the previous source.
	g.state.Store(&egressState{key: "", merged: g.env})
}

// current returns the live merged state, rebuilding it when the authored set
// has changed.
func (g *egressGate) current() *egressState {
	if g == nil {
		return &egressState{merged: nil}
	}
	var authored []string
	if source := g.source.Load(); source != nil && *source != nil {
		authored = (*source).EgressAllowlist()
	}
	key := strings.Join(authored, "\x00")

	if st := g.state.Load(); st != nil && st.key == key {
		return st
	}

	g.rebuild.Lock()
	defer g.rebuild.Unlock()
	// Another goroutine may have rebuilt while this one waited.
	if st := g.state.Load(); st != nil && st.key == key {
		return st
	}

	merged, dropped := g.env.Merge(authored)
	st := &egressState{key: key, merged: merged, db: authored, dropped: dropped}
	if len(dropped) > 0 {
		// elitea-main validates on write and internal/policy rejects a bad row
		// by name, so this should be unreachable. "Should be" is not a
		// guarantee across a database restore, so say it out loud instead of
		// letting the entry disappear.
		g.logger.Error("account: authored egress allowlist entries could not be parsed and are NOT in force",
			"count", len(dropped), "entries", strings.Join(dropped, ","))
	}
	g.state.Store(st)
	return st
}

// allows reports whether apiBase may be dialled under the merged allowlist.
func (g *egressGate) allows(apiBase string) bool {
	if g == nil {
		return false
	}
	return g.current().merged.Allows(apiBase)
}

// configured reports whether ANY source supplied an entry. When false the
// allowlist imposes no host restriction, and the private network stays closed.
func (g *egressGate) configured() bool {
	if g == nil {
		return false
	}
	return g.current().merged.Configured()
}

// allowsPrivateNetwork reports whether an entry EXPLICITLY names a private
// destination. See egresslib: this is not "is anything configured".
func (g *egressGate) allowsPrivateNetwork() bool {
	if g == nil {
		return false
	}
	return g.current().merged.AllowsPrivateNetwork()
}

// EgressReport is the operator-facing view of the merged allowlist, source by
// source. It backs GET /governance/status.
//
// The two sources are reported apart on purpose. "The host I added is not
// working" has two very different answers depending on whether the entry
// reached the gateway at all, and a single merged list cannot tell them apart.
type EgressReport struct {
	// Configured is false when neither source supplied an entry. Every
	// tenant-authored api_base host is then permitted, and PrivateNetwork is
	// false, so the SSRF-safe dialer refuses private destinations for everyone.
	Configured bool `json:"configured"`
	// Env is the GATEWAY_EGRESS_ALLOWLIST floor, normalised.
	Env []string `json:"env"`
	// DB is the authored `egress_allowlist` rows, as the gateway last read them.
	DB []string `json:"db"`
	// Effective is the union actually enforced.
	Effective []string `json:"effective"`
	// Dropped names authored entries the gateway could not parse. Non-empty
	// means an authored destination is NOT in force.
	Dropped []string `json:"dropped"`
	// PrivateNetwork reports whether bifrost's SSRF-safe dialer is relaxed for
	// the self-hosted provider classes, because an entry names a private
	// destination.
	PrivateNetwork bool `json:"private_network"`
}

// EgressReport renders the merged allowlist for the status surface.
func (a *EliteaAccount) EgressReport() EgressReport {
	if a == nil || a.egress == nil {
		return EgressReport{Env: []string{}, DB: []string{}, Effective: []string{}, Dropped: []string{}}
	}
	st := a.egress.current()
	rep := EgressReport{
		Configured:     st.merged.Configured(),
		Env:            a.egress.env.Entries(),
		DB:             append([]string{}, st.db...),
		Effective:      st.merged.Entries(),
		Dropped:        append([]string{}, st.dropped...),
		PrivateNetwork: st.merged.AllowsPrivateNetwork(),
	}
	if rep.DB == nil {
		rep.DB = []string{}
	}
	if rep.Dropped == nil {
		rep.Dropped = []string{}
	}
	return rep
}
