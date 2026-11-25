// Package patexpiry produces the in-app notice a personal access token's
// owner gets shortly before the key stops working (issue #940 A3), and the
// notice to the owners of `inherit` tokens that their administration access
// ends (ADR-0059 decision 2; story R3.15b, issue 2619).
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
	// InheritOwners is the owners of `inherit` tokens whose notice is not
	// delivered yet; InheritQueued the notices this pass queued for them
	// (one per owner; 0 for an owner whose notice already waits to be sent).
	InheritOwners int
	InheritQueued int
}

// InheritStore is the persistence of the `inherit` token notice (R3.15b).
// repos.PATExpiryNotificationRepository satisfies it.
type InheritStore interface {
	// ListInheritTokenOwners answers the owners of `inherit` tokens whose
	// notice was not delivered yet, each with those tokens.
	ListInheritTokenOwners(ctx context.Context, now time.Time, limit int32) ([]InheritOwner, error)
	// QueueInheritTokenNotice queues the owner's one notice for the sunset
	// date and reports whether it queued one. The access notice sender
	// delivers it and stores the delivery time on each listed token
	// (token_lifecycle.inherit_notice_delivered_at).
	QueueInheritTokenNotice(ctx context.Context, owner InheritOwner, sunsetAfter string, now time.Time) (bool, error)
}

// InheritOwner is one owner of `inherit` tokens due a notice.
type InheritOwner struct {
	UserID int64
	Email  string
	Name   string
	Tokens []InheritToken
}

// InheritToken is one `inherit` token of an owner.
type InheritToken struct {
	ID   int64
	Name string
}

// Notifier runs the pass.
type Notifier struct {
	store  Store
	window time.Duration
	limit  int32
	// inherit and sunsetAfter are the `inherit` token notice's; nil leaves
	// that half of the pass out.
	inherit     InheritStore
	sunsetAfter func() string
}

// WithInheritNotices adds the `inherit` token notice to every pass: once
// sunsetAfter answers a date (adminaccess.InheritEndsOn), each owner of an
// `inherit` token whose notice was not delivered gets one queued. While it
// answers "" the half does nothing, so a deployment that upgrades later sends
// the notice when it first runs with the date set.
func (n *Notifier) WithInheritNotices(store InheritStore, sunsetAfter func() string) error {
	if n == nil || store == nil || sunsetAfter == nil {
		return errors.New("the inherit token notice needs a notifier, a store and the sunset date")
	}
	n.inherit, n.sunsetAfter = store, sunsetAfter
	return nil
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
	return n.inheritPass(ctx, now, result)
}

// inheritPass queues the `inherit` token notices. It does nothing while the
// sunset date is not set. A queue failure stops it and the next pass retries:
// nothing is stored as delivered until the mailer accepted the message, so an
// owner whose notice failed is listed again.
func (n *Notifier) inheritPass(ctx context.Context, now time.Time, result Result) (Result, error) {
	if n.inherit == nil {
		return result, nil
	}
	sunsetAfter := n.sunsetAfter()
	if sunsetAfter == "" {
		return result, nil
	}
	owners, err := n.inherit.ListInheritTokenOwners(ctx, now, n.limit)
	if err != nil {
		return result, fmt.Errorf("list owners of inherit tokens: %w", err)
	}
	result.InheritOwners = len(owners)
	for _, owner := range owners {
		queued, err := n.inherit.QueueInheritTokenNotice(ctx, owner, sunsetAfter, now)
		if err != nil {
			return result, fmt.Errorf("queue the inherit token notice of user %d: %w", owner.UserID, err)
		}
		if queued {
			result.InheritQueued++
		}
	}
	return result, nil
}
