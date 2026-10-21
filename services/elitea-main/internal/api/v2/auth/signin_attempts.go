package auth

// The single-sign-on planes' attempt limiter (#1078).
//
// The OIDC begin and callback and the SAML begin and assertion consumer
// service are unauthenticated: before this, one client could drive unbounded
// state and nonce cookies, identity provider redirects, code exchanges and
// assertion validation. Each now asks the shared PostgreSQL limiter
// (internal/infra/authattempt) first, per client and per stage, with no global
// row — the Form begin's rationale (authattempt.Config.Global): a global
// begin row would let anyone spread over many client keys lock every user out
// of sign-in.
//
// The limiter fails closed: a limited attempt is 429 with Retry-After, and a
// limiter that cannot answer — or a client address that cannot be resolved
// although trusted proxies are configured — is 503. A deployment with no
// trusted proxy CIDRs cannot tell a client from its ingress, so the
// composition root attaches no limiter there and logs a warning; keying on the
// ingress would put every user in one window.

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
)

// SignInClientAddresses resolves the real caller address behind the
// deployment's trusted proxies. scim.ClientAddressResolver implements it; ok
// is false when the address cannot be known.
type SignInClientAddresses interface {
	Resolve(*http.Request) (address string, ok bool)
}

type signInAttempts struct {
	admitter  browserapp.AttemptAdmitter
	addresses SignInClientAddresses
}

func newSignInAttempts(admitter browserapp.AttemptAdmitter, addresses SignInClientAddresses) *signInAttempts {
	if admitter == nil || addresses == nil {
		return nil
	}
	return &signInAttempts{admitter: admitter, addresses: addresses}
}

// admit answers the request itself and reports false when the attempt must
// not proceed. A nil receiver (no limiter composed) admits everything.
func (a *signInAttempts) admit(w http.ResponseWriter, r *http.Request, stage browserapp.BrowserAttemptStage) bool {
	if a == nil {
		return true
	}
	raw, ok := a.addresses.Resolve(r)
	if !ok {
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return false
	}
	address, err := netip.ParseAddr(raw)
	if err != nil {
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return false
	}
	retryAfter, err := a.admitter.Admit(r.Context(), browserapp.BrowserAttempt{
		ClientKey: browserapp.AttemptClientKey(address),
		Stage:     stage,
	})
	if err == nil {
		return true
	}
	if errors.Is(err, browserapp.ErrAttemptLimited) {
		setSignInRetryAfter(w, retryAfter)
		http.Error(w, "too many sign-in attempts", http.StatusTooManyRequests)
		return false
	}
	http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
	return false
}

func setSignInRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	retryAfter = min(retryAfter, browserapp.MaxBrowserAttemptRetryAfter)
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// WithAttemptLimiter puts the OIDC begin and callback behind the shared
// attempt limiter. Either argument nil leaves the plane unlimited; the
// composition root decides that, never a request.
func (h *OIDCHandler) WithAttemptLimiter(admitter browserapp.AttemptAdmitter, addresses SignInClientAddresses) *OIDCHandler {
	if h == nil {
		return h
	}
	h.attempts = newSignInAttempts(admitter, addresses)
	return h
}

// WithAttemptLimiter puts the SAML begin and assertion consumer service behind
// the shared attempt limiter. See OIDCHandler.WithAttemptLimiter.
func (h *SAMLHandler) WithAttemptLimiter(admitter browserapp.AttemptAdmitter, addresses SignInClientAddresses) *SAMLHandler {
	if h == nil {
		return h
	}
	h.attempts = newSignInAttempts(admitter, addresses)
	return h
}
