package cache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
)

// Limits bound one replica's cache. Every value comes from the policy
// revision (profile.CachePolicy); the defaults are test values only.
type Limits struct {
	MaxEntries int
	MaxBytes   int64
	// Skew is subtracted from not_after: an entry is never served within
	// Skew of its expiry.
	Skew time.Duration
}

// Entry is one cached credential. Material is owned by whoever holds the
// Entry: Get returns a copy the caller must Destroy, Put copies its input.
type Entry struct {
	// Kind is the envelope kind ("oauth_access", "static").
	Kind     string
	Material map[string][]byte
	NotAfter time.Time
	// Epochs are the current epochs read when the entry was filled.
	Epochs []epoch.SecurityEpoch
}

// Destroy zeroes the material.
func (e *Entry) Destroy() {
	for key, value := range e.Material {
		clear(value)
		delete(e.Material, key)
	}
}

func (e Entry) size() int64 {
	var n int64
	for key, value := range e.Material {
		n += int64(len(key) + len(value))
	}
	return n
}

func (e Entry) clone() Entry {
	out := Entry{Kind: e.Kind, NotAfter: e.NotAfter, Epochs: append([]epoch.SecurityEpoch(nil), e.Epochs...)}
	out.Material = make(map[string][]byte, len(e.Material))
	for key, value := range e.Material {
		out.Material[key] = append([]byte(nil), value...)
	}
	return out
}

// Store is the narrow interface every credential path puts through: the
// broker's Entra on-behalf-of tokens and static secrets, and provider OAuth
// access tokens.
type Store interface {
	Get(key PartitionKey, current []epoch.SecurityEpoch, now time.Time) (Entry, bool)
	Put(key PartitionKey, entry Entry)
	Invalidate(match func(PartitionKey) bool) int
}

const metricLookups = "elitea.credential.cache.lookups"

type item struct {
	key   PartitionKey
	entry Entry
	size  int64
}

// Cache is a bounded LRU keyed by the full PartitionKey.
type Cache struct {
	limits Limits

	mu    sync.Mutex
	ll    *list.List
	items map[PartitionKey]*list.Element
	bytes int64

	lookups metric.Int64Counter
}

// New builds a cache. A meter may be nil (the global one).
func New(limits Limits, meter metric.Meter) (*Cache, error) {
	if limits.MaxEntries <= 0 || limits.MaxBytes <= 0 || limits.Skew < 0 {
		return nil, errors.New("credential cache: max entries and bytes must be positive and the skew not negative")
	}
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	lookups, err := meter.Int64Counter(metricLookups,
		metric.WithDescription("Governed credential cache lookups, by result (hit, miss, stale, expired)."))
	if err != nil {
		return nil, err
	}
	return &Cache{limits: limits, ll: list.New(), items: map[PartitionKey]*list.Element{}, lookups: lookups}, nil
}

// Limits answers the cache's bounds.
func (c *Cache) Limits() Limits { return c.limits }

func (c *Cache) count(result string) {
	c.lookups.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

// Get answers a copy of the entry of key when every current epoch equals a
// stored one and now is before not_after minus the skew. A stale or expiring
// entry is evicted (and zeroed) and answers a miss. The caller destroys the
// copy.
func (c *Cache) Get(key PartitionKey, current []epoch.SecurityEpoch, now time.Time) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		c.count("miss")
		return Entry{}, false
	}
	it := element.Value.(*item)
	if !epoch.Equal(it.entry.Epochs, current) {
		c.removeLocked(element)
		c.count("stale")
		return Entry{}, false
	}
	if !now.Before(it.entry.NotAfter.Add(-c.limits.Skew)) {
		c.removeLocked(element)
		c.count("expired")
		return Entry{}, false
	}
	c.ll.MoveToFront(element)
	c.count("hit")
	return it.entry.clone(), true
}

// Put stores a copy of entry under key, replacing any entry there, then
// evicts least-recently-used entries until both bounds hold. An entry
// without epochs, without material or larger than the byte bound is not
// stored.
func (c *Cache) Put(key PartitionKey, entry Entry) {
	if c == nil || len(entry.Material) == 0 || len(entry.Epochs) == 0 || entry.NotAfter.IsZero() {
		return
	}
	owned := entry.clone()
	size := owned.size()
	if size > c.limits.MaxBytes {
		owned.Destroy()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.removeLocked(element)
	}
	c.items[key] = c.ll.PushFront(&item{key: key, entry: owned, size: size})
	c.bytes += size
	for c.ll.Len() > c.limits.MaxEntries || c.bytes > c.limits.MaxBytes {
		c.removeLocked(c.ll.Back())
	}
}

// Invalidate evicts (and zeroes) every entry whose key matches.
func (c *Cache) Invalidate(match func(PartitionKey) bool) int {
	if c == nil || match == nil {
		return 0
	}
	return c.evict(func(it *item) bool { return match(it.key) })
}

// InvalidateEpoch evicts every entry filled at an older epoch of the record
// the change names. It is the hub listener.
func (c *Cache) InvalidateEpoch(e epoch.SecurityEpoch) int {
	if c == nil {
		return 0
	}
	return c.evict(func(it *item) bool { return epoch.Contains(it.entry.Epochs, e) })
}

// Listener adapts InvalidateEpoch to the hub.
func (c *Cache) Listener() epoch.Listener {
	return func(change epoch.Change) { c.InvalidateEpoch(change.Epoch) }
}

func (c *Cache) evict(match func(*item) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for element := c.ll.Front(); element != nil; {
		next := element.Next()
		if match(element.Value.(*item)) {
			c.removeLocked(element)
			n++
		}
		element = next
	}
	return n
}

// Len answers the number of entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Bytes answers the bytes held.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *Cache) removeLocked(element *list.Element) {
	it := element.Value.(*item)
	c.ll.Remove(element)
	delete(c.items, it.key)
	c.bytes -= it.size
	it.entry.Destroy()
}

var _ Store = (*Cache)(nil)
