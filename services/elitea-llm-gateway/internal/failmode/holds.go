package failmode

import (
	"context"
	"sync"
)

// Holds is the set of degraded-mode reservations ONE request holds (human
// decision, DECISIONS.md 2026-10-22). The request path creates it with the
// request context; GovernanceStore.CheckBudget adds a Hold to it when it
// admits a capped degraded request; the billing path takes the holds and
// releases them after the real cost is on the degraded counter.
//
// The order matters: a hold is released only AFTER the request's cost is
// billed (or when the request ends with nothing to bill). Releasing first
// would leave a window in which neither the hold nor the cost is counted.
//
// The zero value is not usable; call NewHolds. All methods are safe on a nil
// receiver and for concurrent use.
type Holds struct {
	mu   sync.Mutex
	list []*Hold
	// adopted: a stream settler owns the release. The context-done backstop
	// then does nothing, because a stream's context is cancelled BEFORE the
	// settler bills (stream_drain.go).
	adopted bool
	// closed: the holds were released or taken; a late Add releases at once.
	closed bool
}

// NewHolds returns an empty set.
func NewHolds() *Holds { return &Holds{} }

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

// HeldFor returns the amount this request holds against key.
func (s *Holds) HeldFor(key string) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, h := range s.list {
		if h.key == key {
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

// Take hands the holds to the caller, who must release them after the bill
// lands, and closes the set.
func (s *Holds) Take() []*Hold {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list
	s.list = nil
	s.closed = true
	return out
}

// Adopt marks the set as owned by a stream settler: ReleaseUnlessAdopted then
// does nothing, and the settler calls ReleaseAll when it settles.
func (s *Holds) Adopt() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.adopted = true
	s.mu.Unlock()
}

// ReleaseAll releases every hold still in the set and closes it.
func (s *Holds) ReleaseAll() {
	ReleaseHolds(s.Take())
}

// ReleaseUnlessAdopted is the context-done backstop: it releases the holds of
// a request that ended without billing, unless a stream settler adopted them.
func (s *Holds) ReleaseUnlessAdopted() {
	if s == nil {
		return
	}
	s.mu.Lock()
	adopted := s.adopted
	s.mu.Unlock()
	if !adopted {
		s.ReleaseAll()
	}
}

// ReleaseHolds releases each hold in hs.
func ReleaseHolds(hs []*Hold) {
	for _, h := range hs {
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
