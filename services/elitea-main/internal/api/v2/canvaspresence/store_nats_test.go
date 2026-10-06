package canvaspresence_test

// The NATS store is what makes presence work across replicas, and it is where
// the reference's two roster defects stay fixed. These tests run against a
// real JetStream server (ELITEA_TEST_NATS_URL, which ci-go.yml provides), so
// they exercise real KV Put/Delete/watch and bucket MaxAge semantics rather
// than a hand-written double.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	v2canvaspresence "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/canvaspresence"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/natsbus"
)

const natsTestURLEnv = "ELITEA_TEST_NATS_URL"

func natsTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	url := os.Getenv(natsTestURLEnv)
	if url == "" {
		t.Skip("set " + natsTestURLEnv + " (a JetStream-enabled NATS) to run the NATS presence store tests")
	}
	conn, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func newNATSStore(t *testing.T) (*v2canvaspresence.NATSStore, jetstream.JetStream, *nats.Conn) {
	t.Helper()
	conn := natsTestConn(t)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := v2canvaspresence.NewNATSStore(ctx, js, v2canvaspresence.NATSStoreConfig{})
	if err != nil {
		t.Fatalf("NewNATSStore: %v", err)
	}
	return store, js, conn
}

var rosterSeq atomic.Int64

// uniqueRoster keeps tests (and repeated runs against one long-lived server)
// from reading each other's entries in the shared bucket.
func uniqueRoster(t *testing.T) string {
	t.Helper()
	return v2canvaspresence.RosterKey(
		fmt.Sprintf("%d", time.Now().UnixNano()),
		fmt.Sprintf("%s-%d", t.Name(), rosterSeq.Add(1)),
	)
}

func TestNATSStoreHoldsARosterAcrossCallers(t *testing.T) {
	store, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)

	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// A second beat from the same editor replaces, it does not duplicate.
	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace", State: v2canvaspresence.StateViewing}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 2 || roster[0].UserID != "1" || roster[1].UserID != "2" {
		t.Fatalf("roster = %#v, want both editors once each, in user-id order", roster)
	}
	if roster[1].State != v2canvaspresence.StateViewing {
		t.Fatalf("roster[1].State = %q, want the latest beat's state", roster[1].State)
	}
}

// Two stores over one bucket are two replicas: what one writes, the other
// reads. This is the property the in-process store cannot give.
func TestNATSStoreIsSharedAcrossReplicas(t *testing.T) {
	replicaA, _, _ := newNATSStore(t)
	replicaB, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)

	if err := replicaA.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}
	roster, err := replicaB.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 1 || roster[0].UserID != "1" {
		t.Fatalf("replica B roster = %#v, want the editor replica A recorded", roster)
	}
}

// The reference's leave path removes nobody unless the leaver is the LAST
// editor. This one removes exactly the leaver.
func TestNATSStoreRemovesOneEditorOfSeveral(t *testing.T) {
	store, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)

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

// PER-ENTRY expiry, not per-roster. The reference expires the whole SET, so
// one live editor's heartbeat keeps a dead tab present. Here each entry's own
// deadline decides, while both entries are still stored.
func TestNATSStoreExpiresEachEntryOnItsOwnDeadline(t *testing.T) {
	store, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)

	now := time.Now()
	store.SetClock(func() time.Time { return now })

	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, 2*time.Second)
	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL)
	now = now.Add(3 * time.Second)

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 1 || roster[0].UserID != "2" {
		t.Fatalf("roster = %#v, want the stale entry dropped and the fresh one kept", roster)
	}
}

func TestNATSStoreListsAnAbsentRosterAsEmpty(t *testing.T) {
	store, _, _ := newNATSStore(t)
	roster, err := store.List(context.Background(), uniqueRoster(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if roster == nil || len(roster) != 0 {
		t.Fatalf("roster = %#v, want an empty, non-nil roster", roster)
	}
}

// Identifiers are encoded, never spliced into the key: a user id holding '.',
// '*' or '>' must neither be refused by KV key validation nor widen a roster's
// filter onto another roster.
func TestNATSStoreKeysCannotEscapeTheirRoster(t *testing.T) {
	store, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)
	neighbour := key + ":x" // a roster key that EXTENDS this one

	for _, id := range []string{"a.b", "*", ">", "x:y", "ünïcode"} {
		if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: id, UserName: id}, v2canvaspresence.TTL); err != nil {
			t.Fatalf("touch %q: %v", id, err)
		}
	}
	if err := store.Touch(ctx, neighbour, v2canvaspresence.Editor{UserID: "intruder", UserName: "intruder"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch neighbour: %v", err)
	}

	roster, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 5 {
		t.Fatalf("roster = %#v, want exactly the five editors of this roster", roster)
	}
	for _, editor := range roster {
		if editor.UserID == "intruder" {
			t.Fatalf("roster %q read an entry of roster %q", key, neighbour)
		}
	}
}

// The bucket's MaxAge is the garbage collector that stands in for the Redis
// key's PEXPIRE: without it an entry nobody refreshes is stored forever. It
// must equal TTL, and History must be 1 so a heartbeat does not pile up
// revisions.
func TestNATSStoreBucketCollectsEntriesAfterTheTTL(t *testing.T) {
	_, js, _ := newNATSStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kv, err := js.KeyValue(ctx, v2canvaspresence.PresenceBucket)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	status, err := kv.Status(ctx)
	if err != nil {
		t.Fatalf("bucket status: %v", err)
	}
	if status.TTL() != v2canvaspresence.TTL {
		t.Fatalf("bucket MaxAge = %v, want TTL (%v)", status.TTL(), v2canvaspresence.TTL)
	}
	if status.History() != 1 {
		t.Fatalf("bucket history = %d, want 1", status.History())
	}
}

// The production wiring end to end over a real server: two handlers (two
// replicas) share the roster through the NATS store, and every beat reaches a
// subscriber of the project's subject on the live-update bus — the subject the
// SSE route reads.
func TestNATSBackendSharesTheRosterAndPublishes(t *testing.T) {
	store, _, conn := newNATSStore(t)
	bus := natsbus.NewFromConn(conn, "test")

	projectID := fmt.Sprintf("%d", 900000+rosterSeq.Add(1))
	events, cancel, err := bus.Raw(context.Background(), "project:"+projectID+":events")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	resolver := &schemaResolver{canvases: map[string]map[string]string{
		projectID: {"1": fmt.Sprintf("canvas-%s-%d", projectID, time.Now().UnixNano())},
	}}
	backend := v2canvaspresence.Backend{Store: store, Bus: bus}
	replicaA := route(v2canvaspresence.NewHandler(resolver, v2canvaspresence.WithBackend(backend)))
	replicaB := route(v2canvaspresence.NewHandler(resolver, v2canvaspresence.WithBackend(backend)))

	path := "/canvas/prompt_lib/" + projectID + "/1/presence"
	post(t, replicaA, path, `{"state":"editing"}`, "1", "ada@example.com")
	second := post(t, replicaB, path, `{"state":"editing"}`, "2", "grace@example.com")
	if roster := decodeResponse(t, second).Editors; len(roster) != 2 {
		t.Fatalf("roster = %#v, want both editors out of the shared NATS roster", roster)
	}

	deadline := time.After(5 * time.Second)
	for seen := 0; seen < 2; {
		select {
		case event := <-events:
			if event.Type != v2canvaspresence.EventType || !strings.Contains(string(event.Payload), `"editors"`) {
				t.Fatalf("frame %+v is not a %s roster", event, v2canvaspresence.EventType)
			}
			seen++
		case <-deadline:
			t.Fatalf("only %d of 2 presence frames reached project %s's subject", seen, projectID)
		}
	}
}
