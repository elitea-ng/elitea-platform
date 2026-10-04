package nativepolicy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeLoader struct {
	policy platformconfig.NativeClientPolicy
	err    error
	calls  int
}

func (f *fakeLoader) load(context.Context) (platformconfig.NativeClientPolicy, error) {
	f.calls++
	return f.policy, f.err
}

type fakeClients []nativeauth.Client

func (f fakeClients) Clients(context.Context) ([]nativeauth.Client, error) { return f, nil }

func TestPolicyCachesForTTLAndInvalidates(t *testing.T) {
	loader := &fakeLoader{policy: platformconfig.DefaultNativeClientPolicy()}
	svc := NewWithLoader(loader.load, nil)
	now := time.Unix(1_000, 0)
	svc.SetClock(func() time.Time { return now })
	ctx := context.Background()

	if _, err := svc.Policy(ctx); err != nil {
		t.Fatal(err)
	}
	loader.policy.MinClientVersion = "2.0.0"
	got, _ := svc.Policy(ctx)
	if got.MinClientVersion != "" || loader.calls != 1 {
		t.Fatalf("within TTL the cached value is served: got %q after %d loads", got.MinClientVersion, loader.calls)
	}
	now = now.Add(CacheTTL)
	got, _ = svc.Policy(ctx)
	if got.MinClientVersion != "2.0.0" || loader.calls != 2 {
		t.Fatalf("after TTL the store is read: got %q after %d loads", got.MinClientVersion, loader.calls)
	}
	loader.policy.MinClientVersion = "3.0.0"
	svc.Invalidate()
	got, _ = svc.Policy(ctx)
	if got.MinClientVersion != "3.0.0" {
		t.Fatalf("Invalidate must force a read, got %q", got.MinClientVersion)
	}
}

func TestPolicyKeepsLastGoodOnFailureAndFailsCold(t *testing.T) {
	loader := &fakeLoader{err: errors.New("down")}
	svc := NewWithLoader(loader.load, nil)
	now := time.Unix(1_000, 0)
	svc.SetClock(func() time.Time { return now })
	ctx := context.Background()
	if _, err := svc.Policy(ctx); err == nil {
		t.Fatal("a cold cache over a failing store must return the error")
	}
	loader.err = nil
	loader.policy = platformconfig.DefaultNativeClientPolicy()
	loader.policy.RequireDeviceLock = true
	if _, err := svc.Policy(ctx); err != nil {
		t.Fatal(err)
	}
	loader.err = errors.New("down again")
	now = now.Add(CacheTTL)
	got, err := svc.Policy(ctx)
	if err != nil || !got.RequireDeviceLock {
		t.Fatalf("a failed refresh keeps the last good value: %+v, %v", got, err)
	}
	calls := loader.calls
	_, _ = svc.Policy(ctx)
	if loader.calls != calls {
		t.Fatal("a failed refresh re-arms the TTL: no query per request")
	}
}

func TestMinimumForIsTheHigherOfPolicyAndClient(t *testing.T) {
	loader := &fakeLoader{policy: platformconfig.DefaultNativeClientPolicy()}
	loader.policy.MinClientVersion = "1.5.0"
	clients := fakeClients{
		{ClientID: "ai.elitea.ios", Enabled: true, MinClientVersion: "2.0.0"},
		{ClientID: "ai.elitea.android", Enabled: true, MinClientVersion: "1.0.0"},
		{ClientID: "ai.elitea.desktop", Enabled: true},
		{ClientID: "ai.elitea.old", Enabled: false, MinClientVersion: "9.0.0"},
	}
	svc := NewWithLoader(loader.load, clients)
	ctx := context.Background()
	for id, want := range map[string]string{
		"ai.elitea.ios":     "2.0.0", // the client raises the floor
		"ai.elitea.android": "1.5.0", // a lower client minimum cannot lower it
		"ai.elitea.desktop": "1.5.0",
		"unknown":           "1.5.0",
	} {
		got, err := svc.MinimumFor(ctx, id)
		if err != nil || got != want {
			t.Errorf("MinimumFor(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
	_, mins, err := svc.Minimums(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ai.elitea.ios": "2.0.0", "ai.elitea.android": "1.5.0", "ai.elitea.desktop": "1.5.0"}
	if len(mins) != len(want) {
		t.Fatalf("Minimums = %v, want %v (disabled clients absent)", mins, want)
	}
	for id, v := range want {
		if mins[id] != v {
			t.Errorf("Minimums[%s] = %q, want %q", id, mins[id], v)
		}
	}

	loader.policy.MinClientVersion = ""
	svc.Invalidate()
	_, mins, _ = svc.Minimums(ctx)
	if len(mins) != 2 || mins["ai.elitea.ios"] != "2.0.0" || mins["ai.elitea.android"] != "1.0.0" {
		t.Fatalf("without a policy minimum only clients with their own appear: %v", mins)
	}
}

func TestDecorateResolvesTheClientsMinimum(t *testing.T) {
	loader := &fakeLoader{policy: platformconfig.DefaultNativeClientPolicy()}
	loader.policy.MinClientVersion = "1.0.0"
	loader.policy.RequireDeviceLock = true
	svc := NewWithLoader(loader.load, fakeClients{{ClientID: "ai.elitea.ios", Enabled: true, MinClientVersion: "1.2.0"}})
	body := map[string]any{"access_token": "x"}
	if err := svc.Decorate(context.Background(), "ai.elitea.ios", body); err != nil {
		t.Fatal(err)
	}
	policy, ok := body["client_policy"].(platformconfig.NativeClientPolicy)
	if !ok || policy.MinClientVersion != "1.2.0" || !policy.RequireDeviceLock || body["access_token"] != "x" {
		t.Fatalf("decorated body = %+v", body)
	}
	broken := NewWithLoader((&fakeLoader{err: errors.New("down")}).load, nil)
	body = map[string]any{}
	if err := broken.Decorate(context.Background(), "ai.elitea.ios", body); err == nil {
		t.Fatal("a cold failing store must report the error")
	}
	if _, present := body["client_policy"]; present {
		t.Fatal("no policy may be invented when none could be read")
	}
}
