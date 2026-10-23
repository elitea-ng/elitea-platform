package failmode

import (
	"context"
	"sync"
)

// Holds is the set of degraded-mode reservations ONE request holds (human
// decision, DECISIONS.md 2026-10-22). GovernanceStore.CheckBudget adds a Hold
// to it when it admits a capped degraded request.
//
// THE RULE: a hold is released only AFTER the request's cost is on the
// degraded counter, or when it is certain that nothing will be billed.
// Releasing first would leave a window in which neither the hold nor the cost
// is counted.
//
// The set enforces the rule with pins. It starts with one pin, the request's
// own, which the request scope drops (End) when the HTTP handler returns. Each
// biller pins the set before it hands a cost to a billing goroutine, and unpins
// after that goroutine put the cost on the counter. The holds are released
// when the LAST pin drops. So:
//
//   - a client disconnect or a context cancel releases nothing: the handler
//     is still running and holds the request's pin;
//   - a stream settler that bills after the handler returned pinned the set
//     while the handler still ran;
//   - a realtime session's last turn bills before the session's holds go.
//
// No code path "owns" or "takes" the holds, so no caller has to know which
// surface it serves.
//
// The zero value is not usable; call NewHolds. All methods are safe on a nil
// receiver and for concurrent use.
type Holds struct {
	mu   sync.Mutex
	list []*Hold
	// pins counts who still needs the holds: the request itself (until End)
	// and each biller in flight.
	pins int
	// closed: the last pin dropped and the holds were released. A late Add
	// releases at once and a late Pin is a no-op.
	closed  bool
	endOnce sync.Once
}

// NewHolds returns an empty set that carries the request's own pin.
func NewHolds() *Holds { return &Holds{pins: 1} }

// Add records h. When the set is already closed, h is released at once, so a
// hold can never outlive the request that took it.
func (s *Holds) Add(h *Hold) {
	if s == nil || h == nil {
		h.Release()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		h.Release()
		return
	}
	s.list = append(s.list, h)
	s.mu.Unlock()
}

// HeldFor returns the amount this request holds against key in the CURRENT
// degraded window. A hold that a Reset or ResetAll already cleared is not
// counted: the counter no longer includes it, so subtracting it would
// under-count the replica's spend.
func (s *Holds) HeldFor(key string) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, h := range s.list {
		if h.key == key && h.live() {
			n += h.nano
		}
	}
	return n
}

// Len returns how many holds the set carries.
func (s *Holds) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// Pin keeps the holds until the returned function runs. A biller calls it
// before it hands a cost to a billing goroutine, and runs the returned
// function after the cost is on the counter. The returned function is
// idempotent. On a nil or closed set Pin returns a no-op.
func (s *Holds) Pin() (unpin func()) {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return func() {}
	}
	s.pins++
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(s.unpin) }
}

// End drops the request's own pin. The request scope calls it when the HTTP
// handler returns. It is idempotent.
func (s *Holds) End() {
	if s == nil {
		return
	}
	s.endOnce.Do(s.unpin)
}

func (s *Holds) unpin() {
	s.mu.Lock()
	s.pins--
	if s.pins > 0 || s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	list := s.list
	s.list = nil
	s.mu.Unlock()
	for _, h := range list {
		h.Release()
	}
}

type holdsCtxKey struct{}

type checkOnlyCtxKey struct{}

// HoldsKey is the context key the request path stores its *Holds under. It is
// exported so a *schemas.BifrostContext can SetValue it.
var HoldsKey any = holdsCtxKey{}

// WithHolds returns ctx carrying s.
func WithHolds(ctx context.Context, s *Holds) context.Context {
	return context.WithValue(ctx, holdsCtxKey{}, s)
}

// HoldsFrom returns the request's holds, or nil.
func HoldsFrom(ctx context.Context) *Holds {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(holdsCtxKey{}).(*Holds)
	return s
}

// CheckOnly marks ctx as a re-check of work that is already admitted (the
// realtime ticker). CheckBudget then reserves nothing, and it does not count
// the request's OWN holds as other spend: a live session must not be refused
// because of the reservation it took at admission.
func CheckOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkOnlyCtxKey{}, true)
}

// IsCheckOnly reports whether ctx is a re-check.
func IsCheckOnly(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(checkOnlyCtxKey{}).(bool)
	return v
}
