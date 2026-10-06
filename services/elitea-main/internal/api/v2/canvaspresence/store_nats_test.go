package canvaspresence_test

// The NATS store is what makes presence work across replicas, and it is where
// the reference's two roster defects stay fixed. These tests run against a
// real JetStream server (ELITEA_TEST_NATS_URL, which ci-go.yml provides), so
// they exercise real KV Put/Delete/watch and bucket MaxAge semantics rather
// than a hand-written double.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
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
	t.Cleanup(store.Close)
	return store, js, conn
}

// eventuallyRoster polls List until want accepts the roster: another
// replica's write reaches this replica's mirror through its watcher, so
// cross-replica visibility is near-immediate but not synchronous.
func eventuallyRoster(t *testing.T, store *v2canvaspresence.NATSStore, key string, want func([]v2canvaspresence.Editor) bool) []v2canvaspresence.Editor {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		roster, err := store.List(context.Background(), key)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if want(roster) {
			return roster
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster never converged; last = %#v", roster)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	eventuallyRoster(t, replicaB, key, func(r []v2canvaspresence.Editor) bool {
		return len(r) == 1 && r[0].UserID == "1"
	})

	// A leave on B reaches A's mirror too.
	if err := replicaB.Remove(ctx, key, "1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	eventuallyRoster(t, replicaA, key, func(r []v2canvaspresence.Editor) bool { return len(r) == 0 })
}

// The writing replica reads its own write at once — the heartbeat's response
// is built from List right after Touch/Remove — without waiting for the
// watcher.
func TestNATSStoreReadsItsOwnWrites(t *testing.T) {
	store, _, _ := newNATSStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		key := uniqueRoster(t)
		if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL); err != nil {
			t.Fatalf("touch: %v", err)
		}
		if roster, _ := store.List(ctx, key); len(roster) != 1 {
			t.Fatalf("iteration %d: roster right after Touch = %#v, want the writer's own entry", i, roster)
		}
		if err := store.Remove(ctx, key, "1"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if roster, _ := store.List(ctx, key); len(roster) != 0 {
			t.Fatalf("iteration %d: roster right after Remove = %#v, want empty", i, roster)
		}
	}
}

// A replica started AFTER entries were written sees them: the mirror's
// initial sync reads the bucket's current contents before NewNATSStore
// returns.
func TestNATSStoreInitialSyncSeesExistingEntries(t *testing.T) {
	writer, _, _ := newNATSStore(t)
	key := uniqueRoster(t)
	if err := writer.Touch(context.Background(), key, v2canvaspresence.Editor{UserID: "7", UserName: "lin"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch: %v", err)
	}
	late, _, _ := newNATSStore(t)
	roster, err := late.List(context.Background(), key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(roster) != 1 || roster[0].UserID != "7" {
		t.Fatalf("a replica started later read %#v, want the existing entry", roster)
	}
}

// Resync (what a reconnect triggers) rebuilds the mirror from a fresh watch
// and loses nothing: entries written before and during it are all there
// afterwards, and the mirror keeps following new writes.
func TestNATSStoreResyncKeepsTheMirrorConsistent(t *testing.T) {
	store, _, _ := newNATSStore(t)
	other, _, _ := newNATSStore(t)
	ctx := context.Background()
	key := uniqueRoster(t)

	_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL)
	for i := 0; i < 5; i++ {
		store.Resync()
		_ = other.Touch(ctx, key, v2canvaspresence.Editor{UserID: fmt.Sprintf("o%d", i), UserName: "o"}, v2canvaspresence.TTL)
		// Read-your-writes holds even while a resync is in flight.
		_ = store.Touch(ctx, key, v2canvaspresence.Editor{UserID: fmt.Sprintf("s%d", i), UserName: "s"}, v2canvaspresence.TTL)
		if roster, _ := store.List(ctx, key); !containsUser(roster, fmt.Sprintf("s%d", i)) {
			t.Fatalf("resync %d hid the writer's own entry: %#v", i, roster)
		}
	}
	eventuallyRoster(t, store, key, func(r []v2canvaspresence.Editor) bool { return len(r) == 11 })

	_ = other.Remove(ctx, key, "o0")
	eventuallyRoster(t, store, key, func(r []v2canvaspresence.Editor) bool {
		return len(r) == 10 && !containsUser(r, "o0")
	})
}

// List must not create server-side state: the previous design opened and
// deleted an ordered consumer per List (per heartbeat). Many Lists leave the
// bucket's consumer count where it was.
func TestNATSStoreListCreatesNoConsumers(t *testing.T) {
	store, js, _ := newNATSStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, "KV_"+v2canvaspresence.PresenceBucket)
	if err != nil {
		t.Fatalf("open bucket stream: %v", err)
	}
	consumers := func() int {
		info, err := stream.Info(ctx)
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		return info.State.Consumers
	}
	before := consumers()
	key := uniqueRoster(t)
	for i := 0; i < 50; i++ {
		if _, err := store.List(ctx, key); err != nil {
			t.Fatalf("list: %v", err)
		}
	}
	if after := consumers(); after > before {
		t.Fatalf("50 Lists raised the consumer count from %d to %d", before, after)
	}
}

func containsUser(roster []v2canvaspresence.Editor, userID string) bool {
	for _, editor := range roster {
		if editor.UserID == userID {
			return true
		}
	}
	return false
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

// A real NATS restart under a live store: the connection reconnects, the
// reconnect hook resyncs the mirror, and the mirror both keeps what survived
// and follows writes made after the restart. Needs a nats-server binary
// (ELITEA_TEST_NATS_SERVER_BIN), because the test owns that server's process.
func TestNATSStoreFollowsTheBucketAcrossAServerRestart(t *testing.T) {
	bin := os.Getenv("ELITEA_TEST_NATS_SERVER_BIN")
	if bin == "" {
		t.Skip("set ELITEA_TEST_NATS_SERVER_BIN (a nats-server binary) to run the restart test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	storeDir := t.TempDir()
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "-js", "-a", "127.0.0.1", "-p", strconv.Itoa(port), "-sd", storeDir)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start nats-server: %v", err)
		}
		return cmd
	}
	stopServer := func(cmd *exec.Cmd) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	server := start()
	t.Cleanup(func() { stopServer(server) })

	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	var current atomic.Pointer[v2canvaspresence.NATSStore]
	reconnected := make(chan struct{}, 4)
	dial := func(onReconnect bool) *nats.Conn {
		t.Helper()
		opts := []nats.Option{nats.MaxReconnects(-1), nats.ReconnectWait(50 * time.Millisecond)}
		if onReconnect {
			opts = append(opts, nats.ReconnectHandler(func(*nats.Conn) {
				if s := current.Load(); s != nil {
					s.Resync()
				}
				reconnected <- struct{}{}
			}))
		}
		var conn *nats.Conn
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err = nats.Connect(url, opts...)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("connect: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Cleanup(conn.Close)
		return conn
	}
	open := func(conn *nats.Conn) *v2canvaspresence.NATSStore {
		t.Helper()
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
		t.Cleanup(store.Close)
		return store
	}

	reader := open(dial(true))
	current.Store(reader)
	writer := open(dial(false))
	ctx := context.Background()
	key := uniqueRoster(t)

	_ = writer.Touch(ctx, key, v2canvaspresence.Editor{UserID: "before", UserName: "b"}, v2canvaspresence.TTL)
	eventuallyRoster(t, reader, key, func(r []v2canvaspresence.Editor) bool { return containsUser(r, "before") })

	stopServer(server)
	server = start()
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the reader never reconnected after the restart")
	}

	// The writer's own connection reconnects too; retry its write until it
	// lands, then the reader's mirror must show it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := writer.Touch(ctx, key, v2canvaspresence.Editor{UserID: "after", UserName: "a"}, v2canvaspresence.TTL); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the writer could not write after the restart")
		}
		time.Sleep(100 * time.Millisecond)
	}
	eventuallyRoster(t, reader, key, func(r []v2canvaspresence.Editor) bool {
		return containsUser(r, "before") && containsUser(r, "after")
	})
}

// A bucket that disappears from the server (deleted, or a node replaced
// without its storage) is recreated by the store: a watcher only reconnects
// to a stream that still exists, so without this every beat would fail until
// elitea-main restarted. A bucket of its own: deleting the shared one would
// disturb other packages' tests on the same server.
func TestNATSStoreRecreatesABucketThatDisappeared(t *testing.T) {
	conn := natsTestConn(t)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	bucket := fmt.Sprintf("ELITEA_CANVAS_PRESENCE_TEST_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := v2canvaspresence.NewNATSStore(ctx, js, v2canvaspresence.NATSStoreConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("NewNATSStore: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		_ = js.DeleteKeyValue(context.Background(), bucket)
	})
	key := uniqueRoster(t)
	if err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "1", UserName: "ada"}, v2canvaspresence.TTL); err != nil {
		t.Fatalf("touch before delete: %v", err)
	}

	if err := js.DeleteKeyValue(ctx, bucket); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}

	// Beats fail until the run loop has recreated the bucket, then land and
	// show up in the roster again.
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := store.Touch(ctx, key, v2canvaspresence.Editor{UserID: "2", UserName: "grace"}, v2canvaspresence.TTL)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("touch never succeeded after the bucket was deleted: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := js.KeyValue(ctx, bucket); err != nil {
		t.Fatalf("bucket not recreated: %v", err)
	}
	eventuallyRoster(t, store, key, func(r []v2canvaspresence.Editor) bool { return containsUser(r, "2") })
}
