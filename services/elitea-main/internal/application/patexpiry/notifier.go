// Package patexpiry produces the in-app notice a personal access token's
// owner gets shortly before the key stops working (issue #940 A3).
//
// ONE PASS, TWO CALLERS. The pass is a plain method rather than a scheduler
// handler because two callers run it: the platform scheduler on its cadence
// (runtimecomposition's patExpirySweep) and an operator, through
// `POST /api/v2/admin/background_jobs/administration/pat_expiry_notices:run`.
// Both must be the SAME code — an operator "run now" that takes a different
// path from the scheduled one tests nothing about the scheduled one, and that
// is the shape an end-to-end journey would otherwise be measuring.
package patexpiry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultWindow is the production look-ahead: a token is announced when it has
// less than a day left (ELITEA-0752).
const DefaultWindow = 24 * time.Hour

// DefaultBatchLimit bounds one pass. A deployment with more expiring keys than
// this simply announces the rest on the next tick — the candidate query orders
// by expiry, so the most urgent go first.
const DefaultBatchLimit = int32(500)

// Store is the persistence this notifier needs.
// repos.PATExpiryNotificationRepository satisfies it.
type Store interface {
	ListPATsNeedingExpiryNotice(ctx context.Context, now time.Time, within time.Duration, limit int32) ([]Candidate, error)
	NotifyPATExpiring(ctx context.Context, candidate Candidate, now time.Time) (bool, error)
}

// Candidate is one token due a warning. It mirrors
// repos.PATExpiryCandidate; the adapter in runtimecomposition converts.
type Candidate struct {
	TokenID   int64
	Name      string
	UserID    int64
	ProjectID int64
	Expires   time.Time
}

// Result reports what one pass did. `Produced` and `Skipped` are separate
// because they answer different questions: "did anyone get told" and "did the
// dedupe hold". A pass that examined ten candidates and produced nothing is
// the CORRECT second run of ELITEA-0754, and indistinguishable from a broken
// sweep if only one number is reported.
type Result struct {
	Examined int
	Produced int
	Skipped  int
}

// Notifier runs the pass.
type Notifier struct {
	store  Store
	window time.Duration
	limit  int32
}

// New builds a Notifier over `store`, with the production window.
func New(store Store) (*Notifier, error) {
	if store == nil {
		return nil, errors.New("personal access token expiry store is required")
	}
	return &Notifier{store: store, window: DefaultWindow, limit: DefaultBatchLimit}, nil
}

// Run performs one pass at `now`, looking `within` ahead. A zero or negative
// `within` uses the production window, so a caller that does not care cannot
// accidentally ask for a window of nothing.
func (n *Notifier) Run(ctx context.Context, now time.Time, within time.Duration) (Result, error) {
	if n == nil || n.store == nil {
		return Result{}, errors.New("personal access token expiry notifier is not configured")
	}
	if within <= 0 {
		within = n.window
	}
	candidates, err := n.store.ListPATsNeedingExpiryNotice(ctx, now, within, n.limit)
	if err != nil {
		return Result{}, fmt.Errorf("list expiring personal access tokens: %w", err)
	}
	result := Result{Examined: len(candidates)}
	for _, candidate := range candidates {
		produced, err := n.store.NotifyPATExpiring(ctx, candidate, now)
		if err != nil {
			// The pass stops at the first write failure and reports it. It does
			// NOT carry on: the failure modes here are "the notifications table
			// is gone" and "the mark could not be written", and continuing past
			// the second one is how a sweep produces the duplicate its mark
			// exists to prevent.
			return result, err
		}
		if produced {
			result.Produced++
			continue
		}
		result.Skipped++
	}
	return result, nil
}
