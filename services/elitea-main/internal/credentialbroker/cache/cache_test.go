package cache

import (
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
)

const (
	bindingA = "0192b6e4-0000-7000-8000-00000000000a"
	grantA   = "0192b6e4-0000-7000-8000-00000000000b"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func newTestCache(t *testing.T, limits Limits) *Cache {
	t.Helper()
	c, err := New(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func defaultLimits() Limits { return Limits{MaxEntries: 64, MaxBytes: 1 << 20, Skew: 30 * time.Second} }

func baseKey() PartitionKey {
	return PartitionKey{
		TenantProjectID:       42,
		AgentBindingID:        bindingA,
		SubjectPrincipalRef:   "user:7",
		AuthorityID:           "0192b6e4-0000-7000-8000-00000000000c",
		CredentialReferenceID: "0192b6e4-0000-7000-8000-00000000000d",
		TargetBindingID:       "0192b6e4-0000-7000-8000-00000000000e",
		ScopeDigest:           ScopeDigest([]string{"https://graph.microsoft.com/Sites.Selected"}, "read"),
		PolicyRevisionID:      "2026-10-r1",
		Generation:            1,
	}
}

func epochs(binding int64) []epoch.SecurityEpoch {
	return []epoch.SecurityEpoch{
		{Kind: epoch.KindBinding, ID: bindingA, Value: binding},
		{Kind: epoch.KindGrant, ID: grantA, Value: 4},
	}
}

func entry(token string, notAfter time.Time, e []epoch.SecurityEpoch) Entry {
	return Entry{Kind: "oauth_access", Material: map[string][]byte{"access_token": []byte(token)}, NotAfter: notAfter, Epochs: e}
}

// Two keys that differ in any single PartitionKey field never share an
// entry (#1121 acceptance, 9 cases).
func TestPartitionKeyEveryFieldSeparatesEntries(t *testing.T) {
	cases := map[string]func(*PartitionKey){
		"tenant project":       func(k *PartitionKey) { k.TenantProjectID++ },
		"agent binding":        func(k *PartitionKey) { k.AgentBindingID = "0192b6e4-0000-7000-8000-0000000000f1" },
		"subject principal":    func(k *PartitionKey) { k.SubjectPrincipalRef = "user:8" },
		"authority":            func(k *PartitionKey) { k.AuthorityID = "0192b6e4-0000-7000-8000-0000000000f2" },
		"credential reference": func(k *PartitionKey) { k.CredentialReferenceID = "0192b6e4-0000-7000-8000-0000000000f3" },
		"target binding":       func(k *PartitionKey) { k.TargetBindingID = "0192b6e4-0000-7000-8000-0000000000f4" },
		"scope digest": func(k *PartitionKey) {
			k.ScopeDigest = ScopeDigest([]string{"https://graph.microsoft.com/Sites.Selected"}, "write")
		},
		"policy revision": func(k *PartitionKey) { k.PolicyRevisionID = "2026-10-r2" },
		"generation":      func(k *PartitionKey) { k.Generation++ },
	}
	if len(cases) != 9 {
		t.Fatalf("%d cases; PartitionKey has 9 fields", len(cases))
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestCache(t, defaultLimits())
			c.Put(baseKey(), entry("token-a", t0.Add(time.Hour), epochs(1)))
			other := baseKey()
			mutate(&other)
			if other == baseKey() {
				t.Fatal("the mutation changed nothing")
			}
			if got, ok := c.Get(other, epochs(1), t0); ok {
				got.Destroy()
				t.Fatal("a key differing in one field was served another key's entry")
			}
			got, ok := c.Get(baseKey(), epochs(1), t0)
			if !ok || string(got.Material["access_token"]) != "token-a" {
				t.Fatal("the original key lost its entry")
			}
			got.Destroy()
		})
	}
}

func TestScopeDigestIsOrderInsensitiveAndClassSensitive(t *testing.T) {
	if ScopeDigest([]string{"b", "a"}, "read") != ScopeDigest([]string{"a", "b", "a"}, "read") {
		t.Fatal("scope order or duplicates changed the digest")
	}
	if ScopeDigest([]string{"a"}, "read") == ScopeDigest([]string{"a"}, "write") ||
		ScopeDigest([]string{"ab"}, "") == ScopeDigest([]string{"a", "b"}, "") {
		t.Fatal("distinct scope sets or classes collided")
	}
}

// An epoch bump makes the next lookup a miss and zeroes the stored value.
func TestEpochBumpMakesTheNextHitAMiss(t *testing.T) {
	c := newTestCache(t, defaultLimits())
	c.Put(baseKey(), entry("token-a", t0.Add(time.Hour), epochs(1)))
	c.mu.Lock()
	stored := c.items[baseKey()].Value.(*item).entry.Material["access_token"]
	c.mu.Unlock()
	if _, ok := c.Get(baseKey(), epochs(2), t0); ok {
		t.Fatal("an entry was served at a newer epoch")
	}
	if c.Len() != 0 {
		t.Fatal("the stale entry stayed cached")
	}
	for _, b := range stored {
		if b != 0 {
			t.Fatal("the evicted value was not zeroed")
		}
	}
	if _, ok := c.Get(baseKey(), epochs(1), t0); ok {
		t.Fatal("the evicted entry came back")
	}
}

func TestEntryIsNeverServedWithinTheSkewOfNotAfter(t *testing.T) {
	c := newTestCache(t, defaultLimits())
	notAfter := t0.Add(time.Minute)
	c.Put(baseKey(), entry("token-a", notAfter, epochs(1)))
	if got, ok := c.Get(baseKey(), epochs(1), notAfter.Add(-31*time.Second)); !ok {
		t.Fatal("a fresh entry missed")
	} else {
		got.Destroy()
	}
	if _, ok := c.Get(baseKey(), epochs(1), notAfter.Add(-30*time.Second)); ok {
		t.Fatal("an entry was served at not_after minus the skew")
	}
	c.Put(baseKey(), entry("token-a", notAfter, epochs(1)))
	if _, ok := c.Get(baseKey(), epochs(1), notAfter.Add(time.Second)); ok {
		t.Fatal("an entry was served past not_after")
	}
}

func TestGetReturnsACopyAndPutCopiesItsInput(t *testing.T) {
	c := newTestCache(t, defaultLimits())
	in := entry("token-a", t0.Add(time.Hour), epochs(1))
	c.Put(baseKey(), in)
	in.Destroy()
	got, ok := c.Get(baseKey(), epochs(1), t0)
	if !ok || string(got.Material["access_token"]) != "token-a" {
		t.Fatal("destroying the caller's entry reached the cache")
	}
	got.Destroy()
	again, ok := c.Get(baseKey(), epochs(1), t0)
	if !ok || string(again.Material["access_token"]) != "token-a" {
		t.Fatal("destroying a returned copy reached the cache")
	}
	again.Destroy()
}

func TestBoundsEvictLeastRecentlyUsed(t *testing.T) {
	c := newTestCache(t, Limits{MaxEntries: 2, MaxBytes: 1 << 20, Skew: time.Second})
	keys := []PartitionKey{baseKey(), baseKey(), baseKey()}
	keys[1].Generation, keys[2].Generation = 2, 3
	c.Put(keys[0], entry("a", t0.Add(time.Hour), epochs(1)))
	c.Put(keys[1], entry("b", t0.Add(time.Hour), epochs(1)))
	if got, ok := c.Get(keys[0], epochs(1), t0); ok { // keys[0] is now most recent
		got.Destroy()
	}
	c.Put(keys[2], entry("c", t0.Add(time.Hour), epochs(1)))
	if c.Len() != 2 {
		t.Fatalf("len = %d", c.Len())
	}
	if _, ok := c.Get(keys[1], epochs(1), t0); ok {
		t.Fatal("the least recently used entry survived")
	}

	small := newTestCache(t, Limits{MaxEntries: 10, MaxBytes: int64(len("access_token") + 4), Skew: time.Second})
	small.Put(keys[0], entry("aaaa", t0.Add(time.Hour), epochs(1)))
	small.Put(keys[1], entry("bbbb", t0.Add(time.Hour), epochs(1)))
	if small.Len() != 1 || small.Bytes() > small.Limits().MaxBytes {
		t.Fatalf("byte bound broken: len %d bytes %d", small.Len(), small.Bytes())
	}
	small.Put(keys[2], entry("too-large-value", t0.Add(time.Hour), epochs(1)))
	if _, ok := small.Get(keys[2], epochs(1), t0); ok {
		t.Fatal("an entry larger than the byte bound was stored")
	}
}

func TestInvalidateEpochDropsOnlyOlderEntriesOfThatRecord(t *testing.T) {
	c := newTestCache(t, defaultLimits())
	other := baseKey()
	other.Generation = 9
	c.Put(baseKey(), entry("a", t0.Add(time.Hour), epochs(1)))
	c.Put(other, entry("b", t0.Add(time.Hour), epochs(5)))
	// The binding moved to epoch 2: the entry filled at 1 goes, the one
	// filled at 5 (a newer read) stays.
	if n := c.InvalidateEpoch(epoch.SecurityEpoch{Kind: epoch.KindBinding, ID: bindingA, Value: 2}); n != 1 {
		t.Fatalf("invalidated %d", n)
	}
	if c.Len() != 1 {
		t.Fatalf("len = %d", c.Len())
	}
	c.Listener()(epoch.Change{Epoch: epoch.SecurityEpoch{Kind: epoch.KindGrant, ID: grantA, Value: 5}})
	if c.Len() != 0 {
		t.Fatal("the hub listener did not invalidate")
	}
	c.Put(baseKey(), entry("a", t0.Add(time.Hour), epochs(1)))
	if n := c.Invalidate(func(k PartitionKey) bool { return k.AgentBindingID == bindingA }); n != 1 || c.Len() != 0 {
		t.Fatalf("Invalidate(match) evicted %d", n)
	}
}

func TestPutRefusesAnEntryWithoutEpochs(t *testing.T) {
	c := newTestCache(t, defaultLimits())
	c.Put(baseKey(), entry("a", t0.Add(time.Hour), nil))
	if c.Len() != 0 {
		t.Fatal("an entry without epochs was stored")
	}
}
