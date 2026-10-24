package failurelimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFailureLimiterBlocksAKeyAfterTheLimitAndForgetsItAfterTheWindow(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := New(2, time.Minute)
	limiter.now = func() time.Time { return now }

	blocked, _ := limiter.Blocked("client:a")
	require.False(t, blocked)
	limiter.Fail("client:a")
	limiter.Fail("client:a")
	blocked, retry := limiter.Blocked("client:a")
	require.True(t, blocked)
	require.Equal(t, time.Minute, retry)

	// Another key is not affected.
	blocked, _ = limiter.Blocked("client:b")
	require.False(t, blocked)

	now = now.Add(time.Minute)
	blocked, _ = limiter.Blocked("client:a")
	require.False(t, blocked)
}

func TestFailureLimiterKeyMapIsBounded(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := New(5, time.Minute)
	limiter.now = func() time.Time { return now }
	limiter.maxKeys = 3

	limiter.Fail("a")
	limiter.Fail("b")
	now = now.Add(2 * time.Minute)
	limiter.Fail("c")
	// Full: the TTL sweep removes the two ended windows and keeps "c".
	limiter.Fail("d")
	require.Equal(t, 2, limiter.Size())

	limiter.Fail("e")
	// Full of live windows: the map starts again rather than grow.
	limiter.Fail("f")
	require.LessOrEqual(t, limiter.Size(), 3)
	for i := 0; i < 1000; i++ {
		limiter.Fail(fmt.Sprintf("random-%d", i))
	}
	require.LessOrEqual(t, limiter.Size(), 3)
}

// Take charges and checks in one step: of many concurrent takers on one key,
// exactly the limit pass.
func TestFailureLimiterTakeAdmitsExactlyTheLimitUnderConcurrency(t *testing.T) {
	limiter := New(5, time.Minute)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := limiter.Take("actor:1"); ok {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 5, admitted.Load())
	ok, retry := limiter.Take("actor:1")
	require.False(t, ok)
	require.Positive(t, retry)
}

func TestFailureLimiterRefundGivesBackOneCharge(t *testing.T) {
	now := time.Unix(1_000, 0)
	limiter := New(1, time.Minute)
	limiter.now = func() time.Time { return now }

	ok, _ := limiter.Take("k")
	require.True(t, ok)
	ok, _ = limiter.Take("k")
	require.False(t, ok)
	limiter.Refund("k")
	ok, _ = limiter.Take("k")
	require.True(t, ok)

	// Refunding an unknown key or an ended window is a no-op.
	limiter.Refund("unknown")
	now = now.Add(2 * time.Minute)
	limiter.Refund("k")
	ok, _ = limiter.Take("k")
	require.True(t, ok)
}
