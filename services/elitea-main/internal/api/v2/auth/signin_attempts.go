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
// A limited attempt is 429 with Retry-After, and a limiter that cannot
// answer is 503, as on the Form begin (internal/api/browserauth
// admitAttempt): the limiter's store is the PostgreSQL every sign-in needs
// anyway. A client address that cannot be resolved (a trusted proxy sent no,
// or more than one, X-Forwarded-For line) is NOT refused: one misbehaving
// proxy hop must not take single sign-on down for everyone behind it. The
// attempt is keyed on the socket peer instead, a coarser bucket shared by
// whatever else arrives unresolvable through that hop, and logged at most
// once a minute. A deployment with no trusted proxy CIDRs cannot tell a
// client from its ingress, so the composition root attaches no limiter there
// and logs a warning; keying on the ingress would put every user in one
// window.

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
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
	// lastFallbackLog is the unix-nano time the peer fallback was last
	// logged, so a proxy that never sends a usable header logs once a
	// minute rather than once per sign-in.
	lastFallbackLog atomic.Int64
}

// signInFallbackLogEvery spaces the peer-fallback warnings.
const signInFallbackLogEvery = time.Minute

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
	address, ok := a.clientAddress(r)
	if !ok {
		address = a.peerAddress(r, stage)
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

// clientAddress is the real caller behind the trusted proxies, when known.
func (a *signInAttempts) clientAddress(r *http.Request) (netip.Addr, bool) {
	raw, ok := a.addresses.Resolve(r)
	if !ok {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// peerAddress is the fallback key when the caller cannot be resolved: the
// socket peer, or the unspecified address for a peer the server could not
// even parse, so every such attempt still counts against one window.
func (a *signInAttempts) peerAddress(r *http.Request, stage browserapp.BrowserAttemptStage) netip.Addr {
	address := netip.IPv4Unspecified()
	if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		address = peer.Addr().Unmap()
	} else if parsed, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		address = parsed.Unmap()
	}
	now := time.Now().UnixNano()
	last := a.lastFallbackLog.Load()
	if now-last >= int64(signInFallbackLogEvery) && a.lastFallbackLog.CompareAndSwap(last, now) {
		slog.Warn("single sign-on attempt limiter could not resolve the client address; keying on the proxy peer",
			"stage", string(stage), "peer", address.String())
	}
	return address
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
