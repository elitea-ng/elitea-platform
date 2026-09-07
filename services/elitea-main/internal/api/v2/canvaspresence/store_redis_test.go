package canvaspresence_test

// The Redis store is what makes presence work across replicas, and it is where
// the reference's two roster defects were fixed. miniredis is the same in-memory
// server the rest of this service's Redis tests use, so these assertions run on
// real HSET/HDEL/PEXPIRE semantics rather than on a hand-written double.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
)

func newRedisStore(t *testing.T) (*v2canvaspresence.RedisStore, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return v2canvaspresence.NewRedisStore(client), server
}

func TestRedisStoreHoldsARosterAcrossCallers(t *testing.T) {
	store, _ := newRedisStore(t)
	ctx := context.Background()
	key := v2canvaspresence.RosterKey("7", "canvas-uuid")

	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 2 || roster[0].UserID != "1" || roster[1].UserID != "2" {
		t.Fatalf("roster = %#v, want both editors in user-id order", roster)
	}
}

// The reference's leave path removes nobody unless the leaver is the LAST
// editor. This one removes exactly the leaver.
func TestRedisStoreRemovesOneEditorOfSeveral(t *testing.T) {
	store, _ := newRedisStore(t)
	ctx := context.Background()
	key := v2canvaspresence.RosterKey("7", "canvas-uuid")

	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL)
	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL)
	if err := store.Remove(ctx, key, "1"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 1 || roster[0].UserID != "2" {
		t.Fatalf("roster = %#v, want only the editor who stayed", roster)
	}
	if err := store.Remove(ctx, key, "does-not-exist"); err != nil {
		t.Fatalf("removing an absent editor must not be an error: %v", err)
	}
}

// PER-ENTRY expiry, not per-key. The reference expires the whole SET, so one
// live editor's heartbeat keeps a dead tab present. Here the live editor's beat
// refreshes only their own field and the dead tab drops out of the next read.
func TestRedisStoreExpiresEachEntryOnItsOwnDeadline(t *testing.T) {
	store, server := newRedisStore(t)
	ctx := context.Background()
	key := v2canvaspresence.RosterKey("7", "canvas-uuid")

	// miniredis.FastForward advances KEY ttls; the per-entry deadlines are
	// computed against the store's own clock, so both have to move together.
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	// Two editors on the SAME key, one with a short deadline and one with the
	// full TTL. The key's own expiry takes the longer of the two, so the key is
	// still alive when the first entry goes stale — which is precisely the state
	// the reference's whole-key expiry cannot represent.
	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, 2*time.Second)
	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL)
	now = now.Add(3 * time.Second)
	server.FastForward(3 * time.Second)

	if !server.Exists(key) {
		t.Fatal("the key expired; this test must observe a LIVE key holding one stale field")
	}

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 1 || roster[0].UserID != "2" {
		t.Fatalf("roster = %#v, want the stale entry dropped and the fresh one kept", roster)
	}

	// The stale field is gone from storage too, not just filtered out of the
	// answer — otherwise a roster nobody reads grows without bound.
	fields, err := server.HKeys(key)
	if err != nil {
		t.Fatalf("hkeys: %v", err)
	}
	if len(fields) != 1 || fields[0] != "2" {
		t.Fatalf("stored fields = %#v, want only the live editor", fields)
	}
}

func TestRedisStoreListsAnAbsentRosterAsEmpty(t *testing.T) {
	store, _ := newRedisStore(t)
	roster, err := store.List(context.Background(), v2canvaspresence.RosterKey("7", "never-touched"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("roster = %#v, want empty", roster)
	}
}

// A nil client must not produce a store that panics on first use: the option
// that takes it is called unconditionally by router.go.
func TestNewRedisStoreOnANilClientIsNil(t *testing.T) {
	if store := v2canvaspresence.NewRedisStore(nil); store != nil {
		t.Fatalf("NewRedisStore(nil) = %#v, want nil so WithStore leaves the in-process default in place", store)
	}
}

// WithRedis on a nil client leaves the handler on its in-process store, which is
// what lets the route register in a deployment with no Redis.
func TestWithRedisOnANilClientKeepsServingLocally(t *testing.T) {
	handler := v2canvaspresence.NewHandler(twoProjectResolver(), v2canvaspresence.WithRedis(nil))
	recorder := post(t, route(handler), "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200 from the in-process default", recorder.Code)
	}
}

// The PRODUCTION wiring end to end: WithRedis is the single option router.go
// calls, and it has to give the handler BOTH a shared roster and a live
// publisher. A test that only checked the roster would pass over a build that
// published nothing, which is the state the route would ship in if the two arms
// were configurable separately.
func TestWithRedisSharesTheRosterAndPublishes(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	handler := v2canvaspresence.NewHandler(twoProjectResolver(), v2canvaspresence.WithRedis(client))
	router := route(handler)

	subscription := client.Subscribe(context.Background(), "project:7:events")
	t.Cleanup(func() { _ = subscription.Close() })
	if _, err := subscription.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	messages := subscription.Channel()

	post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "1", "ada@example.com")
	second := post(t, router, "/canvas/prompt_lib/7/1/presence", `{"state":"editing"}`, "2", "grace@example.com")

	roster := decodeResponse(t, second).Editors
	if len(roster) != 2 {
		t.Fatalf("roster = %#v, want both editors out of the shared Redis roster", roster)
	}

	// The frame a browser's EventSource would receive, read off the same channel
	// internal/api/v2/events subscribes to.
	deadline := time.After(5 * time.Second)
	seen := 0
	for seen < 2 {
		select {
		case message := <-messages:
			if !strings.Contains(message.Payload, v2canvaspresence.EventType) {
				t.Fatalf("frame %q does not carry the %s event type", message.Payload, v2canvaspresence.EventType)
			}
			seen++
		case <-deadline:
			t.Fatalf("only %d of 2 presence frames reached project 7's channel", seen)
		}
	}
}
