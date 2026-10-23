package failmode

import (
	"sync"
)

// DegradedCounters tracks, per budget scope, the nano-USD this replica has
// spent while NATS is down (design §8.5). It bounds per-replica overspend
// during the breaker-open window and is reset to zero — per scope — on the
// breaker→CLOSED transition, after the NATS replay is confirmed, so the cap
// keeps gating until the authoritative counter is current. It otherwise resets
// only on pod restart.
//
// A scope's spend has two parts:
//
//   - billed: the cost of requests that finished and were billed while NATS
//     was down (Add).
//   - held: the RESERVATIONS of degraded requests that are admitted and not
//     yet billed (TryReserve, Hold.Release). Admission does not know a
//     request's cost, so without a reservation every request admitted before
//     the first one is billed would see the same spend and pass (human
//     decision, DECISIONS.md 2026-10-22).
//
// Get reports billed + held, the amount the FSM compares with the share.
//
// It is safe for concurrent use: the request path calls Add/Get/TryReserve;
// the recovery goroutine calls Reset/ResetAll. One mutex guards the map and
// every entry. The degraded path is the only caller of TryReserve, and it runs
// only while NATS is down, so contention is not a concern.
type DegradedCounters struct {
	mu   sync.Mutex
	vals map[string]*degradedEntry
}

// degradedEntry is one scope's degraded spend. gen changes on every Reset, so
// a Hold taken before a reset cannot subtract from the new window.
type degradedEntry struct {
	billed int64
	held   int64
	gen    uint64
}

// NewDegradedCounters constructs an empty counter set.
func NewDegradedCounters() *DegradedCounters {
	return &DegradedCounters{vals: make(map[string]*degradedEntry)}
}

// entryLocked returns the entry for key, creating it on first use. The caller
// holds d.mu.
func (d *DegradedCounters) entryLocked(key string) *degradedEntry {
	e := d.vals[key]
	if e == nil {
		e = &degradedEntry{}
		d.vals[key] = e
	}
	return e
}

// Add records deltaNano billed against key while degraded and returns the new
// replica-local BILLED total. deltaNano is nano-USD (may be negative for a
// correction).
func (d *DegradedCounters) Add(key string, deltaNano int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entryLocked(key)
	e.billed += deltaNano
	return e.billed
}

// Get returns this replica's degraded-window spend for key: billed plus held
// (0 if never used).
func (d *DegradedCounters) Get(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.vals[key]
	if e == nil {
		return 0
	}
	return e.billed + e.held
}

// Held returns the reserved, not yet billed amount for key.
func (d *DegradedCounters) Held(key string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.vals[key]; e != nil {
		return e.held
	}
	return 0
}

// TryReserve reserves nano against key when billed + held + nano <= share, and
// returns the Hold that releases it. The test and the add are one atomic step,
// so concurrent admissions cannot all pass against the same spend. A share or
// a reservation of 0 or less reserves nothing and returns ok=false.
func (d *DegradedCounters) TryReserve(key string, nano, share int64) (*Hold, bool) {
	if nano <= 0 || share <= 0 {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entryLocked(key)
	if e.billed+e.held+nano > share {
		return nil, false
	}
	e.held += nano
	return &Hold{d: d, key: key, nano: nano, gen: e.gen}, true
}

// Reset zeroes the degraded counter for key, both billed and held. Called on
// the breaker→CLOSED transition after the NATS replay for that scope is
// confirmed (§8.5). Resetting before the replay would let the now-healthy
// counter under-count; resetting per-request would defeat the cap.
//
// Clearing held as well is deliberate: a request still in flight bills onto
// the healthy NATS counter, and a Hold that leaked (a path that never released
// it) must not shrink the next outage's share. Holds taken before the reset
// release nothing afterwards (gen).
func (d *DegradedCounters) Reset(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.vals[key]; e != nil {
		e.billed, e.held = 0, 0
		e.gen++
	}
}

// ResetAll zeroes every scope counter. Used when the breaker closes and the
// recovery pass has reconciled all outstanding outage rows for this replica.
func (d *DegradedCounters) ResetAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.vals {
		e.billed, e.held = 0, 0
		e.gen++
	}
}

// Hold is one admitted degraded request's reservation. Release returns it.
// Release is idempotent and safe on a nil Hold.
type Hold struct {
	d    *DegradedCounters
	key  string
	nano int64
	gen  uint64
	once sync.Once
}

// Key is the budget scope key the hold is reserved against.
func (h *Hold) Key() string {
	if h == nil {
		return ""
	}
	return h.key
}

// Nano is the reserved amount in nano-USD.
func (h *Hold) Nano() int64 {
	if h == nil {
		return 0
	}
	return h.nano
}

// Release returns the reservation. A hold taken before a Reset of its scope
// releases nothing: the reset already cleared it.
func (h *Hold) Release() {
	if h == nil || h.d == nil {
		return
	}
	h.once.Do(func() {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		if e := h.d.vals[h.key]; e != nil && e.gen == h.gen {
			e.held -= h.nano
		}
	})
}
