package canvaspresence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// PresenceBucket is the JetStream KV bucket that holds every canvas roster.
const PresenceBucket = "ELITEA_CANVAS_PRESENCE"

// NATSStore is the cross-replica Store: one JetStream KV entry per
// (roster, editor), on the same NATS server as the live-update bus the roster
// is published on, READ from an in-process mirror of the bucket.
//
// # Layout
//
// Key = base64url(rosterKey) + "." + base64url(userID). RosterKey contains ':'
// and user ids are not ours to constrain, while KV keys allow only
// [-/_=.a-zA-Z0-9]; unpadded base64url maps any input into [A-Za-z0-9_-], so no
// caller-supplied byte can become a '.' token separator or a '*'/'>' wildcard.
// The mirror groups entries by the first token, so one roster is one map.
//
// # Reads come from a mirror, writes go to KV
//
// Every replica runs ONE watcher over the whole bucket (an ordered JetStream
// consumer, started by NewNATSStore) and keeps the latest value of each key
// in memory. List reads that mirror. The previous design opened and tore down
// an ordered consumer per List, i.e. per heartbeat — a consumer create and
// delete on the server for every open canvas tab every 40 seconds.
//
// Writes (Touch, Remove) go to KV as before and, once KV has accepted them,
// are applied to the local mirror too, so the writing replica reads its own
// write in the heartbeat's response. Other replicas see it when their watcher
// delivers it, normally within milliseconds.
//
// Every mirror update is guarded by the KV REVISION (the stream sequence), so
// the order in which a local write and the watcher's delivery of it — or of an
// older or newer write to the same key — arrive cannot move an entry
// backwards. A delete leaves a revisioned tombstone for the same reason.
//
// The mirror resyncs (a fresh watcher, swapped in once its initial values are
// all delivered) when its watcher ends and when Resync is called; the
// composition root calls it on every NATS reconnect.
//
// # Expiry: the same two layers the Redis HASH store it replaced had
//
//   - The VALUE carries its own deadline (storedEntry.ExpiresAt, from Touch's
//     ttl), and List drops anything past it. This is what decides the answer,
//     so a stale editor is gone from the next roster exactly one TTL after its
//     last beat, per editor — not when the whole roster expires (the
//     reference's design: one Redis SET expired wholesale after 120s). It is
//     also what removes an entry from the MIRROR: the server's MaxAge removal
//     produces no watcher event, so the mirror prunes by deadline itself.
//   - The BUCKET's MaxAge is the garbage collector, standing in for the Redis
//     key's PEXPIRE: every Touch is a new message, so an entry nobody refreshes
//     is removed by the server one MaxAge after its last beat, and a canvas
//     nobody touches leaves nothing behind. MaxAge is TTL, the only ttl the
//     handler passes in production; a Touch with a LONGER ttl would be
//     collected before its deadline, so callers must not exceed it.
//
// Per-message TTL (NATS 2.11+ AllowMsgTTL / jetstream.KeyTTL) is deliberately
// not used: nats.go only offers it on Create, not Put, so a heartbeat could
// not refresh it; and bucket MaxAge works on every server version.
//
// Storage is FILE because the shipped NATS chart disables JetStream's memory
// store (deploy/helm/nats/values-*.yaml); the working set is one small
// message per open canvas tab and lives at most MaxAge.
type NATSStore struct {
	kv  jetstream.KeyValue
	now func() time.Time

	mu sync.RWMutex
	// current is what List reads: roster prefix → user token → record.
	current mirror
	// next is non-nil while a resync's watcher is delivering its initial
	// values; every update (local or watched) is applied to both, and next
	// replaces current at the watcher's end-of-initial-values marker.
	next mirror

	resync  chan struct{}
	stop    context.CancelFunc
	stopped chan struct{}
}

// mirror is roster prefix → user token → record.
type mirror map[string]map[string]record

// record is one key's latest known state.
type record struct {
	revision uint64
	// deleted marks a tombstone: the key's latest revision is a delete.
	deleted bool
	// entry is the decoded value; ExpiresAt 0 (unreadable) is never live.
	entry storedEntry
	// seen is when a tombstone was written, for pruning.
	seen time.Time
}

// NATSStoreConfig shapes the bucket.
type NATSStoreConfig struct {
	// Replicas is the bucket's replica count. 0 means 1, which is right for
	// the scale-1 NATS profile; an HA (3-node) server may take 3. Presence is
	// ephemeral, so R1 on an HA server only means a node loss empties rosters
	// (they refill on the next beat) — it never loses data that matters.
	Replicas int
}

// natsMirrorPruneInterval is how often the mirror drops expired entries and
// old tombstones. Expired entries are already invisible to List; this only
// bounds memory.
const natsMirrorPruneInterval = 30 * time.Second

// natsWatchRetryDelay spaces out restarts of a watcher that keeps failing
// (e.g. while NATS is down), so the loop does not spin.
const natsWatchRetryDelay = time.Second

// NewNATSStore creates the bucket if it is absent (or updates its config),
// starts the replica's bucket watcher and waits — bounded by ctx — until the
// mirror holds the bucket's current contents. A JetStream-less server fails
// here, at startup. Close stops the watcher.
func NewNATSStore(ctx context.Context, js jetstream.JetStream, cfg NATSStoreConfig) (*NATSStore, error) {
	if js == nil {
		return nil, errors.New("canvaspresence: a JetStream context is required")
	}
	replicas := cfg.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      PresenceBucket,
		Description: "elitea-main canvas presence rosters (one entry per editor; TTL-bounded)",
		History:     1,
		TTL:         TTL,
		Storage:     jetstream.FileStorage,
		Replicas:    replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("canvaspresence: create KV bucket %s: %w", PresenceBucket, err)
	}
	store := newNATSStoreOver(kv)
	ready := make(chan struct{})
	loopCtx, stop := context.WithCancel(context.Background())
	store.stop = stop
	go store.run(loopCtx, ready)
	select {
	case <-ready:
		return store, nil
	case <-ctx.Done():
		store.Close()
		return nil, fmt.Errorf("canvaspresence: initial sync of KV bucket %s: %w", PresenceBucket, ctx.Err())
	}
}

func newNATSStoreOver(kv jetstream.KeyValue) *NATSStore {
	return &NATSStore{
		kv:      kv,
		now:     time.Now,
		current: mirror{},
		resync:  make(chan struct{}, 1),
		stopped: make(chan struct{}),
	}
}

// SetClock replaces time.Now. Tests use it to walk entries past their
// deadline without waiting out the bucket's MaxAge.
func (s *NATSStore) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Resync asks the watcher to rebuild the mirror from a fresh watch of the
// bucket. It does not block; the current mirror keeps serving until the new
// one is complete. The composition root calls it on every NATS reconnect.
func (s *NATSStore) Resync() {
	select {
	case s.resync <- struct{}{}:
	default:
	}
}

// Close stops the watcher and waits for it to exit. Safe to call twice.
func (s *NATSStore) Close() {
	if s.stop == nil {
		return
	}
	s.stop()
	<-s.stopped
}

// storedEntry is one KV entry's value: the editor plus its own deadline.
type storedEntry struct {
	Editor    Editor `json:"editor"`
	ExpiresAt int64  `json:"expires_at_unix_milli"`
}

var keyEncoding = base64.RawURLEncoding

func natsRosterPrefix(rosterKey string) string {
	return keyEncoding.EncodeToString([]byte(rosterKey))
}

func natsUserToken(userID string) string {
	return keyEncoding.EncodeToString([]byte(userID))
}

func natsEntryKey(rosterKey, userID string) string {
	return natsRosterPrefix(rosterKey) + "." + natsUserToken(userID)
}

// splitEntryKey reverses natsEntryKey's join. A key of any other shape (not
// written by this store) is ignored.
func splitEntryKey(key string) (roster, user string, ok bool) {
	roster, user, ok = strings.Cut(key, ".")
	if !ok || roster == "" || user == "" || strings.Contains(user, ".") {
		return "", "", false
	}
	return roster, user, true
}

func (s *NATSStore) Touch(ctx context.Context, key string, editor Editor, ttl time.Duration) error {
	s.mu.RLock()
	now := s.now()
	s.mu.RUnlock()
	held := storedEntry{Editor: editor, ExpiresAt: now.Add(ttl).UnixMilli()}
	value, err := json.Marshal(held)
	if err != nil {
		return fmt.Errorf("canvaspresence: marshal entry: %w", err)
	}
	revision, err := s.kv.Put(ctx, natsEntryKey(key, editor.UserID), value)
	if err != nil {
		return fmt.Errorf("canvaspresence: touch: %w", err)
	}
	// Read-your-writes: the heartbeat's own List must see this beat.
	s.apply(natsRosterPrefix(key), natsUserToken(editor.UserID), record{revision: revision, entry: held})
	return nil
}

// Remove writes a delete marker. Removing an absent editor is not an error:
// KV Delete does not check that the key exists.
func (s *NATSStore) Remove(ctx context.Context, key string, userID string) error {
	if err := s.kv.Delete(ctx, natsEntryKey(key, userID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("canvaspresence: remove: %w", err)
	}
	// Read-your-writes. KV Delete does not return the marker's revision, so
	// the local tombstone takes the revision the mirror already holds for the
	// key: it hides that value now, and the watcher's delivery of the real
	// marker (a higher revision) replaces it.
	roster, user := natsRosterPrefix(key), natsUserToken(userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	tombstone := func(m mirror) {
		if m == nil {
			return
		}
		held, ok := m[roster][user]
		if !ok {
			return
		}
		setRecord(m, roster, user, record{revision: held.revision, deleted: true, seen: s.now()})
	}
	tombstone(s.current)
	tombstone(s.next)
	return nil
}

// List reads the roster from the replica's mirror of the bucket.
func (s *NATSStore) List(ctx context.Context, key string) ([]Editor, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("canvaspresence: list: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now().UnixMilli()
	live := []Editor{}
	for _, held := range s.current[natsRosterPrefix(key)] {
		if held.deleted || held.entry.ExpiresAt <= now {
			continue
		}
		live = append(live, held.entry.Editor)
	}
	return sortRoster(live), nil
}

// apply installs r for (roster, user) in the mirror(s) unless the mirror
// already holds a newer revision of that key. Equal revisions: a tombstone
// wins over a value (Remove's local tombstone reuses the value's revision).
func (s *NATSStore) apply(roster, user string, r record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	applyTo(s.current, roster, user, r)
	if s.next != nil {
		applyTo(s.next, roster, user, r)
	}
}

func applyTo(m mirror, roster, user string, r record) {
	if held, ok := m[roster][user]; ok {
		if held.revision > r.revision || (held.revision == r.revision && (held.deleted || !r.deleted)) {
			return
		}
	}
	setRecord(m, roster, user, r)
}

func setRecord(m mirror, roster, user string, r record) {
	users, ok := m[roster]
	if !ok {
		users = map[string]record{}
		m[roster] = users
	}
	users[user] = r
}

// applyEntry turns one watcher delivery into a mirror update.
func (s *NATSStore) applyEntry(entry jetstream.KeyValueEntry) {
	roster, user, ok := splitEntryKey(entry.Key())
	if !ok {
		return
	}
	r := record{revision: entry.Revision()}
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		r.deleted = true
		s.mu.RLock()
		r.seen = s.now()
		s.mu.RUnlock()
	default:
		// Unreadable is stale by definition: ExpiresAt stays 0, so List
		// never returns it and the pruner drops it. MaxAge collects it on
		// the server.
		_ = json.Unmarshal(entry.Value(), &r.entry)
	}
	s.apply(roster, user, r)
}

// prune drops expired entries and tombstones older than TTL from the mirror.
// A tombstone only has to outlive any write that could still be in flight
// when it was set; TTL is far longer than that.
func (s *NATSStore) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	nowMilli := now.UnixMilli()
	for _, m := range []mirror{s.current, s.next} {
		for roster, users := range m {
			for user, held := range users {
				if (held.deleted && now.Sub(held.seen) > TTL) || (!held.deleted && held.entry.ExpiresAt <= nowMilli) {
					delete(users, user)
				}
			}
			if len(users) == 0 {
				delete(m, roster)
			}
		}
	}
}

// run keeps one watcher over the bucket alive until ctx ends, restarting it
// (as a resync) when it ends or Resync is called. ready is closed once the
// first watcher's initial values are all in the mirror.
func (s *NATSStore) run(ctx context.Context, ready chan struct{}) {
	defer close(s.stopped)
	prune := time.NewTicker(natsMirrorPruneInterval)
	defer prune.Stop()
	for {
		err := s.watch(ctx, prune.C, ready)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("canvaspresence: bucket watcher ended; resyncing", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(natsWatchRetryDelay):
			}
		}
	}
}

// watch runs one watcher. It returns nil when a resync was requested and an
// error when the watcher could not start or ended on its own.
func (s *NATSStore) watch(ctx context.Context, prune <-chan time.Time, ready chan struct{}) error {
	s.mu.Lock()
	s.next = mirror{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.next = nil
		s.mu.Unlock()
	}()

	watcher, err := s.kv.WatchAll(ctx)
	if err != nil {
		return fmt.Errorf("watch bucket: %w", err)
	}
	defer func() { _ = watcher.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.resync:
			return nil
		case <-prune:
			s.prune()
		case entry, ok := <-watcher.Updates():
			if !ok {
				return errors.New("watcher closed")
			}
			if entry == nil {
				// End of initial values: the fresh mirror is complete.
				s.mu.Lock()
				if s.next != nil {
					s.current, s.next = s.next, nil
				}
				s.mu.Unlock()
				select {
				case <-ready:
				default:
					close(ready)
				}
				continue
			}
			s.applyEntry(entry)
		}
	}
}
