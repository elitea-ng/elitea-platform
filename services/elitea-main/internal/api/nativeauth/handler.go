// Package nativeauth serves the native authorization endpoints (ADR-0025 WP2):
//
//	GET  /api/v2/auth/native/authorize            RFC 8252 authorization request
//	GET  /api/v2/auth/native/authorize/continue   resume after the browser sign-in, consent page
//	POST /api/v2/auth/native/authorize/decision   the user's answer -> 302 to the app
//	POST /api/v2/auth/native/token                code and refresh grants
//	POST /api/v2/auth/native/revoke               RFC 7009 revocation (whole family)
//
// and the admin registry editor under /api/v2/admin/native_clients.
//
// The routes are root-mounted siblings of /api/v2 (like the branding
// bootstrap), so the /api/v2 group's JSON 401 never applies to the browser
// pages. Every native route answers 404 while no client is registered in either
// layer (the ADR's "no client, no endpoints"); the admin routes are always
// mounted, because they are how the first client is registered.
//
// Nothing here writes a token, code, handle, binder or hash to a log line or an
// audit row.
package nativeauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

// The route paths.
const (
	BasePath      = "/api/v2/auth/native"
	AuthorizePath = BasePath + "/authorize"
	ContinuePath  = AuthorizePath + "/continue"
	DecisionPath  = AuthorizePath + "/decision"
	TokenPath     = BasePath + "/token"
	RevokePath    = BasePath + "/revoke"
)

// Rate limits (in memory, per replica, like the SCIM token endpoint).
const (
	tokenFailuresPerClientAddress = 20
	tokenFailuresPerAddress       = 60
	authorizePerAddress           = 30
	// UnknownAuthorizationsPerWindow is the shared /authorize pool of every
	// caller whose address cannot be known (no trusted-proxy CIDRs, the Helm
	// default), per replica. Without it those callers had no limit at all,
	// and a flood of anonymous requests filled pendingCeiling and refused
	// every sign-in for AuthorizationTTL. Sized so that the pool alone stores
	// at most a quarter of the ceiling per replica over one TTL
	// (250/min x 10 min = 2,500): a flood now costs the anonymous callers of
	// that replica their sign-ins only while it lasts, and never the callers
	// whose address is known. Configure trusted-proxy CIDRs to get
	// per-address limits instead.
	UnknownAuthorizationsPerWindow = 250
	revokePerAddress               = 60
	limitWindow                    = time.Minute
	// pendingCeiling refuses /authorize while this many unexpired pending
	// requests exist deployment-wide.
	pendingCeiling     = 10_000
	pendingCountMaxAge = 5 * time.Second
	maxFormBytes       = 8 << 10
	// maxLoginBounces is the loop guard: a deployment whose sign-in plane
	// writes a session this service cannot read would otherwise bounce the
	// browser between sign-in and continue forever.
	maxLoginBounces = 3
)

// ClientAddresses resolves the real caller address from the trusted proxy
// CIDRs. ok is false when the address cannot be known.
type ClientAddresses interface {
	Resolve(*http.Request) (address string, ok bool)
}

// TokenResponseDecorator adds fields to every successful token response. WP4
// adds `client_policy` through it. It must not remove or rename a field.
type TokenResponseDecorator func(ctx context.Context, clientID string, body map[string]any) error

// Config composes the handler.
type Config struct {
	Registry *domain.Registry
	Store    *domain.Store
	// PublicOrigin is publicorigin.Normalize(DEPLOYMENT_URL). It is the
	// `iss` of every authorization response (RFC 9207) and the origin the
	// decision POST's Origin header must equal. Empty refuses /authorize with
	// a 503 page: an `iss` derived from a request header is one a client
	// that talks to many deployments cannot trust.
	PublicOrigin string
	Pages        *browserauth.NativePages
	// Authenticate is apimw.Auth over the SAME AuthConfig the /api/v2 group
	// uses. Nil refuses the browser pages with 503.
	Authenticate func(http.Handler) http.Handler
	Addresses    ClientAddresses
	// Audit receives the token endpoint's explicit rows and wraps the
	// decision route. Nil records nothing.
	Audit audit.Recorder
	// SecureCookies selects the `__Host-` binder cookie (COOKIE_SECURE).
	SecureCookies bool
	// Decorate is the WP4 seam; nil adds nothing.
	Decorate TokenResponseDecorator
	// MinimumClientVersion is the effective minimum version for a client id
	// (ADR-0025 WP4, internal/application/nativepolicy). The token endpoint
	// answers 426 to a client whose X-Client-Version is below it, BEFORE a
	// code or refresh token is consumed, so the same credential still works
	// once the app is updated. Nil gates nothing.
	MinimumClientVersion func(ctx context.Context, clientID string) (string, error)
}

// Handler serves the native routes.
type Handler struct {
	cfg Config

	tokenFailures *failurelimit.Limiter
	// tokenAddressFailures stops one address spraying many client ids.
	tokenAddressFailures *failurelimit.Limiter
	authorizations       *failurelimit.Limiter
	// unknownAuthorizations is the one shared bucket of unknown addresses.
	unknownAuthorizations *failurelimit.Limiter
	revocations           *failurelimit.Limiter

	pendingMu      sync.Mutex
	pendingCount   int64
	pendingCounted time.Time
	now            func() time.Time
}

// New builds the handler.
func New(cfg Config) *Handler {
	return &Handler{
		cfg:                   cfg,
		tokenFailures:         failurelimit.New(tokenFailuresPerClientAddress, limitWindow),
		tokenAddressFailures:  failurelimit.New(tokenFailuresPerAddress, limitWindow),
		authorizations:        failurelimit.New(authorizePerAddress, limitWindow),
		unknownAuthorizations: failurelimit.New(UnknownAuthorizationsPerWindow, limitWindow),
		revocations:           failurelimit.New(revokePerAddress, limitWindow),
		now:                   time.Now,
	}
}

// Mount registers the five native routes on the root router.
func (h *Handler) Mount(r chi.Router) {
	r.Get(AuthorizePath, h.registered(h.authorize, false))
	r.Get(ContinuePath, h.registered(h.continueRoute, false))
	r.Post(DecisionPath, h.registered(h.decision, false))
	r.Post(TokenPath, h.registered(h.token, true))
	r.Post(RevokePath, h.registered(h.revoke, true))
}

// registered answers 404 while no client exists in either layer.
//
// oauthEndpoint marks the token and revocation endpoints. RFC 6749 §5.1
// requires both `Cache-Control: no-store` and `Pragma: no-cache` on every
// answer that carries tokens or credentials, and they are written here,
// before any branch runs, because some answers are not written by this
// package (the shared 401 device_revoked and 426 upgrade-required writers set
// Cache-Control only). An iOS URLCache keeps a token response that does not
// forbid it in a plaintext on-disk database.
func (h *Handler) registered(next http.HandlerFunc, oauthEndpoint bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if oauthEndpoint {
			w.Header().Set("Pragma", "no-cache")
		}
		ok, err := h.cfg.Registry.Registered(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "native authorization: client registry unavailable", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "temporarily_unavailable", "error_description": "the client registry could not be read",
			})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		next(w, r)
	}
}

func (h *Handler) address(r *http.Request) (string, bool) {
	if h.cfg.Addresses == nil {
		return "", false
	}
	return h.cfg.Addresses.Resolve(r)
}

func addressKey(address string, known bool) string {
	if !known {
		return "addr:<unknown>"
	}
	return "addr:" + address
}

func clientAddressKey(clientID, address string, known bool) string {
	if len(clientID) > 128 {
		clientID = "<oversized>"
	}
	return "client:" + clientID + "|" + addressKey(address, known)
}

// pending returns the cached count of unexpired pending authorizations.
func (h *Handler) pending(ctx context.Context) (int64, error) {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	if !h.pendingCounted.IsZero() && h.now().Sub(h.pendingCounted) < pendingCountMaxAge {
		return h.pendingCount, nil
	}
	count, err := h.cfg.Store.CountPending(ctx)
	if err != nil {
		return 0, err
	}
	h.pendingCount, h.pendingCounted = count, h.now()
	return count, nil
}

func (h *Handler) binderCookieName() string {
	if h.cfg.SecureCookies {
		return "__Host-elitea_native_authz"
	}
	return "elitea_native_authz"
}

func (h *Handler) setBinder(w http.ResponseWriter, binder string) {
	http.SetCookie(w, &http.Cookie{
		Name: h.binderCookieName(), Value: binder, Path: "/", MaxAge: int(domain.AuthorizationTTL.Seconds()),
		HttpOnly: true, Secure: h.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearBinder(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: h.binderCookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) binder(r *http.Request) string {
	cookie, err := r.Cookie(h.binderCookieName())
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	if h.cfg.Pages == nil {
		writeJSON(w, status, map[string]string{"error": "invalid_request", "error_description": message})
		return
	}
	h.cfg.Pages.RenderError(w, r, status, message)
}

func (h *Handler) record(ctx context.Context, event audit.Event) {
	if h.cfg.Audit == nil {
		return
	}
	if typed, ok := h.cfg.Audit.(*audit.PostgresRecorder); ok && typed == nil {
		return
	}
	event.Timestamp = time.Now().UTC()
	event.EventType = "api"
	event.HTTPMethod = http.MethodPost
	event.EntityType = "native_device"
	h.cfg.Audit.Record(ctx, event)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func int64Ptr(v int64) *int64 { return &v }

func int32Ptr(v int32) *int32 { return &v }
