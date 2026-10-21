package auth

// The single-sign-on attempt limiter (signin_attempts.go, #1078).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
)

// countingAdmitter admits `limit` attempts per (stage, client) and then
// answers ErrAttemptLimited, or always answers err when it is set.
type countingAdmitter struct {
	mu       sync.Mutex
	limit    int
	err      error
	retry    time.Duration
	attempts []browserapp.BrowserAttempt
	counts   map[browserapp.BrowserAttempt]int
}

func (a *countingAdmitter) Admit(_ context.Context, attempt browserapp.BrowserAttempt) (time.Duration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = append(a.attempts, attempt)
	if a.err != nil {
		return 0, a.err
	}
	if a.counts == nil {
		a.counts = map[browserapp.BrowserAttempt]int{}
	}
	if a.counts[attempt] >= a.limit {
		return a.retry, browserapp.ErrAttemptLimited
	}
	a.counts[attempt]++
	return 0, nil
}

// peerAddresses resolves the socket peer, or nothing when unresolvable is set.
type peerAddresses struct{ unresolvable bool }

func (p peerAddresses) Resolve(r *http.Request) (string, bool) {
	if p.unresolvable {
		return "", false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return host, err == nil
}

func signInRequest(method, target, peer string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = peer
	return request
}

func TestOIDCBeginIsRefusedWith429AndRetryAfterOnceTheClientIsLimited(t *testing.T) {
	admitter := &countingAdmitter{limit: 3, retry: 29200 * time.Millisecond}
	handler := envOnlyOIDCHandler().WithAttemptLimiter(admitter, peerAddresses{})

	for index := 0; index < 3; index++ {
		recorder := httptest.NewRecorder()
		handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "192.0.2.7:4000"))
		if recorder.Code != http.StatusFound {
			t.Fatalf("begin %d = %d, want 302", index, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "192.0.2.7:4001"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth begin = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("a refused begin set %d cookies", len(cookies))
	}
	// Another client is not affected.
	recorder = httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "192.0.2.8:4000"))
	if recorder.Code != http.StatusFound {
		t.Fatalf("another client's begin = %d, want 302", recorder.Code)
	}
	for _, attempt := range admitter.attempts {
		if attempt.Stage != browserapp.BrowserAttemptOIDCBegin {
			t.Fatalf("begin admitted as stage %q", attempt.Stage)
		}
	}
}

func TestOIDCFailsClosedWith503WhenTheLimiterErrors(t *testing.T) {
	admitter := &countingAdmitter{err: errors.New("database unavailable")}
	handler := envOnlyOIDCHandler().WithAttemptLimiter(admitter, peerAddresses{})

	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "192.0.2.7:4000"))
	if recorder.Code != http.StatusServiceUnavailable || len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("begin = %d with %d cookies, want 503 and none", recorder.Code, len(recorder.Result().Cookies()))
	}
	recorder = httptest.NewRecorder()
	handler.Callback(recorder, signInRequest(http.MethodGet, "/auth/oidc/callback?code=x&state=y", "192.0.2.7:4000"))
	// Admitted, this callback would be 400 (no state cookie).
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("callback = %d, want 503", recorder.Code)
	}
	if len(admitter.attempts) != 2 ||
		admitter.attempts[0].Stage != browserapp.BrowserAttemptOIDCBegin ||
		admitter.attempts[1].Stage != browserapp.BrowserAttemptOIDCCallback {
		t.Fatalf("attempts = %+v", admitter.attempts)
	}
}

// A trusted proxy that sends no usable X-Forwarded-For used to make every
// sign-in 503. The attempt now proceeds, keyed on the socket peer: a coarser
// bucket, still limited.
func TestAnUnresolvableClientIsKeyedOnThePeerInsteadOfRefused(t *testing.T) {
	admitter := &countingAdmitter{limit: 2, retry: time.Second}
	handler := envOnlyOIDCHandler().WithAttemptLimiter(admitter, peerAddresses{unresolvable: true})

	for index := 0; index < 2; index++ {
		recorder := httptest.NewRecorder()
		handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "10.0.0.5:4000"))
		if recorder.Code != http.StatusFound {
			t.Fatalf("begin %d = %d, want 302", index, recorder.Code)
		}
	}
	if len(admitter.attempts) != 2 || admitter.attempts[0].ClientKey != "10.0.0.5" {
		t.Fatalf("attempts = %+v, want two keyed on the peer", admitter.attempts)
	}
	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "10.0.0.5:4001"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("third begin through the same peer = %d, want 429", recorder.Code)
	}

	// A peer the server could not parse still counts, in one shared window.
	recorder = httptest.NewRecorder()
	handler.Callback(recorder, signInRequest(http.MethodGet, "/auth/oidc/callback?code=x&state=y", "not-an-address"))
	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("callback with an unparsable peer = 503, want it admitted")
	}
	if got := admitter.attempts[len(admitter.attempts)-1].ClientKey; got != "0.0.0.0" {
		t.Fatalf("unparsable peer keyed as %q, want 0.0.0.0", got)
	}
}

// One IPv6 subscriber holds a whole /64: two addresses in it are one client.
func TestAnIPv6ClientIsKeyedByItsSlash64(t *testing.T) {
	admitter := &countingAdmitter{limit: 1, retry: time.Second}
	handler := envOnlyOIDCHandler().WithAttemptLimiter(admitter, peerAddresses{})

	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "[2001:db8:1:2::1]:4000"))
	if recorder.Code != http.StatusFound {
		t.Fatalf("first begin = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "[2001:db8:1:2:ffff::9]:4000"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second begin in the same /64 = %d, want 429", recorder.Code)
	}
	if got := admitter.attempts[1].ClientKey; got != "2001:db8:1:2::/64" {
		t.Fatalf("client key = %q", got)
	}
}

func TestSAMLBeginAndACSAreLimitedPerStage(t *testing.T) {
	admitter := &countingAdmitter{limit: 0, retry: 5 * time.Second}
	handler := NewSAMLHandler(nil, "test-secret", stubProviderSource{}, nil, true).
		WithAttemptLimiter(admitter, peerAddresses{})

	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/saml/login", "192.0.2.7:4000"))
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("SAML begin = %d Retry-After %q, want 429 and 5", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	recorder = httptest.NewRecorder()
	handler.ACS(recorder, signInRequest(http.MethodPost, "/auth/saml/acs", "192.0.2.7:4000"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("SAML ACS = %d, want 429", recorder.Code)
	}
	if len(admitter.attempts) != 2 ||
		admitter.attempts[0].Stage != browserapp.BrowserAttemptSAMLBegin ||
		admitter.attempts[1].Stage != browserapp.BrowserAttemptSAMLACS {
		t.Fatalf("attempts = %+v", admitter.attempts)
	}

	admitter.err = errors.New("database unavailable")
	recorder = httptest.NewRecorder()
	handler.ACS(recorder, signInRequest(http.MethodPost, "/auth/saml/acs", "192.0.2.7:4000"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("SAML ACS on a limiter error = %d, want 503", recorder.Code)
	}
}

// A plane composed without a limiter behaves as before.
func TestNoLimiterLeavesTheBeginUnchanged(t *testing.T) {
	handler := envOnlyOIDCHandler().WithAttemptLimiter(nil, peerAddresses{})
	recorder := httptest.NewRecorder()
	handler.Login(recorder, signInRequest(http.MethodGet, "/auth/oidc/login", "192.0.2.7:4000"))
	if recorder.Code != http.StatusFound {
		t.Fatalf("begin = %d, want 302", recorder.Code)
	}
}
