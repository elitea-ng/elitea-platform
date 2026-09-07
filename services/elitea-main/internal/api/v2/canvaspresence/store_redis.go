package canvaspresence

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/redis"
)

// RedisStore is the cross-replica Store. One Redis HASH per canvas: field =
// user id, value = the editor plus its own deadline.
//
// WHY A HASH WITH PER-FIELD DEADLINES rather than the reference's design. The
// reference keeps a Redis SET of participant ids and expires the whole KEY after
// 120s. Two consequences it lives with, both avoided here:
//
//   - A leaver is only removed when they are the LAST editor
//     (canvas_leave_room's `len(current_editors) == 1` branch). With two people
//     editing, one closing the tab stays in the roster until the whole set
//     expires.
//   - Because the key expires wholesale, one live editor's heartbeat refreshes
//     EVERYBODY, including the tab that died ten minutes ago.
//
// A hash field carries its own deadline, so List can drop exactly the entries
// that are stale. The KEY still gets a TTL — a canvas nobody has touched for a
// TTL leaves no row behind.
type RedisStore struct {
	client *goredis.Client
	now    func() time.Time
}

// NewRedisStore returns a Store backed by client. A nil client returns nil, so
// the caller can pass an absent Redis straight through to WithStore, which
// ignores a nil Store and leaves the in-process default in place.
func NewRedisStore(client *goredis.Client) *RedisStore {
	if client == nil {
		return nil
	}
	return &RedisStore{client: client, now: time.Now}
}

// storedEntry is the hash field's value.
type storedEntry struct {
	Editor    Editor `json:"editor"`
	ExpiresAt int64  `json:"expires_at_unix_milli"`
}

// SetClock replaces time.Now. Tests use it to walk entries past their deadline
// alongside miniredis's own FastForward, which advances KEY ttls but not the
// process clock these per-entry deadlines are computed against.
func (s *RedisStore) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *RedisStore) Touch(ctx context.Context, key string, editor Editor, ttl time.Duration) error {
	value, err := json.Marshal(storedEntry{Editor: editor, ExpiresAt: s.now().Add(ttl).UnixMilli()})
	if err != nil {
		return fmt.Errorf("canvaspresence: marshal entry: %w", err)
	}
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, key, editor.UserID, value)
	// The KEY's own expiry is refreshed on every beat and is always at least as
	// long as the longest field deadline, so it can never evict a live editor.
	pipe.PExpire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("canvaspresence: touch: %w", err)
	}
	return nil
}

func (s *RedisStore) Remove(ctx context.Context, key string, userID string) error {
	if err := s.client.HDel(ctx, key, userID).Err(); err != nil && err != goredis.Nil {
		return fmt.Errorf("canvaspresence: remove: %w", err)
	}
	return nil
}

func (s *RedisStore) List(ctx context.Context, key string) ([]Editor, error) {
	fields, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		if err == goredis.Nil {
			return []Editor{}, nil
		}
		return nil, fmt.Errorf("canvaspresence: list: %w", err)
	}
	now := s.now().UnixMilli()
	live := make([]Editor, 0, len(fields))
	stale := make([]string, 0, len(fields))
	for userID, raw := range fields {
		var held storedEntry
		if err := json.Unmarshal([]byte(raw), &held); err != nil {
			// An unreadable field is stale by definition: it cannot be shown and
			// it must not be kept forever.
			stale = append(stale, userID)
			continue
		}
		if held.ExpiresAt <= now {
			stale = append(stale, userID)
			continue
		}
		live = append(live, held.Editor)
	}
	if len(stale) > 0 {
		// Best effort. A failed cleanup costs one more pass, never a wrong
		// roster: the deadline check above is what decides what is returned.
		_ = s.client.HDel(ctx, key, stale...).Err()
	}
	return sortRoster(live), nil
}

// WithRedis is the PRODUCTION wiring in one option: it makes the roster
// cross-replica and turns the publish on.
//
// It takes the client rather than a Store and an Emitter because the two must
// not be able to drift apart. A roster shared across replicas whose event is
// published nowhere is a heartbeat only the beating tab can see; an event
// published from a per-replica roster tells the other tabs a roster that is
// missing half its editors. #152 is this repository's record of what happens
// when the two arms of one surface are allowed to be configured separately.
//
// A NIL CLIENT IS A NO-OP, deliberately. The route keeps serving on the
// in-process store, so a deployment without Redis still answers its own tab
// correctly (see NewHandler). This is what lets router.go call it
// unconditionally instead of branching on cfg.RedisClient — a nil-comparison
// there is read by TestNilGatedRouterFieldsAreWiredOrDeclared as a gate that
// decides REGISTRATION, which this is not: the route registers either way.
//
// THE TRANSPORT MATCHES THE SSE STREAM'S. The project stream this publishes
// onto is mounted from cfg.RedisClient in every shipped deployment — the reason
// is written out in cmd/elitea-main/event_stream_redis.go: every elitea-main
// deployment here passes REDIS_URL and none is given a NATS endpoint. When
// elitea-main's own bus moves to NATS, this option and that mount move
// together.
func WithRedis(client *goredis.Client) Option {
	return func(h *Handler) {
		if client == nil {
			return
		}
		h.store = NewRedisStore(client)
		h.emitter = events.NewPublisher(redis.NewEventBus(client, eventSource))
	}
}

// eventSource is the `source` field stamped on every event this package
// publishes (redis.Event.Source).
const eventSource = "elitea-main"
