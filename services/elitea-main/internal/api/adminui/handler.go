package adminui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

type Config struct {
	StaticDir     string // path to admin_ui/static/dist
	ViteServerURL string // e.g. "/api/v2"
	BasePath      string // e.g. "/admin/app"
	SecretKey     string // session cookie HMAC key

	// Resolver reads the operator's REAL administration-mode permissions.
	//
	// A nil Resolver means "no permissions". It never means "all permissions".
	// A mis-wired composition root must degrade closed.
	Resolver auth.PermissionResolver

	// ForwardedIdentityVerifier proves that the X-Auth-* headers on a request
	// were written by the authenticating edge and not by the browser.
	//
	// It is what makes the forwarded-identity source below usable at all. A nil
	// verifier yields no identity from headers — never a trusted one.
	ForwardedIdentityVerifier apimw.ForwardedIdentityPeerVerifier

	// Sessions validates the SERVER-SIDE browser session an `elitea_session`
	// cookie names (shared migration 0117).
	//
	// It is required, not optional, on any deployment that has one. The cookie
	// no longer carries claims: after 0117 its value is an opaque identifier,
	// so the HMAC reader below finds nothing in it and this page falls back to
	// an empty permission list — the exact empty-sidebar defect this file's
	// comment records, reintroduced from the other side. A nil value keeps the
	// pre-0117 behaviour for a deployment that composes no store.
	Sessions BrowserSessions

	// Emails resolves the operator's address for the nav footer. Optional:
	// without it the footer falls back to the generic word "Admin", which is
	// what it already did for every load served through the forwarded-identity
	// path (the headers carry an ID, never an address).
	Emails EmailLookup
}

// BrowserSessions is the one read this page makes against the session store.
// *browsersession.Manager satisfies it.
type BrowserSessions interface {
	Validate(ctx context.Context, cookieValue string) (browsersession.Session, error)
}

// EmailLookup reads one user's address. *pgxpool.Pool does not satisfy it
// directly; the composition root adapts a pool, and a nil value is legal.
type EmailLookup interface {
	UserEmail(ctx context.Context, userID int64) (string, error)
}

type Handler struct {
	cfg       Config
	indexOnce sync.Once
	indexHTML string
}

func NewHandler(cfg Config) *Handler {
	return &Handler{cfg: cfg}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	assetsDir := filepath.Join(h.cfg.StaticDir, "assets")
	r.Handle("/assets/*", http.StripPrefix(h.cfg.BasePath+"/assets/", http.FileServer(http.Dir(assetsDir))))
	r.Get("/*", h.ServeSPA)
	r.Get("/", h.ServeSPA)
	return r
}

type adminUIConfig struct {
	ViteServerURL string   `json:"vite_server_url"`
	ViteBaseURI   string   `json:"vite_base_uri"`
	UserID        any      `json:"user_id"`
	UserName      string   `json:"user_name"`
	UserEmail     string   `json:"user_email"`
	Permissions   []string `json:"permissions"`
	// Roles is always empty. The resolver reports permissions only, and no
	// bundle reads this field. It stays in the payload so an older admin
	// bundle that reads `roles` gets an empty list, never "super_admin".
	Roles []string `json:"roles"`
}

func (h *Handler) ServeSPA(w http.ResponseWriter, r *http.Request) {
	cfg := adminUIConfig{
		ViteServerURL: h.cfg.ViteServerURL,
		ViteBaseURI:   h.cfg.BasePath,
		Permissions:   []string{},
		Roles:         []string{},
	}

	// Read the operator from the request, then resolve the permissions that
	// operator really holds.
	//
	// TWO credential sources, in the order an authenticating edge makes them
	// available. Reading only the second is what emptied the admin sidebar:
	//
	//  1. The identity the edge PROJECTED onto this request as X-Auth-*, proven
	//     to have crossed the header-stripping ingress. The runtime deployment
	//     logs a browser in at /forward-auth/login, which stores an opaque
	//     server-side session under `elitea_browser_auth` — a different cookie,
	//     with a different shape, from a different store. No `elitea_session`
	//     cookie exists on that browser at all, so source 2 found nothing, the
	//     handler injected `permissions: []`, and the SPA hid every nav item it
	//     has: `adminNavItems.ts` shows an item only when the injected list
	//     carries one of its permissions. The operator got a sidebar containing
	//     nothing but their own avatar, and ten implemented pages reachable only
	//     by typing a URL. `user_name` was empty for the same reason, which is
	//     why the footer read the generic fallback "Admin" rather than an
	//     address — one cause, both symptoms.
	//
	//  2. The `elitea_session` HMAC cookie minted by internal/api/v2/auth's OIDC
	//     handler, for a deployment that authenticates through that path.
	//
	// Neither source is authorisation, and the order does not make one weaker:
	// both end at the same resolver, which reloads the user and refuses a
	// suspended one, and every admin route resolves the permissions again.
	//
	// DEFECT this replaces: the handler wrote a fixed list of 37 admin
	// permissions, plus roles ["super_admin"], for EVERY caller whose cookie
	// passed the HMAC and exp check. A rank-and-file user who opened
	// /admin/app therefore saw the whole admin console. Every destructive
	// control stayed visible and enabled. Each click ended in a
	// server-side 403.
	// A suspended user with an unexpired cookie saw the same, because
	// verifySession never reads the database.
	//
	// The resolver closes both halves with one query: the administration mode
	// reads only roles with mode='administration', and the resolver refuses a
	// suspended user.
	if principal, ok := apimw.ForwardedIdentity(r, h.cfg.ForwardedIdentityVerifier); ok {
		// The headers carry an ID and never an address, so the display name
		// comes from the database — after the resolver has confirmed the user is
		// real and active, never before, so a spoofed ID cannot be used to probe
		// for addresses.
		if userID, permissions, resolved := h.resolveForwarded(r.Context(), principal); resolved {
			cfg.UserID = userID
			cfg.Permissions = permissions
			if email := h.lookupEmail(r.Context(), userID); email != "" {
				cfg.UserEmail = email
				cfg.UserName = email
			}
		}
	} else if cookie, err := r.Cookie("elitea_session"); err == nil {
		// SOURCE 2 IS NOW TWO SHAPES, and the prefix decides which. After
		// shared migration 0117 the cookie carries an opaque identifier and no
		// claims at all, so reading only the HMAC form would leave every
		// OIDC-authenticated operator with an empty sidebar — the same defect
		// the comment above records, from the other side.
		if userID, email, ok := h.sessionIdentity(r, cookie.Value); ok {
			if email != "" {
				cfg.UserEmail = email
				cfg.UserName = email
			}
			// The address and the id are set INDEPENDENTLY. A legacy cookie
			// can carry one and not the other — the shape
			// handler_injection_test.go drives — and dropping the address
			// because the id claim is missing would change what that test
			// guards for no reason.
			if userID > 0 {
				cfg.UserID = userID
				cfg.Permissions = h.resolvePermissions(r.Context(), userID)
			}
		}
	}

	cfgJSON, _ := json.Marshal(cfg)

	indexHTML := h.loadIndex()

	// Replace asset paths from relative to absolute
	indexHTML = strings.ReplaceAll(indexHTML, `src="./assets`, fmt.Sprintf(`src="%s/assets`, h.cfg.BasePath))
	indexHTML = strings.ReplaceAll(indexHTML, `href="./assets`, fmt.Sprintf(`href="%s/assets`, h.cfg.BasePath))

	// Inject config.
	//
	// Emitted as a bare JS object literal, NOT as JSON.parse('...'). The quoted
	// form was a script-injection hole (CodeQL go/unsafe-quoting, critical): a
	// single quote anywhere in the payload closes the JS string literal early
	// and everything after it is executed as code. That is reachable, not
	// theoretical — cfg.UserEmail/UserName come from the session JWT's `email`
	// claim, and a single quote is legal in an email local part
	// (o'brien@example.com), so a user whose address contains one injects script
	// into the ADMIN page.
	//
	// Two properties make the bare form safe, and both matter:
	//   1. No surrounding quotes, so there is no string literal to break out of.
	//      JSON has been a syntactic subset of JavaScript since ES2019, so the
	//      value parses as an object literal directly.
	//   2. encoding/json escapes <, > and & to <, > and & by
	//      default, so a payload containing "</script>" cannot terminate the
	//      enclosing tag. Do NOT switch this to a json.Encoder with
	//      SetEscapeHTML(false) — that silently reopens the tag-breakout half.
	inlineConfigScript := fmt.Sprintf("window.admin_ui_config = %s;", string(cfgJSON))
	configScript := "<script>" + inlineConfigScript + "</script>"
	indexHTML = strings.Replace(indexHTML, "<!-- admin_ui_config -->", configScript, 1)

	// This page is per-operator and must never be replayed to another one.
	//
	// It carries the signed-in operator's address and the exact list of
	// administration-mode permissions they hold. Until the forwarded-identity
	// source above existed, the runtime deployment received a byte-identical
	// payload on every load — empty permissions, empty name — so a cache could
	// not leak anything. It can now: with no directive and no validator, a
	// shared proxy or the browser's own disk cache may store this response and
	// serve it after a logout, or to the next person on the machine, who then
	// gets a fully populated admin console belonging to someone else.
	//
	// no-store rather than private/no-cache: there is nothing here worth
	// revalidating, and no-store is what every other identity-bearing response
	// in this service sets (internal/api/v2/auth/session.go's writeSessionJSON
	// and Logout, internal/api/middleware/internal_admin.go).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	// The admin console runs with no Content-Security-Policy at all until this
	// line (issue #177). Only the browserauth login page set one.
	//
	// The threat is measured, not hypothetical: a critical script-injection
	// hole in THIS handler's own config injection (CodeQL go/unsafe-quoting)
	// was fixed a few lines above.
	//
	// nosniff goes with it: this body is assembled from a file on disk plus
	// injected JSON, and a browser that content-sniffs a mislabelled asset
	// undoes part of what the policy buys.
	w.Header().Set("Content-Security-Policy", spaContentSecurityPolicy(inlineConfigScript))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, indexHTML)
}

// spaContentSecurityPolicy builds the policy for one rendered admin page.
//
// # What it does NOT do, stated first
//
// It does not defend the ONE inline script this handler emits against a
// payload injected into that script. The hash is computed over the bytes
// actually served, so an injected payload is inside the hashed block and the
// browser runs it. A nonce has the same property: the nonce sits on the tag
// that carries the payload. The `go/unsafe-quoting` hole above is closed by
// the JSON escaping, and it stays closed by its own test — not by this.
//
// # What it does do
//
//   - `script-src` carries no 'unsafe-inline'. Any OTHER inline script — a
//     separate <script> element reflected into the page, or one written into
//     an index.html on disk — has a different hash and does not run.
//   - `script-src 'self'` denies a foreign script origin, so a bootstrap
//     loader cannot pull the real payload from an attacker host.
//   - `connect-src 'self'` denies the exfiltration leg. Script running in
//     this page reaches the operator's permissions, address and session; with
//     no route off the origin, reading them is worth much less.
//   - `base-uri 'none'` stops an injected <base> from re-pointing every
//     relative asset and API path; `object-src 'none'` removes the plugin
//     surface; `frame-ancestors 'none'` blocks clickjacking of an
//     authenticated admin session; `form-action 'self'` keeps a POST here.
//
// The hash is derived from the response, never restated as a constant, so it
// cannot drift away from the script it authorises — which is the way a
// hash-based policy usually rots into a blank page.
//
// 'unsafe-inline' stays on style-src because Emotion (MUI's styling engine)
// injects <style> elements at runtime; that is a styling channel, not a script
// one. img-src allows data:/blob: because the brand pack's logos are data URIs
// (shared/brand/schema.ts: "data: URI or same-origin path"). Everything else
// falls back to default-src 'self': the bundle, the API and the fonts are all
// same-origin, and `ViteServerURL` is the hardcoded relative "/api/v2".
func spaContentSecurityPolicy(inlineScript string) string {
	digest := sha256.Sum256([]byte(inlineScript))
	return strings.Join([]string{
		"default-src 'self'",
		"base-uri 'none'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
	}, "; ")
}

// resolvePermissions returns the administration-mode permissions of one user.
//
// It returns an empty list on ANY error, a refusal included. The injected list
// is a presentation hint for the admin SPA, so an empty list hides controls.
// It never grants anything: every admin route resolves the permissions again.
func (h *Handler) resolvePermissions(ctx context.Context, userID int64) []string {
	if h.cfg.Resolver == nil {
		return []string{}
	}
	resolution, err := h.cfg.Resolver.ResolvePermissions(
		ctx,
		auth.User{UserID: strconv.FormatInt(userID, 10)},
		auth.PermissionModeAdministration,
		"",
	)
	if err != nil {
		slog.DebugContext(ctx, "admin ui: permission resolution refused or failed",
			"user_id", userID, "error", err)
		return []string{}
	}
	if resolution.Permissions == nil {
		return []string{}
	}
	return resolution.Permissions
}

// resolveForwarded resolves the permissions of a principal an authenticating
// edge projected onto the request.
//
// It hands the WHOLE principal to the resolver rather than a bare numeric ID:
// a forwarded token principal carries a token ID that is not its owner's user
// ID, and the resolver is the component that cross-checks the pair and reports
// the owning user. Taking `X-Auth-ID` as a user ID would read a token's ID as a
// user's on every token-authenticated load.
//
// `resolved` is false whenever the resolver refused — an absent, suspended or
// unmatched principal — and the caller then injects nothing, not a partial
// identity.
func (h *Handler) resolveForwarded(ctx context.Context, principal auth.User) (int64, []string, bool) {
	if h.cfg.Resolver == nil {
		return 0, nil, false
	}
	resolution, err := h.cfg.Resolver.ResolvePermissions(
		ctx, principal, auth.PermissionModeAdministration, "",
	)
	if err != nil {
		// `is_token` is a BOOL derived from the principal, not the principal's
		// own strings. Every field of `principal` is request-header material
		// (X-Auth-Type / X-Auth-ID / X-Auth-User-ID), and copying header
		// material verbatim into a log is how attacker-chosen content reaches
		// the log sink — CodeQL go/clear-text-logging flags exactly that flow,
		// and it is right to: the values reaching here are constrained to a
		// two-element enum only because tryTraefikHeaders already rejected
		// everything else, which is an invariant no reader of this line can
		// see. The bool answers the question the line exists for — which
		// resolution branch refused — and carries none of the input.
		slog.DebugContext(ctx, "admin ui: forwarded permission resolution refused or failed",
			"is_token", principal.TokenID != "", "error", err)
		return 0, nil, false
	}
	if resolution.UserID <= 0 {
		return 0, nil, false
	}
	permissions := resolution.Permissions
	if permissions == nil {
		permissions = []string{}
	}
	return resolution.UserID, permissions, true
}

// lookupEmail returns the operator's address, or "" when there is no lookup
// configured or the read fails. An empty result is not an error path for the
// page: the SPA falls back to the generic word "Admin" for the footer.
func (h *Handler) lookupEmail(ctx context.Context, userID int64) string {
	if h.cfg.Emails == nil {
		return ""
	}
	email, err := h.cfg.Emails.UserEmail(ctx, userID)
	if err != nil {
		// Warn, not Debug. A lookup that was configured and then failed is a
		// mis-wiring or an outage, and the only symptom on screen is a footer
		// reading "Admin" — indistinguishable from a user who genuinely has no
		// address on file. At Debug the operator's report produces nothing in
		// the logs at any level production runs.
		slog.WarnContext(ctx, "admin ui: user email lookup failed", "user_id", userID, "error", err)
		return ""
	}
	return email
}

// sessionClaimUserID reads the `uid` claim of the session cookie. It accepts
// the string and the number form, because encoding/json decodes a JSON number
// as float64. It mirrors the reader in internal/api/v2/auth/session.go.
func sessionClaimUserID(claims map[string]any) (int64, bool) {
	switch value := claims["uid"].(type) {
	case string:
		id, err := strconv.ParseInt(value, 10, 64)
		return id, err == nil && id > 0
	case float64:
		id := int64(value)
		return id, value == float64(id) && id > 0
	default:
		return 0, false
	}
}

// sessionIdentity reads whichever of the two cookie formats arrived.
//
// The server-side form is tried FIRST, and only when this deployment composed
// a store; a value without the `s1.` prefix can only be the legacy signed
// cookie, and a value with it can only be an identifier. See
// browsersession.LooksServerSide for why the two cannot be confused.
//
// A refused session yields no identity and no reason. This page is the admin
// SPA's shell, not an API: it degrades to the empty permission list, which
// hides every control, and the operator is sent to the login start by the
// SPA's own session probe.
func (h *Handler) sessionIdentity(r *http.Request, value string) (int64, string, bool) {
	if h.cfg.Sessions != nil && browsersession.LooksServerSide(value) {
		session, err := h.cfg.Sessions.Validate(r.Context(), value)
		if err != nil {
			return 0, "", false
		}
		return session.UserID, session.Email, true
	}
	if h.cfg.SecretKey == "" {
		return 0, "", false
	}
	claims := h.verifySession(value)
	if claims == nil {
		return 0, "", false
	}
	// The minting code writes the claim as `uid`. The old code read `user_id`,
	// so window.admin_ui_config.user_id was null on every page load. See
	// internal/api/v2/auth/session.go makeSessionToken. A cookie with no
	// usable `uid` still names an address, and the caller uses each half only
	// if it is there.
	userID, _ := sessionClaimUserID(claims)
	email, _ := claims["email"].(string)
	return userID, email, true
}

func (h *Handler) verifySession(token string) map[string]any {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil
	}

	mac := hmac.New(sha256.New, []byte(h.cfg.SecretKey))
	mac.Write([]byte(parts[0]))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expectedSig)) {
		return nil
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil
	}

	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil
		}
	}

	return claims
}

func (h *Handler) loadIndex() string {
	h.indexOnce.Do(func() {
		path := filepath.Join(h.cfg.StaticDir, "index.html")
		data, err := os.ReadFile(path)
		if err != nil {
			h.indexHTML = "<html><body>Admin UI not found</body></html>"
			return
		}
		h.indexHTML = string(data)
	})
	return h.indexHTML
}
