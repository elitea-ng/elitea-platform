package canvaspresence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// PresenceBucket is the JetStream KV bucket that holds every canvas roster.
const PresenceBucket = "ELITEA_CANVAS_PRESENCE"

// NATSStore is the cross-replica Store: one JetStream KV entry per
// (roster, editor), on the same NATS server as the live-update bus the roster
// is published on.
//
// # Layout
//
// Key = base64url(rosterKey) + "." + base64url(userID). RosterKey contains ':'
// and user ids are not ours to constrain, while KV keys allow only
// [-/_=.a-zA-Z0-9]; unpadded base64url maps any input into [A-Za-z0-9_-], so no
// caller-supplied byte can become a '.' token separator or a '*'/'>' wildcard.
// One roster is then the subject filter `<base64url(rosterKey)>.*`.
//
// # Expiry: the same two layers the Redis HASH store it replaced had
//
//   - The VALUE carries its own deadline (storedEntry.ExpiresAt, from Touch's
//     ttl), and List drops anything past it. This is what decides the answer,
//     so a stale editor is gone from the next roster exactly one TTL after its
//     last beat, per editor — not when the whole roster expires (the
//     reference's design: one Redis SET expired wholesale after 120s).
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
}

// NATSStoreConfig shapes the bucket.
type NATSStoreConfig struct {
	// Replicas is the bucket's replica count. 0 means 1, which is right for
	// the scale-1 NATS profile; an HA (3-node) server may take 3. Presence is
	// ephemeral, so R1 on an HA server only means a node loss empties rosters
	// (they refill on the next beat) — it never loses data that matters.
	Replicas int
}

// NewNATSStore creates the bucket if it is absent (or updates its config) and
// returns a store over it. A JetStream-less server fails here, at startup.
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
	return newNATSStoreOver(kv), nil
}

func newNATSStoreOver(kv jetstream.KeyValue) *NATSStore {
	return &NATSStore{kv: kv, now: time.Now}
}

// SetClock replaces time.Now. Tests use it to walk entries past their
// deadline without waiting out the bucket's MaxAge.
func (s *NATSStore) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
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

func natsEntryKey(rosterKey, userID string) string {
	return natsRosterPrefix(rosterKey) + "." + keyEncoding.EncodeToString([]byte(userID))
}

func (s *NATSStore) Touch(ctx context.Context, key string, editor Editor, ttl time.Duration) error {
	value, err := json.Marshal(storedEntry{Editor: editor, ExpiresAt: s.now().Add(ttl).UnixMilli()})
	if err != nil {
		return fmt.Errorf("canvaspresence: marshal entry: %w", err)
	}
	if _, err := s.kv.Put(ctx, natsEntryKey(key, editor.UserID), value); err != nil {
		return fmt.Errorf("canvaspresence: touch: %w", err)
	}
	return nil
}

// Remove writes a delete marker. Removing an absent editor is not an error:
// KV Delete does not check that the key exists.
func (s *NATSStore) Remove(ctx context.Context, key string, userID string) error {
	if err := s.kv.Delete(ctx, natsEntryKey(key, userID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("canvaspresence: remove: %w", err)
	}
	return nil
}

// List reads the latest value of every entry under the roster with one
// filtered watch, which delivers the current values and then a nil marker.
func (s *NATSStore) List(ctx context.Context, key string) ([]Editor, error) {
	watcher, err := s.kv.WatchFiltered(ctx, []string{natsRosterPrefix(key) + ".*"}, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("canvaspresence: list: %w", err)
	}
	defer func() { _ = watcher.Stop() }()

	now := s.now().UnixMilli()
	live := []Editor{}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("canvaspresence: list: %w", ctx.Err())
		case entry, ok := <-watcher.Updates():
			if !ok {
				return nil, errors.New("canvaspresence: list: watcher closed before the roster was read")
			}
			if entry == nil {
				return sortRoster(live), nil
			}
			var held storedEntry
			if err := json.Unmarshal(entry.Value(), &held); err != nil {
				// Unreadable is stale by definition. MaxAge collects it.
				continue
			}
			if held.ExpiresAt <= now {
				continue
			}
			live = append(live, held.Editor)
		}
	}
}
