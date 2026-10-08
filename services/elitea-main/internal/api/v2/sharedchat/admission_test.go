package sharedchat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

const unlockPath = "/api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/"

// unknownToken is well formed (43 base64url characters) and issued by nobody.
const unknownToken = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

// countingStore counts the lookups that reach the store.
type countingStore struct {
	*fakeStore
	resolves atomic.Int64
}

func (c *countingStore) ResolveByTokenHash(ctx context.Context, h []byte) (sharedchat.Resolved, error) {
	c.resolves.Add(1)
	return c.fakeStore.ResolveByTokenHash(ctx, h)
}

// headerResolver names the client from a test header, standing in for the
// deployment's trusted-proxy resolver.
type headerResolver struct{}

func (headerResolver) Resolve(r *http.Request) (string, bool) {
	v := r.Header.Get("X-Test-Client")
	return v, v != ""
}

// admissionFixture is a handler over a counting store with one
// password-protected link, plus the router that serves it.
type admissionFixture struct {
	store  *countingStore
	router chi.Router
	token  string
}

func newAdmissionFixture(t *testing.T, opts ...func(*sharedchat.Handler) *sharedchat.Handler) admissionFixture {
	t.Helper()
	store := &countingStore{fakeStore: newFakeStore()}
	h := sharedchat.NewHandler(store, &fakeTranscript{}, []byte("secret"))
	// The link is created with the real KDF before any test hook is set.
	router := newRouter(h)
	token, _ := createLink(t, router, `{"password":"correct horse"}`)
	for _, opt := range opts {
		h = opt(h)
	}
	store.resolves.Store(0)
	return admissionFixture{store: store, router: router, token: token}
}

func postUnlock(router http.Handler, token, password, peer string, header http.Header) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, unlockPath+token+"/unlock", strings.NewReader(string(body)))
	req.RemoteAddr = peer
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// stubVerifier counts calls and answers false.
type stubVerifier struct{ calls atomic.Int64 }

func (s *stubVerifier) verify(string, []byte, []byte) bool { s.calls.Add(1); return false }

// TestUnlockRefusesTheRequestAfterTheVerificationCap proves limit and limit+1:
// with every slot taken by a blocked verification, the next request is
// answered 429 at once, before the store and before any key derivation.
func TestUnlockRefusesTheRequestAfterTheVerificationCap(t *testing.T) {
	const slots = 2
	entered := make(chan struct{}, slots)
	release := make(chan struct{})
	var calls atomic.Int64
	fx := newAdmissionFixture(t, func(h *sharedchat.Handler) *sharedchat.Handler {
		return h.WithVerifySlots(slots).WithPasswordVerifier(func(string, []byte, []byte) bool {
			calls.Add(1)
			entered <- struct{}{}
			<-release
			return false
		})
	})

	var wg sync.WaitGroup
	codes := make(chan int, slots)
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", nil).Code
		}()
	}
	for i := 0; i < slots; i++ {
		<-entered // both slots are now held by a verification that cannot finish
	}
	resolvesBefore := fx.store.resolves.Load()

	// Returns while the other two are still blocked, so it was not queued
	// behind them.
	rec := postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limit+1 status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 carries no Retry-After")
	}
	if got := calls.Load(); got != slots {
		t.Fatalf("verifier ran %d times, want %d: the refused request derived a key", got, slots)
	}
	if fx.store.resolves.Load() != resolvesBefore {
		t.Fatal("the refused request reached the store")
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusForbidden {
			t.Fatalf("in-flight request status = %d, want 403", code)
		}
	}
	// A freed slot admits again.
	if rec := postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("after release status = %d, want 403", rec.Code)
	}
}

func TestUnlockAppliesAPerClientAttemptBudget(t *testing.T) {
	const budget = 3
	v := &stubVerifier{}
	fx := newAdmissionFixture(t, func(h *sharedchat.Handler) *sharedchat.Handler {
		return h.WithPasswordVerifier(v.verify).WithAttemptBudget(budget, time.Hour, 100).
			WithClientAddresses(headerResolver{})
	})
	as := func(client string) http.Header { return http.Header{"X-Test-Client": {client}} }

	for i := 0; i < budget; i++ {
		if rec := postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", as("alice")); rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d status = %d, want 403", i+1, rec.Code)
		}
	}
	resolves := fx.store.resolves.Load()
	rec := postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", as("alice"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("budget+1 status = %d, retry-after = %q; want 429 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	if v.calls.Load() != budget || fx.store.resolves.Load() != resolves {
		t.Fatalf("over-budget request reached the verifier (%d calls) or the store", v.calls.Load())
	}
	if rec := postUnlock(fx.router, fx.token, "x", "192.0.2.1:1", as("bob")); rec.Code != http.StatusForbidden {
		t.Fatalf("a different client status = %d, want 403", rec.Code)
	}
}

// TestUnlockBudgetKeyIgnoresForwardedFor: with no resolver wired the key is
// the socket peer; a caller-supplied X-Forwarded-For does not select it.
func TestUnlockBudgetKeyIgnoresForwardedFor(t *testing.T) {
	v := &stubVerifier{}
	fx := newAdmissionFixture(t, func(h *sharedchat.Handler) *sharedchat.Handler {
		return h.WithPasswordVerifier(v.verify).WithAttemptBudget(1, time.Hour, 100)
	})
	first := postUnlock(fx.router, fx.token, "x", "198.51.100.7:1", http.Header{"X-Forwarded-For": {"203.0.113.1"}})
	second := postUnlock(fx.router, fx.token, "x", "198.51.100.7:2", http.Header{"X-Forwarded-For": {"203.0.113.2"}})
	if first.Code != http.StatusForbidden || second.Code != http.StatusTooManyRequests {
		t.Fatalf("statuses = %d, %d; want 403 then 429 for one peer", first.Code, second.Code)
	}
	if other := postUnlock(fx.router, fx.token, "x", "198.51.100.8:1", nil); other.Code != http.StatusForbidden {
		t.Fatalf("another peer status = %d, want 403", other.Code)
	}
}

// TestUnlockRefusesATokenOfTheWrongShapeWithoutDerivingAKey: the shape of a
// token is public, so skipping the KDF for it reveals nothing, and the answer
// stays the one a well-formed unknown token gets.
func TestUnlockRefusesATokenOfTheWrongShapeWithoutDerivingAKey(t *testing.T) {
	v := &stubVerifier{}
	fx := newAdmissionFixture(t, func(h *sharedchat.Handler) *sharedchat.Handler {
		return h.WithPasswordVerifier(v.verify).WithAttemptBudget(1, time.Hour, 100)
	})
	known := postUnlock(fx.router, unknownToken, "x", "192.0.2.50:1", nil)
	if known.Code != http.StatusForbidden || v.calls.Load() != 1 {
		t.Fatalf("well-formed unknown token: status = %d, verifier calls = %d; want 403 and 1 (timing equalised)",
			known.Code, v.calls.Load())
	}
	resolves := fx.store.resolves.Load()
	for _, token := range []string{"has.dot", "has%20space", strings.Repeat("A", 129)} {
		rec := postUnlock(fx.router, token, "x", "192.0.2.51:1", nil)
		if rec.Code != http.StatusForbidden || rec.Body.String() != known.Body.String() {
			t.Fatalf("token %q: status = %d, body = %q; want the unknown-token answer %q",
				token, rec.Code, rec.Body.String(), known.Body.String())
		}
	}
	if v.calls.Load() != 1 {
		t.Fatalf("verifier calls = %d, want 1: a wrong-shape token derived a key", v.calls.Load())
	}
	if fx.store.resolves.Load() != resolves {
		t.Fatal("a wrong-shape token reached the store")
	}
	// Wrong-shape requests cost nothing, so they do not spend the budget.
	if rec := postUnlock(fx.router, fx.token, "x", "192.0.2.51:1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("well-formed request after wrong-shape ones: status = %d, want 403", rec.Code)
	}
}

// Latency budget for a cheap request served while an unlock burst is running.
// The burst can occupy only the verification cap, so the other cores keep
// serving; measured values are in the Worker source-mapping note.
const burstProbeP99Budget = 250 * time.Millisecond

// raceDetectorOn reports whether the test binary was built with -race, which
// slows the KDF several-fold and makes wall-clock assertions meaningless.
func raceDetectorOn() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value == "true"
		}
	}
	return false
}

// TestUnlockBurstStaysWithinTheVerificationCap fires a burst of parallel
// unlock requests, each from its own client so only the process-wide cap
// applies, at the real PBKDF2 verifier, while a cheap probe route on the same
// server is timed.
func TestUnlockBurstStaysWithinTheVerificationCap(t *testing.T) {
	if testing.Short() {
		t.Skip("burst test runs the real KDF")
	}
	const goroutines, rounds = 200, 5
	const burst = goroutines * rounds
	slots := min(2, max(1, runtime.GOMAXPROCS(0)/2))

	var inFlight, maxInFlight, executions atomic.Int64
	real := sharedchat.VerifyPassword
	fx := newAdmissionFixture(t, func(h *sharedchat.Handler) *sharedchat.Handler {
		return h.WithVerifySlots(slots).WithClientAddresses(headerResolver{}).
			WithPasswordVerifier(func(p string, hash, salt []byte) bool {
				n := inFlight.Add(1)
				for {
					m := maxInFlight.Load()
					if n <= m || maxInFlight.CompareAndSwap(m, n) {
						break
					}
				}
				executions.Add(1)
				defer inFlight.Add(-1)
				return real(p, hash, salt)
			})
	})
	mux := chi.NewRouter()
	mux.Mount("/", fx.router)
	mux.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

	var latencies []time.Duration
	stop := make(chan struct{})
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			resp, err := client.Get(srv.URL + "/probe")
			if err == nil {
				resp.Body.Close()
				latencies = append(latencies, time.Since(start))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var refused, answered atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				// A distinct client per request, so only the process-wide
				// cap (not the per-client budget) is under test.
				id := itoa(int64(g*rounds + round))
				body := `{"password":"wrong horse"}`
				req, _ := http.NewRequest(http.MethodPost, srv.URL+unlockPath+fx.token+"/unlock", strings.NewReader(body))
				req.Header.Set("X-Test-Client", "client-"+id)
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("request %s: %v", id, err)
					return
				}
				resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusTooManyRequests:
					refused.Add(1)
				case http.StatusForbidden:
					answered.Add(1)
				default:
					t.Errorf("request %s: status %d", id, resp.StatusCode)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	<-probeDone

	if got := maxInFlight.Load(); got > int64(slots) {
		t.Fatalf("max concurrent verifications = %d, cap %d", got, slots)
	}
	if refused.Load() == 0 {
		t.Fatal("no request was refused: the burst was never bounded")
	}
	if refused.Load()+answered.Load() != burst || answered.Load() != executions.Load() {
		t.Fatalf("refused %d + answered %d != %d, or answered != KDF executions (%d)",
			refused.Load(), answered.Load(), burst, executions.Load())
	}
	if len(latencies) < 10 {
		t.Fatalf("only %d probes completed during the burst", len(latencies))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99 := latencies[len(latencies)*99/100]
	t.Logf("burst=%d slots=%d gomaxprocs=%d verified=%d refused=%d max_in_flight=%d probes=%d probe_p50=%v probe_p99=%v probe_max=%v",
		burst, slots, runtime.GOMAXPROCS(0), answered.Load(), refused.Load(), maxInFlight.Load(),
		len(latencies), latencies[len(latencies)/2], p99, latencies[len(latencies)-1])
	if raceDetectorOn() {
		t.Log("latency assertion skipped: the race detector slows the KDF and the scheduler")
	} else if runtime.GOMAXPROCS(0) >= 2 && p99 > burstProbeP99Budget {
		t.Fatalf("probe p99 = %v during the burst, budget %v", p99, burstProbeP99Budget)
	}
}
