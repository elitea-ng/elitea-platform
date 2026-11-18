package delegationnotice

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingPermissions struct {
	allowed bool
	calls   int
}

func (c *countingPermissions) HasProjectPermission(context.Context, int64, int64, string) (bool, error) {
	c.calls++
	return c.allowed, nil
}

func TestServiceAsksThePermissionOncePerViewerAndAsksAgainAfterTheTTL(t *testing.T) {
	permissions := &countingPermissions{allowed: true}
	service := NewService(permissions, fakeEditors{})
	clock := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	for _, caller := range []int64{1, 2, 3} {
		if _, err := service.DelegationReview(t.Context(), 7, 9, caller); err != nil {
			t.Fatal(err)
		}
	}
	if permissions.calls != 1 {
		t.Fatalf("calls = %d, want one per viewer, not per calling agent", permissions.calls)
	}
	clock = clock.Add(reviewTTL + time.Second)
	permissions.allowed = false
	review, _ := service.DelegationReview(t.Context(), 7, 9, 1)
	if permissions.calls != 2 || review.CanReview {
		t.Fatalf("a role change was not seen after the TTL: calls=%d review=%+v", permissions.calls, review)
	}
}

func TestCachedKeepsAnAnswerForTheTTLAndAFailureBriefly(t *testing.T) {
	inner := &fakeReviewer{review: Review{CanReview: true}}
	reviewer, ok := Cached(inner).(*cached)
	if !ok {
		t.Fatal("Cached did not wrap the reviewer")
	}
	clock := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reviewer.now = func() time.Time { return clock }
	_, _ = reviewer.DelegationReview(t.Context(), 1, 2, 3)
	clock = clock.Add(reviewTTL - time.Second)
	_, _ = reviewer.DelegationReview(t.Context(), 1, 2, 3)
	if inner.calls != 1 {
		t.Fatalf("calls = %d inside the TTL", inner.calls)
	}
	clock = clock.Add(2 * time.Second)
	_, _ = reviewer.DelegationReview(t.Context(), 1, 2, 3)
	if inner.calls != 2 {
		t.Fatalf("calls = %d after the TTL: a stream kept a stale permission", inner.calls)
	}

	failing := &fakeReviewer{err: errors.New("down")}
	retried, ok := Cached(failing).(*cached)
	if !ok {
		t.Fatal("Cached did not wrap the reviewer")
	}
	retried.now = func() time.Time { return clock }
	for i := 0; i < 5; i++ {
		if _, err := retried.DelegationReview(t.Context(), 1, 2, 3); err == nil {
			t.Fatal("a failure was served as an answer")
		}
	}
	if failing.calls != 1 {
		t.Fatalf("a failing lookup was retried by every frame: %d calls", failing.calls)
	}
	clock = clock.Add(failureTTL + time.Second)
	_, _ = retried.DelegationReview(t.Context(), 1, 2, 3)
	if failing.calls != 2 {
		t.Fatalf("calls = %d after the failure TTL", failing.calls)
	}
}
