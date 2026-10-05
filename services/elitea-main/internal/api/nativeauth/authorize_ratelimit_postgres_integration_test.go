package nativeauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

func withAddresses(addresses nativeapi.ClientAddresses) stackOption {
	return func(cfg *nativeapi.Config, _ *domain.Config) { cfg.Addresses = addresses }
}

func authorizeOutcome(t *testing.T, s *stack) string {
	t.Helper()
	recorder := s.do(http.MethodGet, authorizeQuery(nil), nil)
	location := recorder.Header().Get("Location")
	if recorder.Code != http.StatusFound {
		t.Fatalf("authorize = %d, want 302", recorder.Code)
	}
	if strings.HasPrefix(location, nativeapi.ContinuePath) {
		return "continue"
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("location %q: %v", location, err)
	}
	return parsed.Query().Get("error")
}

// With no trusted-proxy CIDRs every caller's address is unknown. Those callers
// must not share one 30-per-minute bucket: one anonymous caller (or a busy
// Monday) would otherwise lock every native sign-in on the replica out. The
// pending ceiling still bounds the stored requests.
func TestAuthorizeUnknownAddressesDoNotShareARateLimit(t *testing.T) {
	s := newStack(t)
	for attempt := 1; attempt <= 40; attempt++ {
		if got := authorizeOutcome(t, s); got != "continue" {
			t.Fatalf("authorization %d from an unknown address = %q, want the consent continue page", attempt, got)
		}
	}
}

// A KNOWN address keeps its per-address limit.
func TestAuthorizeKnownAddressIsStillRateLimited(t *testing.T) {
	s := newStack(t, withAddresses(fixedAddress("198.51.100.9")))
	for attempt := 1; attempt <= 30; attempt++ {
		if got := authorizeOutcome(t, s); got != "continue" {
			t.Fatalf("authorization %d = %q, want continue", attempt, got)
		}
	}
	if got := authorizeOutcome(t, s); got != "temporarily_unavailable" {
		t.Fatalf("31st authorization from one address = %q, want temporarily_unavailable", got)
	}
}

// switchableAddress resolves to a known address while one is set, and to an
// unknown one otherwise.
type switchableAddress struct{ address atomic.Value }

func (a *switchableAddress) Resolve(*http.Request) (string, bool) {
	address, _ := a.address.Load().(string)
	return address, address != ""
}

func pendingRows(t *testing.T, s *stack) int {
	t.Helper()
	var count int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.native_authorizations WHERE status = 'pending'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Unknown-address callers must not have UNLIMITED inserts: before this, a
// flood of anonymous /authorize calls (public client id, public redirect)
// filled the deployment-wide pending ceiling and every real sign-in answered
// temporarily_unavailable for the ten minutes the rows lived. They now share a
// bounded per-replica pool, sized so that it alone cannot fill the ceiling,
// and a caller whose address is known is not refused because of it.
func TestAuthorizeUnknownAddressesShareABoundedPool(t *testing.T) {
	addresses := &switchableAddress{}
	s := newStack(t, withAddresses(addresses))
	for attempt := 1; attempt <= nativeapi.UnknownAuthorizationsPerWindow; attempt++ {
		if got := authorizeOutcome(t, s); got != "continue" {
			t.Fatalf("authorization %d from an unknown address = %q, want continue", attempt, got)
		}
	}
	if got := authorizeOutcome(t, s); got != "temporarily_unavailable" {
		t.Fatalf("authorization %d from unknown addresses = %q, want temporarily_unavailable (unbounded anonymous inserts)",
			nativeapi.UnknownAuthorizationsPerWindow+1, got)
	}
	if rows := pendingRows(t, s); rows != nativeapi.UnknownAuthorizationsPerWindow {
		t.Fatalf("%d pending rows stored, want exactly the unknown pool (%d)", rows, nativeapi.UnknownAuthorizationsPerWindow)
	}

	addresses.address.Store("198.51.100.20")
	if got := authorizeOutcome(t, s); got != "continue" {
		t.Fatalf("a known caller after the anonymous flood = %q, want continue", got)
	}
}

// The ceiling is exact: the store refuses the insert that would pass it,
// rather than a cached count letting every replica overshoot for 5 s.
func TestCreateAuthorizationRespectsTheCeilingExactly(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	request := domain.AuthorizationRequest{
		ClientID: testClientID, RedirectURI: testRedirect, CodeChallenge: testChallenge, State: "s",
		DeviceName: "d", Platform: "ios", ClientVersion: "1.0.0",
	}
	for i := 0; i < 3; i++ {
		if _, _, err := s.store.CreateAuthorizationBounded(ctx, request, 3); err != nil {
			t.Fatalf("insert %d under the ceiling: %v", i+1, err)
		}
	}
	if _, _, err := s.store.CreateAuthorizationBounded(ctx, request, 3); !errors.Is(err, domain.ErrPendingCeiling) {
		t.Fatalf("the insert past the ceiling = %v, want ErrPendingCeiling", err)
	}
	if rows := pendingRows(t, s); rows != 3 {
		t.Fatalf("%d pending rows, want 3", rows)
	}
}
