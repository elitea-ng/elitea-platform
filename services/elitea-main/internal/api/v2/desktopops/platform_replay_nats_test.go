package desktopops

// The replay store is what keeps a retried platform MCP write from running
// twice when the retry lands on another replica (issue 1231). These tests run
// against a real JetStream server (ELITEA_TEST_NATS_URL, which ci-go.yml
// provides), each "replica" on its own connection, so they exercise real KV
// Create/Update/Delete races and bucket MaxAge rather than a double.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const natsTestURLEnv = "ELITEA_TEST_NATS_URL"

var replayBucketSeq atomic.Int64

// replayReplica is one elitea-main replica's view of the bucket: its own
// connection and its own store.
type replayReplica struct {
	conn  *nats.Conn
	store *NATSPlatformReplays
}

// newReplayBucket creates a bucket of its own (the nats-bootstrap Job's
// settings, with ttl) and returns n replicas bound to it.
func newReplayBucket(t *testing.T, ttl time.Duration, n int) (string, []replayReplica) {
	t.Helper()
	url := os.Getenv(natsTestURLEnv)
	if url == "" {
		t.Skip("set " + natsTestURLEnv + " (a JetStream-enabled NATS) to run the NATS replay store tests")
	}
	bucket := fmt.Sprintf("TEST_PLATFORM_MCP_REPLAY_%d_%d", time.Now().UnixNano(), replayBucketSeq.Add(1))
	admin, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: bucket, History: 1, TTL: ttl, MaxValueSize: PlatformReplayMaxValue, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = js.DeleteKeyValue(ctx, bucket)
	})
	replicas := make([]replayReplica, n)
	for i := range replicas {
		conn, err := nats.Connect(url, nats.Timeout(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Close)
		rjs, err := jetstream.New(conn)
		if err != nil {
			t.Fatal(err)
		}
		kv, err := rjs.KeyValue(ctx, bucket)
		if err != nil {
			t.Fatal(err)
		}
		// Bound without NewNATSPlatformReplays' TTL check, so a test can use
		// a short TTL; TestNATSPlatformReplaysVerifiesTheBucket covers it.
		replicas[i] = replayReplica{conn: conn, store: newNATSPlatformReplaysOver(kv)}
	}
	return bucket, replicas
}

// slowPlatform blocks every call until release is closed.
type slowPlatform struct {
	fakePlatform
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowPlatform) CallPlatformTool(r *http.Request, call PlatformToolCall) (PlatformToolResult, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.fakePlatform.CallPlatformTool(r, call)
}

func replicaHandler(t *testing.T, platform PlatformToolCaller, store PlatformReplayStore) *remoteToolkitHandler {
	t.Helper()
	h, _, _, _ := platformHandler(platform)
	h.replays = store
	h.replayWait, h.replayPoll = 0, 10*time.Millisecond
	return h
}

// servePlatform is serveWith with the request's own Idempotency-Key.
func servePlatform(t *testing.T, h *remoteToolkitHandler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	user := desktopToken()
	router := chi.NewRouter()
	router.Method(http.MethodPost, RemoteToolkitPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r.WithContext(auth.ContextWithUser(r.Context(), *user)))
	}))
	request := httptest.NewRequest(http.MethodPost, platformURL, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const writeBody = `"tool_name":"post_elitea_core_applications","arguments":{"name":"a"}`

// Claim, answer, replay: a retry on ANOTHER replica answers the first run's
// result byte for byte and never runs the tool.
func TestNATSPlatformReplayAnswersARetryOnAnotherReplica(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 2)
	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	a := replicaHandler(t, platform, replicas[0].store)
	b := replicaHandler(t, platform, replicas[1].store)
	first := servePlatform(t, a, "k-replay", remoteBody(writeBody))
	second := servePlatform(t, b, "k-replay", remoteBody(writeBody))
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("first %d %s, second %d %s", first.Code, first.Body, second.Code, second.Body)
	}
	if len(platform.calls) != 1 {
		t.Fatalf("the tool ran %d times, want once", len(platform.calls))
	}
	// The same key is the caller's own: another project's call with it is a
	// new call (the key hashes caller, project and Idempotency-Key).
	if platformReplayKey(7, 3, "k-replay") == platformReplayKey(7, 4, "k-replay") ||
		platformReplayKey(7, 3, "k-replay") == platformReplayKey(8, 3, "k-replay") {
		t.Fatal("the replay key ignores the caller or the project")
	}
	// A refusal is replayed as well, with its status.
	unknown := &fakePlatform{result: PlatformToolResult{Unknown: true, Available: []string{"a_tool"}}}
	c := replicaHandler(t, unknown, replicas[0].store)
	d := replicaHandler(t, unknown, replicas[1].store)
	one := servePlatform(t, c, "k-refused", remoteBody(writeBody))
	two := servePlatform(t, d, "k-refused", remoteBody(writeBody))
	if one.Code != http.StatusUnprocessableEntity || two.Code != one.Code || one.Body.String() != two.Body.String() || len(unknown.calls) != 1 {
		t.Fatalf("refusal: %d %s / %d %s, calls %d", one.Code, one.Body, two.Code, two.Body, len(unknown.calls))
	}
}

// Many concurrent claims of one key from two replicas: exactly one wins, and
// through the handler exactly one run executes.
func TestNATSPlatformReplayConcurrentClaimsRunOnce(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 2)
	const claimers = 16
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for i := range claimers {
		wg.Go(func() {
			got, err := replicas[i%2].store.Claim(context.Background(), "call.race", "digest")
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if got.Verdict == ReplayClaimed {
				claimed.Add(1)
			} else if got.Verdict != ReplayInProgress {
				t.Errorf("verdict %v", got.Verdict)
			}
		})
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("%d claims won, want exactly 1", claimed.Load())
	}

	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	// One handler per request (the test fakes are not goroutine-safe); the
	// platform and the two replicas' stores are shared.
	responses := make([]*httptest.ResponseRecorder, 8)
	handlers := make([]*remoteToolkitHandler, len(responses))
	for i := range handlers {
		handlers[i] = replicaHandler(t, platform, replicas[i%2].store)
		handlers[i].replayWait = 10 * time.Second
	}
	for i := range responses {
		wg.Go(func() { responses[i] = servePlatform(t, handlers[i], "k-concurrent", remoteBody(writeBody)) })
	}
	wg.Wait()
	for i, response := range responses {
		if response.Code != http.StatusOK || response.Body.String() != responses[0].Body.String() {
			t.Errorf("response %d: %d %s", i, response.Code, response.Body)
		}
	}
	if len(platform.calls) != 1 {
		t.Fatalf("the tool ran %d times across two replicas, want once", len(platform.calls))
	}
}

// A retry that finds its call still running waits (bounded) for the answer
// and replays it; past the bound it answers 409 remote_toolkit_in_progress.
func TestNATSPlatformReplayWaitsForTheRunningAttempt(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 2)
	slow := &slowPlatform{started: make(chan struct{}), release: make(chan struct{})}
	slow.result = PlatformToolResult{Text: `{"id":9}`}
	a := replicaHandler(t, slow, replicas[0].store)
	b := replicaHandler(t, slow, replicas[1].store)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- servePlatform(t, a, "k-wait", remoteBody(writeBody)) }()
	<-slow.started

	b.replayWait = 150 * time.Millisecond
	started := time.Now()
	busy := servePlatform(t, b, "k-wait", remoteBody(writeBody))
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), "remote_toolkit_in_progress") {
		t.Fatalf("while running: %d %s", busy.Code, busy.Body)
	}
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("answered in progress after %v, before the bounded wait", waited)
	}

	b.replayWait = 10 * time.Second
	waiting := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiting <- servePlatform(t, b, "k-wait", remoteBody(writeBody)) }()
	time.Sleep(100 * time.Millisecond)
	close(slow.release)
	first, replayed := <-firstDone, <-waiting
	if first.Code != http.StatusOK || replayed.Code != http.StatusOK || first.Body.String() != replayed.Body.String() {
		t.Fatalf("first %d %s, waiting retry %d %s", first.Code, first.Body, replayed.Code, replayed.Body)
	}
	if len(slow.calls) != 1 {
		t.Fatalf("the tool ran %d times, want once", len(slow.calls))
	}
}

// The same key with another input is refused on any replica, before the tool.
func TestNATSPlatformReplayRefusesAnotherBodyUnderTheKey(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 2)
	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	a := replicaHandler(t, platform, replicas[0].store)
	b := replicaHandler(t, platform, replicas[1].store)
	if first := servePlatform(t, a, "k-mismatch", remoteBody(writeBody)); first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	other := servePlatform(t, b, "k-mismatch",
		remoteBody(`"tool_name":"post_elitea_core_applications","arguments":{"name":"b"}`))
	if other.Code != http.StatusConflict || !strings.Contains(other.Body.String(), "idempotency_conflict") || len(platform.calls) != 1 {
		t.Fatalf("another body: %d %s, calls %d", other.Code, other.Body, len(platform.calls))
	}
}

// KV down: the call is refused 503 (retryable) and the tool never runs. The
// handler fails closed rather than running without the claim.
func TestNATSPlatformReplayDownFailsClosed(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 1)
	replicas[0].conn.Close()
	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	h := replicaHandler(t, platform, replicas[0].store)
	response := servePlatform(t, h, "k-down", remoteBody(writeBody))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" ||
		!strings.Contains(response.Body.String(), "remote_toolkit_unavailable_now") {
		t.Fatalf("KV down: %d %s", response.Code, response.Body)
	}
	if len(platform.calls) != 0 {
		t.Fatalf("the tool ran %d times without the replay store", len(platform.calls))
	}
	// A typed-nil store refuses too.
	var none *NATSPlatformReplays
	if _, err := none.Claim(context.Background(), "call.x", "d"); err == nil {
		t.Fatal("a nil store claimed a key")
	}
}

// The bucket MaxAge ends the replay window: after it, the key is free and
// the same Idempotency-Key runs the call again.
func TestNATSPlatformReplayExpiresWithTheBucketTTL(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Second, 2)
	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	a := replicaHandler(t, platform, replicas[0].store)
	b := replicaHandler(t, platform, replicas[1].store)
	_ = servePlatform(t, a, "k-ttl", remoteBody(writeBody))
	if replay := servePlatform(t, b, "k-ttl", remoteBody(writeBody)); replay.Code != http.StatusOK || len(platform.calls) != 1 {
		t.Fatalf("inside the window: %d, calls %d", replay.Code, len(platform.calls))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		claim, err := replicas[1].store.Claim(context.Background(), platformReplayKey(7, 3, "k-ttl"),
			platformCallDigest("mcp_elitea_internal_applications", "post_elitea_core_applications", argumentsDigest(json.RawMessage(`{"name":"a"}`))))
		if err != nil {
			t.Fatal(err)
		}
		if claim.Verdict == ReplayClaimed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the entry outlived the bucket TTL: %+v", claim)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A released claim (the call never reached the tool) frees the key; an
// answer too large to keep is replayed as "it ran", never run again.
func TestNATSPlatformReplayReleaseAndOmittedAnswer(t *testing.T) {
	_, replicas := newReplayBucket(t, time.Minute, 2)
	failing := &fakePlatform{err: ErrPlatformToolsUnavailable}
	a := replicaHandler(t, failing, replicas[0].store)
	if refused := servePlatform(t, a, "k-release", remoteBody(writeBody)); refused.Code != http.StatusNotImplemented {
		t.Fatalf("unreachable: %d %s", refused.Code, refused.Body)
	}
	failing.err, failing.result = nil, PlatformToolResult{Text: `{}`}
	b := replicaHandler(t, failing, replicas[1].store)
	if again := servePlatform(t, b, "k-release", remoteBody(writeBody)); again.Code != http.StatusOK || len(failing.calls) != 2 {
		t.Fatalf("after a release: %d, calls %d", again.Code, len(failing.calls))
	}

	big := &fakePlatform{result: PlatformToolResult{Text: `"` + strings.Repeat("x", platformReplayMaxBody) + `"`}}
	c := replicaHandler(t, big, replicas[0].store)
	d := replicaHandler(t, big, replicas[1].store)
	first := servePlatform(t, c, "k-big", remoteBody(writeBody))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"ok":true`) {
		t.Fatalf("big first: %d", first.Code)
	}
	replay := servePlatform(t, d, "k-big", remoteBody(writeBody))
	var body map[string]any
	_ = json.Unmarshal(replay.Body.Bytes(), &body)
	if replay.Code != http.StatusOK || body["result_omitted"] != true || body["replayed_status"] != float64(200) ||
		!strings.Contains(fmt.Sprint(body["error"]), "succeeded") || len(big.calls) != 1 {
		t.Fatalf("big replay: %d %v, calls %d", replay.Code, body, len(big.calls))
	}
}

// NewNATSPlatformReplays binds only to a bucket that holds the replay window
// and refuses a missing one with the Job named.
func TestNATSPlatformReplaysVerifiesTheBucket(t *testing.T) {
	short, replicas := newReplayBucket(t, time.Minute, 1)
	js, err := jetstream.New(replicas[0].conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := NewNATSPlatformReplays(ctx, js, short); err == nil || !strings.Contains(err.Error(), "shorter than") {
		t.Fatalf("a 1m bucket bound: %v", err)
	}
	if _, err := NewNATSPlatformReplays(ctx, js, "TEST_PLATFORM_MCP_REPLAY_MISSING"); err == nil || !strings.Contains(err.Error(), "nats-bootstrap") {
		t.Fatalf("a missing bucket: %v", err)
	}
	full, _ := newReplayBucket(t, PlatformReplayTTL, 0)
	store, err := NewNATSPlatformReplays(ctx, js, full)
	if err != nil || store == nil {
		t.Fatalf("the bootstrap's settings: (%v, %v)", store, err)
	}
	if _, err := NewNATSPlatformReplays(ctx, nil, full); err == nil {
		t.Fatal("bound without JetStream")
	}
}
