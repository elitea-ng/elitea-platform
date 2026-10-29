package workeregress

import (
	"context"
	"errors"
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
// read, and with none the error.
func TestCachedPlatformReader(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_800_000_000, 0)
	source := &countingPlatform{err: errors.New("database down")}
	cache := NewCachedPlatformReader(source)
	cache.now = func() time.Time { return clock }

	if _, err := cache.WorkerEgressSettings(ctx); err == nil {
		t.Fatal("a first failed read answered a value")
	}
	source.err = nil
	source.settings = platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressAllowlist, Allowlist: []string{"a.example"}}
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

	// A failed refresh keeps the last value and tries again next call.
	source.err = errors.New("database down")
	clock = clock.Add(PlatformSettingsTTL)
	calls := source.calls
	for range 2 {
		if settings, err := cache.WorkerEgressSettings(ctx); err != nil || settings.Mode != platformconfig.WorkerEgressOpen {
			t.Fatalf("stale fallback: %+v %v", settings, err)
		}
	}
	if source.calls != calls+2 {
		t.Fatalf("a failed refresh was not retried: %d", source.calls-calls)
	}
}
