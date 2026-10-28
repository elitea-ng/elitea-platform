package runtoken

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingLeases struct {
	live  bool
	err   error
	calls int
}

func (l *countingLeases) ClaimLive(context.Context, Binding) (bool, error) {
	l.calls++
	return l.live, l.err
}

// A live answer is reused within the TTL and re-read after it; a refusal
// and a store fault are never reused.
func TestCachedLeasesReuseOnlyALiveAnswer(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	inner := &countingLeases{live: true}
	cache := NewCachedLeases(inner)
	cache.now = func() time.Time { return now }
	binding := Binding{ClaimID: "c", ExecutionID: "e", Generation: 1, FenceToken: make([]byte, 32), ProjectID: 7, UserID: 3, TokenID: 9}

	for range 3 {
		if live, err := cache.ClaimLive(ctx, binding); !live || err != nil {
			t.Fatalf("live = %v %v", live, err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("calls within the TTL = %d, want 1", inner.calls)
	}
	other := binding
	other.FenceToken = append(make([]byte, 31), 1)
	if _, _ = cache.ClaimLive(ctx, other); inner.calls != 2 {
		t.Fatalf("another fence shares the entry: calls = %d", inner.calls)
	}

	now = now.Add(LiveCacheTTL)
	inner.live = false
	if live, _ := cache.ClaimLive(ctx, binding); live || inner.calls != 3 {
		t.Fatalf("after the TTL: live = %v calls = %d", live, inner.calls)
	}
	if live, _ := cache.ClaimLive(ctx, binding); live || inner.calls != 4 {
		t.Fatalf("a refusal was cached: live = %v calls = %d", live, inner.calls)
	}
	inner.live, inner.err = false, errors.New("down")
	if _, err := cache.ClaimLive(ctx, binding); err == nil || inner.calls != 5 {
		t.Fatalf("a fault: err = %v calls = %d", err, inner.calls)
	}
}
