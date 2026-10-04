package nativeauth_test

import (
	"net/http"
	"net/url"
	"strings"
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
