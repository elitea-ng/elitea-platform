package workeregress

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type countingPlatform struct {
	settings platformconfig.WorkerEgressSettings
	err      error
	calls    int
}

func (p *countingPlatform) WorkerEgressSettings(context.Context) (platformconfig.WorkerEgressSettings, error) {
	p.calls++
	return p.settings, p.err
}

// The section is read once per TTL; a failed refresh answers the last value
// read, and with none the error, and backs off before the next read.
func TestCachedPlatformReader(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_800_000_000, 0)
	source := &countingPlatform{err: errors.New("database down")}
	cache := NewCachedPlatformReader(source)
	cache.now = func() time.Time { return clock }

	if _, err := cache.WorkerEgressSettings(ctx); err == nil {
		t.Fatal("a first failed read answered a value")
	}
	// Within the backoff the error is answered without a read.
	source.err = nil
	source.settings = platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressAllowlist, Allowlist: []string{"a.example"}}
	if _, err := cache.WorkerEgressSettings(ctx); err == nil || source.calls != 1 {
		t.Fatalf("within the backoff: err=%v reads=%d, want the error and no read", err, source.calls)
	}
	clock = clock.Add(platformRefreshBackoff)
	for range 3 {
		settings, err := cache.WorkerEgressSettings(ctx)
		if err != nil || settings.Mode != platformconfig.WorkerEgressAllowlist || len(settings.Allowlist) != 1 {
			t.Fatalf("%+v %v", settings, err)
		}
		settings.Allowlist[0] = "mutated.example" // a caller cannot change the cached value
	}
	if source.calls != 2 {
		t.Fatalf("reads within the TTL = %d, want 2 (the failure and one refresh)", source.calls)
	}

	// Within the TTL an administrator's save is not seen yet; after it, it is.
	source.settings = platformconfig.DefaultWorkerEgressSettings()
	clock = clock.Add(PlatformSettingsTTL - time.Millisecond)
	if settings, _ := cache.WorkerEgressSettings(ctx); settings.Mode != platformconfig.WorkerEgressAllowlist || settings.Allowlist[0] != "a.example" {
		t.Fatalf("within the TTL: %+v", settings)
	}
	clock = clock.Add(time.Millisecond)
	if settings, _ := cache.WorkerEgressSettings(ctx); settings.Mode != platformconfig.WorkerEgressOpen {
		t.Fatalf("after the TTL: %+v", settings)
	}

	// A failed refresh keeps the last value and is not retried on every
	// call: the next read waits for the backoff, which doubles.
	source.err = errors.New("database down")
	clock = clock.Add(PlatformSettingsTTL)
	calls := source.calls
	for range 3 {
		if settings, err := cache.WorkerEgressSettings(ctx); err != nil || settings.Mode != platformconfig.WorkerEgressOpen {
			t.Fatalf("stale fallback: %+v %v", settings, err)
		}
	}
	if source.calls != calls+1 {
		t.Fatalf("reads during the backoff = %d, want 1", source.calls-calls)
	}
	clock = clock.Add(platformRefreshBackoff)
	_, _ = cache.WorkerEgressSettings(ctx)
	clock = clock.Add(platformRefreshBackoff) // the second backoff is twice as long
	_, _ = cache.WorkerEgressSettings(ctx)
	if source.calls != calls+2 {
		t.Fatalf("reads after two backoffs = %d, want 2", source.calls-calls)
	}
	clock = clock.Add(platformRefreshBackoff)
	source.err = nil
	source.settings = platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressAllowlist, Allowlist: []string{"b.example"}}
	if settings, err := cache.WorkerEgressSettings(ctx); err != nil || settings.Allowlist[0] != "b.example" || source.calls != calls+3 {
		t.Fatalf("recovered: %+v %v reads=%d", settings, err, source.calls-calls)
	}
}

// blockingPlatform blocks every read until release is closed.
type blockingPlatform struct {
	release  chan struct{}
	started  chan struct{}
	once     sync.Once
	calls    atomic.Int32
	settings platformconfig.WorkerEgressSettings
}

func (p *blockingPlatform) WorkerEgressSettings(context.Context) (platformconfig.WorkerEgressSettings, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.started) })
	<-p.release
	return p.settings, nil
}

// A slow database never serialises dispatch: while one read of a stale
// value is in flight, every other caller is answered the last value at once;
// with no value yet, the callers share the one read.
func TestCachedPlatformReaderRefreshesOutsideTheLock(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_800_000_000, 0)
	var clockMu sync.Mutex
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }

	// No value yet: concurrent callers share one read.
	first := &blockingPlatform{release: make(chan struct{}), started: make(chan struct{}),
		settings: platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressAllowlist, Allowlist: []string{"a.example"}}}
	cache := NewCachedPlatformReader(first)
	cache.now = now
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if settings, err := cache.WorkerEgressSettings(ctx); err != nil || settings.Allowlist[0] != "a.example" {
				t.Errorf("first read: %+v %v", settings, err)
			}
		})
	}
	<-first.started
	close(first.release)
	wg.Wait()
	if got := first.calls.Load(); got != 1 {
		t.Fatalf("concurrent first reads = %d, want 1", got)
	}

	// A stale value: the refresh blocks, other callers are not held.
	slow := &blockingPlatform{release: make(chan struct{}), started: make(chan struct{}),
		settings: platformconfig.DefaultWorkerEgressSettings()}
	cache.next = slow
	clockMu.Lock()
	clock = clock.Add(PlatformSettingsTTL)
	clockMu.Unlock()
	leader := make(chan platformconfig.WorkerEgressSettings, 1)
	go func() {
		settings, _ := cache.WorkerEgressSettings(ctx)
		leader <- settings
	}()
	<-slow.started
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		for range 16 {
			if settings, err := cache.WorkerEgressSettings(ctx); err != nil || settings.Allowlist[0] != "a.example" {
				t.Errorf("during the refresh: %+v %v", settings, err)
			}
		}
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("callers waited for the refresh in flight")
	}
	close(slow.release)
	if settings := <-leader; settings.Mode != platformconfig.WorkerEgressOpen {
		t.Fatalf("the refreshing caller: %+v", settings)
	}
	if got := slow.calls.Load(); got != 1 {
		t.Fatalf("reads during one refresh = %d, want 1", got)
	}
	if settings, _ := cache.WorkerEgressSettings(ctx); settings.Mode != platformconfig.WorkerEgressOpen {
		t.Fatalf("after the refresh: %+v", settings)
	}
}
